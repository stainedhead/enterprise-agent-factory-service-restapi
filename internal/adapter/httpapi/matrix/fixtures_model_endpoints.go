package matrix

import (
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of API contract v1.5.0 §4.13 (model endpoints, specs/261125-model-endpoint-gateway). Every
// mutation finds a fresh endpoint of its own in Prepare, because the harness repeats a request as every
// caller. Endpoints are written through the owner connection, so the provider switch and slug guards of
// migration 000023 still apply.

const mxARN = "arn:aws:secretsmanager:us-east-1:111122223333:secret:"

// seedEndpoint inserts an endpoint (team "" for a service endpoint) with one model and returns its id,
// slug and version.
func (w *World) seedEndpoint(t *testing.T, team, location, baseURL, auth, pricing string, enabled bool) map[string]string {
	t.Helper()
	id, slug := pgtest.NewID("mep"), "mx"+unique()
	var teamID any
	owner, purpose := "", ""
	if team != "" {
		teamID, owner, purpose = team, "matrix", "matrix tests"
	}
	w.exec(t, `INSERT INTO afs.model_endpoints (id, team_id, slug, display_name, owner, purpose, location, base_url, auth_kind,
  pricing_mode, enabled) VALUES ($1, $2, $3, 'Matrix', $4, $5, $6, $7, $8, $9, $10)`, id, teamID, slug, owner, purpose, location, baseURL, auth, pricing, enabled)
	w.exec(t, `INSERT INTO afs.endpoint_models (endpoint_id, model_name, max_output_tokens) VALUES ($1, 'mx-model', 1000)`, id)
	if pricing == "per_token" {
		w.exec(t, `INSERT INTO afs.endpoint_model_prices (endpoint_id, model_name, effective_from, input_per_mtok_usd, output_per_mtok_usd)
VALUES ($1, 'mx-model', now() - interval '1 day', 1, 2)`, id)
	}
	return map[string]string{"ep": id, "slug": slug, "ver": ver(t, w, `SELECT version FROM afs.model_endpoints WHERE id = $1`, id)}
}

func (w *World) serviceEndpoint(t *testing.T, enabled bool) map[string]string {
	return w.seedEndpoint(t, "", "external", "https://api.openai.com/v1", "bearer", "per_token", enabled)
}

// ownEndpoint is a team A endpoint in its namespace; team A may declare its own endpoints.
func (w *World) ownEndpoint(t *testing.T, auth string, enabled bool) map[string]string {
	w.exec(t, `UPDATE afs.teams SET may_add_model_endpoints = true WHERE id = $1 AND NOT may_add_model_endpoints`, w.TeamA.ID)
	return w.seedEndpoint(t, w.TeamA.ID, "in_namespace", "http://vllm:8000/v1", auth, "free", enabled)
}

func init() {
	svc := func(enabled bool) func(t *testing.T, w *World) map[string]string {
		return func(t *testing.T, w *World) map[string]string { return w.serviceEndpoint(t, enabled) }
	}
	slug := func(*testing.T, *World) map[string]string { return map[string]string{"slug": "mx" + unique()} }
	ifm := map[string]string{"If-Match": ifMatch}

	Register(Case{Op: "ListModelEndpoints", Path: "/v1/admin/model-endpoints?includeTeams=true"})
	Register(Case{Op: "CreateModelEndpoint", Path: "/v1/admin/model-endpoints", Prepare: slug,
		Body: `{"slug":"{slug}","displayName":"Matrix","provider":"other","protocols":["openai_chat"],"location":"external",
"baseUrl":"https://llm.example.com/v1","authKind":"bearer","pricingMode":"per_token","models":[{"modelName":"m1","maxOutputTokens":100}]}`})
	Register(Case{Op: "GetModelEndpoint", Path: "/v1/admin/model-endpoints/{ep}", Prepare: svc(true)})
	Register(Case{Op: "UpdateModelEndpoint", Path: "/v1/admin/model-endpoints/{ep}", Prepare: svc(true), Headers: ifm,
		Body: `{"displayName":"Matrix renamed","timeoutSeconds":120}`})
	Register(Case{Op: "DeleteModelEndpoint", Path: "/v1/admin/model-endpoints/{ep}", Prepare: svc(false), Headers: ifm})
	Register(Case{Op: "PutEndpointModels", Path: "/v1/admin/model-endpoints/{ep}/models", Prepare: svc(true), Headers: ifm,
		Body: `{"models":[{"modelName":"mx-model","maxOutputTokens":2000},{"modelName":"mx-two","maxOutputTokens":100,"enabled":false}]}`})
	Register(Case{Op: "CreateEndpointModelPrice", Path: "/v1/admin/model-endpoints/{ep}/prices", Prepare: svc(true),
		Body: `{"modelName":"mx-model","effectiveFrom":"` + time.Now().UTC().Add(48*time.Hour).Format(time.RFC3339) + `","inputUsdPerMtok":"1.5","outputUsdPerMtok":"6"}`})
	Register(Case{Op: "ListEndpointPriceProposals", Path: "/v1/admin/endpoint-price-proposals?status=pending"})
	proposal := func(t *testing.T, w *World) map[string]string {
		m := w.serviceEndpoint(t, true)
		m["pid"] = pgtest.NewID("epp")
		w.exec(t, `INSERT INTO afs.endpoint_price_proposals (id, endpoint_id, model_name, effective_from, input_per_mtok_usd, output_per_mtok_usd)
VALUES ($1, $2, 'mx-model', now() + interval '1 day', 1.1, 2.2)`, m["pid"], m["ep"])
		return m
	}
	Register(Case{Op: "ApproveEndpointPriceProposal", Path: "/v1/admin/endpoint-price-proposals/{pid}:approve", Prepare: proposal,
		Body: `{"note":"matches the price list"}`})
	Register(Case{Op: "RejectEndpointPriceProposal", Path: "/v1/admin/endpoint-price-proposals/{pid}:reject", Prepare: proposal, Body: `{}`})
	Register(Case{Op: "ListEndpointTeams", Path: "/v1/admin/model-endpoints/{ep}/teams", Prepare: svc(true)})
	Register(Case{Op: "PutModelEndpointCredential", Path: "/v1/admin/model-endpoints/{ep}/credentials/{teamId}", Prepare: svc(true),
		Body: `{"value":"sk-matrix-not-real"}`})
	Register(Case{Op: "RotateModelEndpointCredential", Path: "/v1/admin/model-endpoints/{ep}/credentials/{teamId}:rotate",
		Body: `{"value":"sk-matrix-rotated"}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			m := w.serviceEndpoint(t, true)
			w.exec(t, `INSERT INTO afs.endpoint_credentials (endpoint_id, scope, team_id, secret_arn)
VALUES ($1, $2, $2, $3 || afs.secret_path($2, '_gateway', $4) || '-AbCdEf')`, m["ep"], w.TeamA.ID, mxARN, m["slug"])
			return m
		}})
	Register(Case{Op: "PutEndpointFederation", Path: "/v1/admin/model-endpoints/{ep}/federations/{teamId}",
		Body: `{"tenantId":"11111111-2222-3333-4444-555555555555","clientId":"66666666-7777-8888-9999-000000000000"}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			// Azure is off by default; an endpoint of it is declared while it is on, then it goes off again.
			w.exec(t, `UPDATE afs.inference_providers SET enabled = true WHERE provider = 'azure'`)
			m := w.seedEndpoint(t, "", "external", "https://mx.openai.azure.com/openai/v1", "azure_federated", "per_token", false)
			w.exec(t, `UPDATE afs.inference_providers SET enabled = false WHERE provider = 'azure'`)
			return m
		}})
	Register(Case{Op: "ImportProviderStatement", Path: "/v1/admin/model-endpoints/{ep}/statements",
		Body: `{"period":"2026-10","source":"nous_statement","rows":[{"teamId":"{teamId}","model":"Hermes-4","amountUsd":"12.40","tokens":1000}]}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			return w.seedEndpoint(t, "", "external", "https://inference-api.nousresearch.com/v1", "bearer", "per_token", true)
		}})
	Register(Case{Op: "ListInferenceProviders", Path: "/v1/admin/inference-providers"})
	Register(Case{Op: "UpdateInferenceProvider", Path: "/v1/admin/inference-providers/opencode", Headers: ifm,
		Body: `{"teamSelection":"team_admin"}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			return map[string]string{"ver": ver(t, w, `SELECT version FROM afs.inference_providers WHERE provider = 'opencode'`)}
		}})
	Register(Case{Op: "PutTeamGatewaySettings", Path: "/v1/admin/teams/{teamId}/gateway-settings", Body: `{"deployment":"shared"}`})
	Register(Case{Op: "ListTeamModelEndpoints", Path: "/v1/teams/{teamId}/model-endpoints"})
	// A service admin's selection is removed only by a service admin, so each call starts from no selection.
	Register(Case{Op: "PutTeamModelEndpoints", Path: "/v1/teams/{teamId}/model-endpoint-selections", Body: `{"endpointIds":["{ep}"]}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.exec(t, `DELETE FROM afs.team_model_endpoints WHERE team_id = $1`, w.TeamA.ID)
			return w.serviceEndpoint(t, true)
		}})
	Register(Case{Op: "CreateTeamModelEndpoint", Path: "/v1/teams/{teamId}/model-endpoints",
		Body: `{"slug":"{slug}","displayName":"Matrix vLLM","owner":"ml","purpose":"matrix","protocols":["openai_chat"],"location":"in_namespace",
"baseUrl":"http://vllm:8000/v1","authKind":"none","pricingMode":"free","models":[{"modelName":"llama","maxOutputTokens":100}]}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.exec(t, `UPDATE afs.teams SET may_add_model_endpoints = true WHERE id = $1 AND NOT may_add_model_endpoints`, w.TeamA.ID)
			return slug(t, w)
		}})
	own := func(auth string, enabled bool) func(t *testing.T, w *World) map[string]string {
		return func(t *testing.T, w *World) map[string]string { return w.ownEndpoint(t, auth, enabled) }
	}
	Register(Case{Op: "UpdateTeamModelEndpoint", Path: "/v1/teams/{teamId}/model-endpoints/{ep}", Prepare: own("none", true), Headers: ifm,
		Body: `{"displayName":"Matrix renamed","purpose":"matrix again"}`})
	Register(Case{Op: "DeleteTeamModelEndpoint", Path: "/v1/teams/{teamId}/model-endpoints/{ep}", Prepare: own("none", false), Headers: ifm})
	Register(Case{Op: "PutTeamEndpointModels", Path: "/v1/teams/{teamId}/model-endpoints/{ep}/models", Prepare: own("none", true), Headers: ifm,
		Body: `{"models":[{"modelName":"mx-model","maxOutputTokens":500}]}`})
	Register(Case{Op: "PutTeamModelEndpointCredential", Path: "/v1/teams/{teamId}/model-endpoints/{ep}/credential", Prepare: own("bearer", true),
		Body: `{"value":"sk-matrix-team"}`})
	Register(Case{Op: "GetInferenceReport", Path: "/v1/reports/inference?range=24h&groupBy=team,route"})
	Register(Case{Op: "GetTeamInferenceReport", Path: "/v1/teams/{teamId}/reports/inference?range=24h&groupBy=route,model"})
}

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/modelendpoints"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ModelEndpointsRepo implements modelendpoints.Repository on the 000023 tables as afs_app. Row-level
// security limits every statement to what the caller's session may see and write; the database guards
// (provider switch, slug, credential path, Bedrock fallback) are mapped by MapError.
//
// Route health and the team gateway state come from afs.resource_status, written by the reconciler:
// resource_kind model_endpoint (resource_id the endpoint id), team_route (resource_id
// "<teamId>/<endpointId>") and team_gateway (resource_id the team id).
type ModelEndpointsRepo struct{}

// NewModelEndpointsRepo returns a ModelEndpointsRepo.
func NewModelEndpointsRepo() *ModelEndpointsRepo { return &ModelEndpointsRepo{} }

var _ modelendpoints.Repository = (*ModelEndpointsRepo)(nil)

// Resource kinds of afs.resource_status rows for routes and team gateways.
const (
	StatusKindModelEndpoint = "model_endpoint"
	StatusKindTeamRoute     = "team_route"
	StatusKindTeamGateway   = "team_gateway"
	routeHealthCondition    = "RouteHealth"
)

const mepCols = `e.id, coalesce(e.team_id, ''), e.slug, e.display_name, e.description, e.owner, e.purpose, e.provider, e.protocols,
  e.location, e.base_url, coalesce(e.ca_bundle, ''), e.auth_kind, coalesce(e.auth_header, ''), coalesce(e.metrics_port, 0),
  e.credential_mode, e.pricing_mode, e.requires_terms, e.use_provider_cost, e.enabled, e.timeout_seconds, e.max_body_mib,
  coalesce(e.external_system_id, ''), e.builtin, e.version, e.created_at, e.created_by, e.updated_at,
  afs.provider_enabled(e.provider),
  (SELECT count(*) FROM afs.team_model_endpoints tm WHERE tm.endpoint_id = e.id)`

// healthCols reads the route health condition of the resource_status row joined as rs.
const healthCols = `coalesce(rs.phase, ''), coalesce(hc.c ->> 'reason', ''), (hc.c ->> 'lastTransitionTime')`

func healthJoin(kind, idExpr string) string {
	return ` LEFT JOIN afs.resource_status rs ON rs.resource_kind = '` + kind + `' AND rs.resource_id = ` + idExpr + `
  LEFT JOIN LATERAL (SELECT x AS c FROM jsonb_array_elements(rs.conditions) x WHERE x ->> 'type' = '` + routeHealthCondition + `' LIMIT 1) hc ON true`
}

type scanner interface{ Scan(dest ...any) error }

func scanEndpoint(row scanner, extra ...any) (modelendpoints.Endpoint, error) {
	var e modelendpoints.Endpoint
	var protocols []string
	var loc, auth, cred, pricing, phase, reason string
	var since *string
	var version int64
	var metrics, timeout, maxBody int32
	var teams int64
	dest := []any{&e.ID, &e.TeamID, &e.Slug, &e.DisplayName, &e.Description, &e.Owner, &e.Purpose, &e.Provider, &protocols,
		&loc, &e.BaseURL, &e.CABundle, &auth, &e.AuthHeader, &metrics, &cred, &pricing, &e.RequiresTerms, &e.UseProviderCost,
		&e.Enabled, &timeout, &maxBody, &e.ExternalSystem, &e.Builtin, &version, &e.CreatedAt, &e.CreatedBy, &e.UpdatedAt,
		&e.ProviderOn, &teams, &phase, &reason, &since}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return e, err
	}
	for _, p := range protocols {
		e.Protocols = append(e.Protocols, domain.Protocol(p))
	}
	e.Location, e.AuthKind, e.CredentialMode, e.PricingMode = domain.EndpointLocation(loc), domain.AuthKind(auth), domain.CredentialMode(cred), domain.PricingMode(pricing)
	e.MetricsPort, e.TimeoutSeconds, e.MaxBodyMiB, e.ModelEndpoint.Version, e.TeamsSelected = int(metrics), int(timeout), int(maxBody), int(version), int(teams)
	e.CreatedAt, e.UpdatedAt = e.CreatedAt.UTC(), e.UpdatedAt.UTC()
	e.Health = health(phase, reason, since)
	return e, nil
}

func health(phase, reason string, since *string) modelendpoints.Health {
	h := modelendpoints.Health{State: domain.RouteHealthUnknown, Reason: reason}
	switch phase {
	case "Ready":
		h.State = domain.RouteHealthReady
	case "Degraded":
		h.State = domain.RouteHealthDegraded
	case "Failed":
		h.State = domain.RouteHealthUnavailable
	}
	if since != nil {
		if t, err := time.Parse(time.RFC3339, *since); err == nil {
			t = t.UTC()
			h.Since = &t
		}
	}
	return h
}

// loadModels fills the models of the endpoints, with the price in force and the price state.
func loadModels(ctx context.Context, t *Tx, es []modelendpoints.Endpoint) error {
	if len(es) == 0 {
		return nil
	}
	ids := make([]string, len(es))
	idx := map[string]int{}
	for i, e := range es {
		ids[i], idx[e.ID] = e.ID, i
		es[i].Models = []modelendpoints.Model{}
	}
	rows, err := t.Query(ctx, `SELECT m.endpoint_id, m.model_name, m.max_output_tokens, coalesce(m.context_tokens, 0), m.enabled,
  round(p.input_per_mtok_usd, 6)::text, round(p.output_per_mtok_usd, 6)::text, round(p.cached_input_per_mtok_usd, 6)::text,
  round(p.request_fee_usd, 6)::text,
  EXISTS (SELECT 1 FROM afs.endpoint_price_proposals pp WHERE pp.endpoint_id = m.endpoint_id AND pp.model_name = m.model_name AND pp.state = 'pending')
FROM afs.endpoint_models m
LEFT JOIN LATERAL (SELECT * FROM afs.endpoint_model_prices pr WHERE pr.endpoint_id = m.endpoint_id AND pr.model_name = m.model_name
                    AND pr.effective_from <= now() ORDER BY pr.effective_from DESC LIMIT 1) p ON true
WHERE m.endpoint_id = ANY($1) ORDER BY m.endpoint_id, m.model_name`, ids)
	if err != nil {
		return MapError(err, OpRead, "endpoint models")
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var m modelendpoints.Model
		var maxOut, ctxTok int32
		var in, out, cached, fee *string
		var pending bool
		if err := rows.Scan(&id, &m.ModelName, &maxOut, &ctxTok, &m.Enabled, &in, &out, &cached, &fee, &pending); err != nil {
			return MapError(err, OpRead, "endpoint models")
		}
		m.MaxOutputTokens, m.ContextTokens = int(maxOut), int(ctxTok)
		e := &es[idx[id]]
		switch {
		case e.PricingMode != domain.PricingPerToken:
			m.PriceState = modelendpoints.PriceNotRequired
		case in != nil:
			m.PriceState = modelendpoints.PriceApproved
		case pending:
			m.PriceState = modelendpoints.PricePending
		default:
			m.PriceState = modelendpoints.PriceNone
		}
		if in != nil {
			p, err := price(*in, *out, cached, *fee)
			if err != nil {
				return err
			}
			m.Price = &p
		}
		e.Models = append(e.Models, m)
	}
	return MapError(rows.Err(), OpRead, "endpoint models")
}

func price(in, out string, cached *string, fee string) (modelendpoints.Price, error) {
	var p modelendpoints.Price
	var err error
	if p.Input, err = domain.ParseMoney("inputPerMTokUsd", in); err != nil {
		return p, err
	}
	if p.Output, err = domain.ParseMoney("outputPerMTokUsd", out); err != nil {
		return p, err
	}
	if p.Fee, err = domain.ParseMoney("requestFeeUsd", fee); err != nil {
		return p, err
	}
	if cached != nil {
		c, err := domain.ParseMoney("cachedInputPerMTokUsd", *cached)
		if err != nil {
			return p, err
		}
		p.CachedInput = &c
	}
	return p, nil
}

func priceArgs(p modelendpoints.Price) (string, string, *string, string) {
	var cached *string
	if p.CachedInput != nil {
		s := p.CachedInput.String()
		cached = &s
	}
	return p.Input.String(), p.Output.String(), cached, p.Fee.String()
}

// ListEndpoints implements modelendpoints.Repository.
func (*ModelEndpointsRepo) ListEndpoints(ctx context.Context, tx port.Tx, f modelendpoints.Filter, after string, limit int) ([]modelendpoints.Endpoint, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT `+mepCols+`, `+healthCols+` FROM afs.model_endpoints e`+healthJoin(StatusKindModelEndpoint, "e.id")+`
WHERE e.id > $1 AND ($2 OR e.team_id IS NULL) AND ($3 = '' OR e.provider = $3) AND ($4 = '' OR e.location = $4)
  AND ($5 = '' OR e.pricing_mode = $5)
ORDER BY e.id LIMIT $6`, after, f.IncludeTeams, f.Provider, f.Location, f.PricingMode, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "model endpoints")
	}
	out, err := collectEndpoints(rows)
	if err != nil {
		return nil, err
	}
	return out, loadModels(ctx, t, out)
}

func collectEndpoints(rows pgx.Rows) ([]modelendpoints.Endpoint, error) {
	defer rows.Close()
	out := []modelendpoints.Endpoint{}
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "model endpoints")
		}
		out = append(out, e)
	}
	return out, MapError(rows.Err(), OpRead, "model endpoints")
}

// GetEndpoint implements modelendpoints.Repository.
func (*ModelEndpointsRepo) GetEndpoint(ctx context.Context, tx port.Tx, id string, forUpdate bool) (modelendpoints.Endpoint, error) {
	return getEndpoint(ctx, tx, `e.id = $1`, id, forUpdate)
}

// ServiceEndpointBySlug implements modelendpoints.Repository.
func (*ModelEndpointsRepo) ServiceEndpointBySlug(ctx context.Context, tx port.Tx, slug string) (modelendpoints.Endpoint, error) {
	return getEndpoint(ctx, tx, `e.team_id IS NULL AND e.slug = $1`, slug, false)
}

func getEndpoint(ctx context.Context, tx port.Tx, where, arg string, forUpdate bool) (modelendpoints.Endpoint, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return modelendpoints.Endpoint{}, err
	}
	q := `SELECT ` + mepCols + `, ` + healthCols + ` FROM afs.model_endpoints e` + healthJoin(StatusKindModelEndpoint, "e.id") + ` WHERE ` + where
	if forUpdate {
		q += ` FOR UPDATE OF e`
	}
	e, err := scanEndpoint(t.QueryRow(ctx, q, arg))
	if err != nil {
		return e, MapError(err, OpRead, "model endpoint")
	}
	es := []modelendpoints.Endpoint{e}
	if err := loadModels(ctx, t, es); err != nil {
		return e, err
	}
	return es[0], nil
}

func protocolStrings(ps []domain.Protocol) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}

func endpointArgs(e domain.ModelEndpoint) []any {
	return []any{e.ID, e.TeamID, e.Slug, e.DisplayName, e.Description, e.Owner, e.Purpose, e.Provider, protocolStrings(e.Protocols),
		string(e.Location), e.BaseURL, e.CABundle, string(e.AuthKind), e.AuthHeader, e.MetricsPort, string(e.CredentialMode),
		string(e.PricingMode), e.RequiresTerms, e.UseProviderCost, e.Enabled, e.TimeoutSeconds, e.MaxBodyMiB, e.ExternalSystem}
}

// InsertEndpoint implements modelendpoints.Repository. The database derives the provider from the host.
func (*ModelEndpointsRepo) InsertEndpoint(ctx context.Context, tx port.Tx, e domain.ModelEndpoint) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx, `INSERT INTO afs.model_endpoints (id, team_id, slug, display_name, description, owner, purpose, provider, protocols,
  location, base_url, ca_bundle, auth_kind, auth_header, metrics_port, credential_mode, pricing_mode, requires_terms, use_provider_cost,
  enabled, timeout_seconds, max_body_mib, external_system_id)
VALUES ($1, nullif($2, ''), $3, $4, $5, $6, $7, nullif($8, ''), $9, $10, $11, nullif($12, ''), $13, nullif($14, ''), nullif($15, 0),
  $16, $17, $18, $19, $20, $21, $22, nullif($23, ''))`, endpointArgs(e)...)
	return MapError(err, OpWrite, "model endpoint")
}

// UpdateEndpoint implements modelendpoints.Repository.
func (*ModelEndpointsRepo) UpdateEndpoint(ctx context.Context, tx port.Tx, e domain.ModelEndpoint, expect domain.Version) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := t.Exec(ctx, `UPDATE afs.model_endpoints SET display_name = $4, description = $5, owner = $6, purpose = $7,
  provider = coalesce(nullif($8, ''), provider), protocols = $9, location = $10, base_url = $11, ca_bundle = nullif($12, ''), auth_kind = $13,
  auth_header = nullif($14, ''), metrics_port = nullif($15, 0), credential_mode = $16, pricing_mode = $17, requires_terms = $18,
  use_provider_cost = $19, enabled = $20, timeout_seconds = $21, max_body_mib = $22, external_system_id = nullif($23, '')
WHERE id = $1 AND team_id IS NOT DISTINCT FROM nullif($2, '') AND slug = $3 AND version = $24`, append(endpointArgs(e), int64(expect))...)
	if err != nil {
		return MapError(err, OpWrite, "model endpoint")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	return nil
}

// DeleteEndpoint implements modelendpoints.Repository.
func (*ModelEndpointsRepo) DeleteEndpoint(ctx context.Context, tx port.Tx, id string, expect domain.Version) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := t.Exec(ctx, `DELETE FROM afs.model_endpoints WHERE id = $1 AND version = $2`, id, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "model endpoint")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	return nil
}

// PutModels implements modelendpoints.Repository: models not in ms go (with their prices), the others are
// inserted or updated, and the endpoint's version is bumped.
func (*ModelEndpointsRepo) PutModels(ctx context.Context, tx port.Tx, endpointID string, ms []domain.EndpointModel) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.ModelName
	}
	if _, err := t.Exec(ctx, `DELETE FROM afs.endpoint_models WHERE endpoint_id = $1 AND NOT (model_name = ANY($2))`, endpointID, names); err != nil {
		return MapError(err, OpDelete, "endpoint models")
	}
	for _, m := range ms {
		if _, err := t.Exec(ctx, `INSERT INTO afs.endpoint_models (endpoint_id, model_name, max_output_tokens, context_tokens, enabled)
VALUES ($1, $2, $3, nullif($4, 0), $5)
ON CONFLICT (endpoint_id, model_name) DO UPDATE SET max_output_tokens = EXCLUDED.max_output_tokens,
  context_tokens = EXCLUDED.context_tokens, enabled = EXCLUDED.enabled
WHERE (endpoint_models.max_output_tokens, endpoint_models.context_tokens, endpoint_models.enabled)
  IS DISTINCT FROM (EXCLUDED.max_output_tokens, EXCLUDED.context_tokens, EXCLUDED.enabled)`,
			endpointID, m.ModelName, m.MaxOutputTokens, m.ContextTokens, m.Enabled); err != nil {
			return MapError(err, OpWrite, "endpoint models")
		}
	}
	_, err = t.Exec(ctx, `UPDATE afs.model_endpoints SET updated_at = now() WHERE id = $1`, endpointID)
	return MapError(err, OpWrite, "model endpoint")
}

// InsertPrice implements modelendpoints.Repository.
func (*ModelEndpointsRepo) InsertPrice(ctx context.Context, tx port.Tx, p modelendpoints.PriceRow) (modelendpoints.PriceRow, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return p, err
	}
	in, out, cached, fee := priceArgs(p.Price)
	err = t.QueryRow(ctx, `INSERT INTO afs.endpoint_model_prices (endpoint_id, model_name, effective_from, input_per_mtok_usd,
  output_per_mtok_usd, cached_input_per_mtok_usd, request_fee_usd, source)
VALUES ($1, $2, $3, $4::numeric, $5::numeric, $6::numeric, $7::numeric, $8) RETURNING approved_by, created_at`,
		p.EndpointID, p.ModelName, p.EffectiveFrom, in, out, cached, fee, p.Source).Scan(&p.ApprovedBy, &p.CreatedAt)
	if err != nil {
		var pe interface{ SQLState() string }
		if errors.As(err, &pe) && pe.SQLState() == codeUnique {
			return p, domain.Conflict("duplicate", "a price for this model already starts at effectiveFrom", nil)
		}
		return p, MapError(err, OpWrite, "endpoint price")
	}
	p.CreatedAt = p.CreatedAt.UTC()
	return p, nil
}

const eppCols = `p.id, p.endpoint_id, e.slug, p.model_name, coalesce(p.team_id, ''), p.effective_from, round(p.input_per_mtok_usd, 6)::text,
  round(p.output_per_mtok_usd, 6)::text, round(p.cached_input_per_mtok_usd, 6)::text, round(p.request_fee_usd, 6)::text, p.source, p.state,
  p.fetched_at, p.note, p.decided_at, p.decided_by`

func scanEPP(row scanner) (modelendpoints.Proposal, error) {
	var p modelendpoints.Proposal
	var in, out, fee string
	var cached *string
	if err := row.Scan(&p.ID, &p.EndpointID, &p.Slug, &p.ModelName, &p.TeamID, &p.EffectiveFrom, &in, &out, &cached, &fee, &p.Source,
		&p.State, &p.FetchedAt, &p.Note, &p.DecidedAt, &p.DecidedBy); err != nil {
		return p, err
	}
	p.EffectiveFrom, p.FetchedAt = p.EffectiveFrom.UTC(), p.FetchedAt.UTC()
	if p.DecidedAt != nil {
		d := p.DecidedAt.UTC()
		p.DecidedAt = &d
	}
	pr, err := price(in, out, cached, fee)
	p.Price = pr
	return p, err
}

// ListProposals implements modelendpoints.Repository.
func (*ModelEndpointsRepo) ListProposals(ctx context.Context, tx port.Tx, state, endpointID, after string, limit int) ([]modelendpoints.Proposal, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT `+eppCols+` FROM afs.endpoint_price_proposals p JOIN afs.model_endpoints e ON e.id = p.endpoint_id
WHERE ($1 = '' OR p.state = $1) AND ($2 = '' OR p.endpoint_id = $2) AND ($3 = '' OR p.id < $3) ORDER BY p.id DESC LIMIT $4`,
		state, endpointID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "price proposals")
	}
	defer rows.Close()
	out := []modelendpoints.Proposal{}
	for rows.Next() {
		p, err := scanEPP(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "price proposals")
		}
		out = append(out, p)
	}
	return out, MapError(rows.Err(), OpRead, "price proposals")
}

// GetProposal implements modelendpoints.Repository.
func (*ModelEndpointsRepo) GetProposal(ctx context.Context, tx port.Tx, id string, forUpdate bool) (modelendpoints.Proposal, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return modelendpoints.Proposal{}, err
	}
	q := `SELECT ` + eppCols + ` FROM afs.endpoint_price_proposals p JOIN afs.model_endpoints e ON e.id = p.endpoint_id WHERE p.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF p`
	}
	p, err := scanEPP(t.QueryRow(ctx, q, id))
	return p, MapError(err, OpRead, "price proposal")
}

// DecideProposal implements modelendpoints.Repository. The database sets decided_at and decided_by and,
// on approval, writes the price at greatest(effective_from, now()).
func (r *ModelEndpointsRepo) DecideProposal(ctx context.Context, tx port.Tx, id, state, note string) (modelendpoints.Proposal, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return modelendpoints.Proposal{}, err
	}
	tag, err := t.Exec(ctx, `UPDATE afs.endpoint_price_proposals SET state = $2, note = coalesce(nullif($3, ''), note)
WHERE id = $1 AND state = 'pending'`, id, state, note)
	if err != nil {
		return modelendpoints.Proposal{}, MapError(err, OpWrite, "price proposal")
	}
	if tag.RowsAffected() == 0 {
		return modelendpoints.Proposal{}, domain.Conflict("invalid_state", "proposal is no longer pending", nil)
	}
	return r.GetProposal(ctx, tx, id, false)
}

// ListEndpointTeams implements modelendpoints.Repository.
func (*ModelEndpointsRepo) ListEndpointTeams(ctx context.Context, tx port.Tx, e modelendpoints.Endpoint, after string, limit int) ([]modelendpoints.EndpointTeam, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT t.id, t.slug, t.display_name, tm.selected_by IS NOT NULL, coalesce(tm.selected_by, ''),
  ct.status, ct.rotated_at, cs.status, f.status, t.gateway_deployment,
  (SELECT coalesce(sum(u.cost_usd), 0)::text FROM afs.usage_events u WHERE u.team_id = t.id AND u.endpoint_id = $1
     AND u.occurred_at >= date_trunc('month', now())), `+healthCols+`
FROM afs.teams t
LEFT JOIN afs.team_model_endpoints tm ON tm.team_id = t.id AND tm.endpoint_id = $1
LEFT JOIN afs.endpoint_credentials ct ON ct.endpoint_id = $1 AND ct.scope = t.id
LEFT JOIN afs.endpoint_credentials cs ON cs.endpoint_id = $1 AND cs.scope = '_shared'
LEFT JOIN afs.endpoint_federations f ON f.endpoint_id = $1 AND f.team_id = t.id`+healthJoin(StatusKindTeamRoute, "t.id || '/' || $1")+`
WHERE t.state <> 'deleted' AND t.id > $2 AND (nullif($3, '') IS NULL OR t.id = $3)
ORDER BY t.id LIMIT $4`, e.ID, after, e.TeamID, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "endpoint teams")
	}
	defer rows.Close()
	out := []modelendpoints.EndpointTeam{}
	for rows.Next() {
		var et modelendpoints.EndpointTeam
		var credTeam, credShared, fed *string
		var dep, spend, phase, reason string
		var since *string
		if err := rows.Scan(&et.TeamID, &et.TeamSlug, &et.TeamDisplayName, &et.Selected, &et.SelectedBy, &credTeam, &et.CredentialRotatedAt,
			&credShared, &fed, &dep, &spend, &phase, &reason, &since); err != nil {
			return nil, MapError(err, OpRead, "endpoint teams")
		}
		et.GatewayDeployment = domain.GatewayDeployment(dep)
		et.CredentialState, et.FederationState = credentialState(e.ModelEndpoint, credTeam, credShared), federationState(e.ModelEndpoint, fed)
		if et.SpendMTD, err = domain.ParseMoney("spend", spend); err != nil {
			return nil, err
		}
		et.Health = health(phase, reason, since)
		out = append(out, et)
	}
	return out, MapError(rows.Err(), OpRead, "endpoint teams")
}

func credentialState(e domain.ModelEndpoint, team, shared *string) string {
	if !e.AuthKind.NeedsCredential() {
		return modelendpoints.StateNotRequired
	}
	s := team
	if !e.IsTeam() && e.CredentialMode == domain.CredentialShared {
		s = shared
	}
	if s == nil || *s != "set" {
		return modelendpoints.StateMissing
	}
	return modelendpoints.StateSet
}

func federationState(e domain.ModelEndpoint, status *string) string {
	if !e.AuthKind.Federated() {
		return modelendpoints.StateNotRequired
	}
	if status == nil {
		return modelendpoints.FedPending
	}
	return *status
}

// CredentialPath implements modelendpoints.Repository with the same rule as the database guard.
func (*ModelEndpointsRepo) CredentialPath(ctx context.Context, tx port.Tx, e modelendpoints.Endpoint, scope string) (string, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return "", err
	}
	var path string
	if scope == domain.SharedCredentialScope {
		err = t.QueryRow(ctx, `SELECT coalesce((SELECT s.value #>> '{}' FROM afs.service_settings s WHERE s.key = 'secrets.path_prefix'), 'afs')
  || '/_service/gateway/' || $1`, e.Slug).Scan(&path)
	} else {
		err = t.QueryRow(ctx, `SELECT afs.secret_path($1, '_gateway', $2)`, scope, e.Slug).Scan(&path)
	}
	return path, MapError(err, OpRead, "team")
}

const credCols = `endpoint_id, scope, coalesce(team_id, ''), secret_arn, status, rotated_at, version`

func scanCredential(row scanner) (modelendpoints.Credential, error) {
	var c modelendpoints.Credential
	var v int64
	err := row.Scan(&c.EndpointID, &c.Scope, &c.TeamID, &c.ARN, &c.State, &c.RotatedAt, &v)
	c.Version = domain.Version(v)
	if c.RotatedAt != nil {
		r := c.RotatedAt.UTC()
		c.RotatedAt = &r
	}
	return c, err
}

// GetCredential implements modelendpoints.Repository.
func (*ModelEndpointsRepo) GetCredential(ctx context.Context, tx port.Tx, endpointID, scope string) (modelendpoints.Credential, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return modelendpoints.Credential{}, err
	}
	c, err := scanCredential(t.QueryRow(ctx, `SELECT `+credCols+` FROM afs.endpoint_credentials WHERE endpoint_id = $1 AND scope = $2`, endpointID, scope))
	return c, MapError(err, OpRead, "endpoint credential")
}

// PutCredential implements modelendpoints.Repository.
func (*ModelEndpointsRepo) PutCredential(ctx context.Context, tx port.Tx, c modelendpoints.Credential) (modelendpoints.Credential, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return c, err
	}
	out, err := scanCredential(t.QueryRow(ctx, `INSERT INTO afs.endpoint_credentials (endpoint_id, scope, team_id, secret_arn, status, rotated_at)
VALUES ($1, $2, nullif($3, ''), $4, 'set', now())
ON CONFLICT (endpoint_id, scope) DO UPDATE SET secret_arn = EXCLUDED.secret_arn, status = 'set', rotated_at = now()
RETURNING `+credCols, c.EndpointID, c.Scope, c.TeamID, c.ARN))
	return out, MapError(err, OpWrite, "endpoint credential")
}

const fedCols = `endpoint_id, team_id, kind, coalesce(tenant_id, ''), coalesce(client_id, ''), coalesce(project_number, ''), coalesce(pool_id, ''),
  coalesce(provider_id, ''), coalesce(project_id, ''), coalesce(location, ''), status, coalesce(last_error, ''), checked_at, version`

func scanFederation(row scanner) (modelendpoints.Federation, error) {
	var f modelendpoints.Federation
	var v int64
	err := row.Scan(&f.EndpointID, &f.TeamID, &f.Kind, &f.TenantID, &f.ClientID, &f.ProjectNumber, &f.PoolID, &f.ProviderID, &f.ProjectID,
		&f.Location, &f.Status, &f.LastError, &f.CheckedAt, &v)
	f.Version = domain.Version(v)
	return f, err
}

// GetFederation implements modelendpoints.Repository.
func (*ModelEndpointsRepo) GetFederation(ctx context.Context, tx port.Tx, endpointID, teamID string) (modelendpoints.Federation, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return modelendpoints.Federation{}, err
	}
	f, err := scanFederation(t.QueryRow(ctx, `SELECT `+fedCols+` FROM afs.endpoint_federations WHERE endpoint_id = $1 AND team_id = $2`, endpointID, teamID))
	return f, MapError(err, OpRead, "endpoint federation")
}

// PutFederation implements modelendpoints.Repository: new identifiers put the status back to pending.
func (*ModelEndpointsRepo) PutFederation(ctx context.Context, tx port.Tx, f modelendpoints.Federation) (modelendpoints.Federation, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return f, err
	}
	out, err := scanFederation(t.QueryRow(ctx, `INSERT INTO afs.endpoint_federations (endpoint_id, team_id, kind, tenant_id, client_id,
  project_number, pool_id, provider_id, project_id, location)
VALUES ($1, $2, $3, nullif($4, ''), nullif($5, ''), nullif($6, ''), nullif($7, ''), nullif($8, ''), nullif($9, ''), nullif($10, ''))
ON CONFLICT (endpoint_id, team_id) DO UPDATE SET kind = EXCLUDED.kind, tenant_id = EXCLUDED.tenant_id, client_id = EXCLUDED.client_id,
  project_number = EXCLUDED.project_number, pool_id = EXCLUDED.pool_id, provider_id = EXCLUDED.provider_id,
  project_id = EXCLUDED.project_id, location = EXCLUDED.location, status = 'pending', last_error = NULL, checked_at = NULL
RETURNING `+fedCols, f.EndpointID, f.TeamID, f.Kind, f.TenantID, f.ClientID, f.ProjectNumber, f.PoolID, f.ProviderID, f.ProjectID, f.Location))
	return out, MapError(err, OpWrite, "endpoint federation")
}

type statementRow struct {
	TeamID    string      `json:"teamId"`
	Model     string      `json:"model"`
	AmountUSD json.Number `json:"amountUsd"`
	Tokens    *int64      `json:"tokens,omitempty"`
}

// InsertStatement implements modelendpoints.Repository.
func (*ModelEndpointsRepo) InsertStatement(ctx context.Context, tx port.Tx, s modelendpoints.Statement) (modelendpoints.Statement, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return s, err
	}
	rows := make([]statementRow, len(s.Rows))
	for i, r := range s.Rows {
		rows[i] = statementRow{TeamID: r.TeamID, Model: r.Model, AmountUSD: json.Number(r.AmountUSD.String()), Tokens: r.Tokens}
	}
	raw, err := json.Marshal(rows)
	if err != nil {
		return s, fmt.Errorf("encode statement rows: %w", err)
	}
	err = t.QueryRow(ctx, `INSERT INTO afs.provider_statements (id, endpoint_id, period, source, rows) VALUES ($1, $2, $3::date, $4, $5::jsonb)
RETURNING imported_by, imported_at`, s.ID, s.EndpointID, s.Period.Format(time.DateOnly), s.Source, string(raw)).Scan(&s.ImportedBy, &s.ImportedAt)
	if err != nil {
		var pe interface{ SQLState() string }
		if errors.As(err, &pe) && pe.SQLState() == codeUnique {
			return s, domain.Conflict("duplicate", "a statement for this endpoint, period and source is already imported", nil)
		}
		return s, MapError(err, OpWrite, "provider statement")
	}
	s.ImportedAt = s.ImportedAt.UTC()
	return s, nil
}

const providerCols = `p.provider, p.display_name, p.hosts, p.enabled, p.team_selection, coalesce(p.disabled_reason, ''),
  coalesce(p.fallback_model, ''), p.version, p.changed_by, p.changed_at,
  (SELECT count(*) FROM afs.model_endpoints e WHERE e.provider = p.provider),
  (SELECT count(DISTINCT x.team_id) FROM (
     SELECT tm.team_id FROM afs.team_model_endpoints tm JOIN afs.model_endpoints e ON e.id = tm.endpoint_id WHERE e.provider = p.provider
     UNION ALL SELECT e.team_id FROM afs.model_endpoints e WHERE e.provider = p.provider AND e.team_id IS NOT NULL) x),
  (SELECT count(*) FROM afs.external_systems s WHERE EXISTS (SELECT 1 FROM unnest(s.endpoints) h
     WHERE p.provider <> 'other' AND afs.provider_for_host(split_part(h, ':', 1)) = p.provider))`

func scanProvider(row scanner) (modelendpoints.Provider, error) {
	var p modelendpoints.Provider
	var sel string
	var v, routes, teams, direct int64
	err := row.Scan(&p.Provider, &p.DisplayName, &p.Hosts, &p.Enabled, &sel, &p.DisabledReason, &p.FallbackModel, &v, &p.ChangedBy, &p.ChangedAt,
		&routes, &teams, &direct)
	p.TeamSelection, p.Version, p.ChangedAt = domain.TeamSelection(sel), int(v), p.ChangedAt.UTC()
	p.Routes, p.Teams, p.DirectSystems = int(routes), int(teams), int(direct)
	return p, err
}

// ListProviders implements modelendpoints.Repository.
func (*ModelEndpointsRepo) ListProviders(ctx context.Context, tx port.Tx) ([]modelendpoints.Provider, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT `+providerCols+` FROM afs.inference_providers p ORDER BY p.provider`)
	if err != nil {
		return nil, MapError(err, OpRead, "inference providers")
	}
	defer rows.Close()
	out := []modelendpoints.Provider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "inference providers")
		}
		out = append(out, p)
	}
	return out, MapError(rows.Err(), OpRead, "inference providers")
}

// GetProvider implements modelendpoints.Repository.
func (*ModelEndpointsRepo) GetProvider(ctx context.Context, tx port.Tx, provider string, forUpdate bool) (modelendpoints.Provider, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return modelendpoints.Provider{}, err
	}
	q := `SELECT ` + providerCols + ` FROM afs.inference_providers p WHERE p.provider = $1`
	if forUpdate {
		q += ` FOR UPDATE OF p`
	}
	p, err := scanProvider(t.QueryRow(ctx, q, provider))
	return p, MapError(err, OpRead, "inference provider")
}

// UpdateProvider implements modelendpoints.Repository. The database bumps the version and applies the
// Bedrock fallback guard (no_fallback_model).
func (*ModelEndpointsRepo) UpdateProvider(ctx context.Context, tx port.Tx, p domain.InferenceProvider, expect domain.Version) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := t.Exec(ctx, `UPDATE afs.inference_providers SET enabled = $2, team_selection = $3, disabled_reason = nullif($4, ''),
  fallback_model = nullif($5, '') WHERE provider = $1 AND version = $6`,
		p.Provider, p.Enabled, string(p.TeamSelection), p.DisabledReason, p.FallbackModel, int64(expect))
	if err != nil {
		return MapError(err, OpWrite, "inference provider")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	return nil
}

// ProviderImpact implements modelendpoints.Repository. For Bedrock, agents and pools whose default model is
// a Bedrock model move to the fallback; the others lose the provider's routes.
func (*ModelEndpointsRepo) ProviderImpact(ctx context.Context, tx port.Tx, provider, fallback string) (modelendpoints.Impact, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return modelendpoints.Impact{}, err
	}
	im := modelendpoints.Impact{FallbackModel: fallback}
	var routes, teams, agents, moving, degraded, direct, toSelect int64
	err = t.QueryRow(ctx, `WITH eps AS (SELECT e.id, e.team_id FROM afs.model_endpoints e WHERE e.provider = $1 AND e.enabled),
subj AS (
  SELECT a.team_id, a.default_route FROM afs.agents a JOIN afs.teams t ON t.id = a.team_id
   WHERE a.deleted_at IS NULL AND a.state <> 'deleted' AND t.state <> 'deleted'
  UNION ALL
  SELECT p.team_id, p.default_route FROM afs.agent_pools p JOIN afs.teams t ON t.id = p.team_id
   WHERE p.deleted_at IS NULL AND p.state <> 'deleted' AND t.state <> 'deleted'),
affected AS (
  SELECT s.* FROM subj s WHERE $1 = 'bedrock'
     OR EXISTS (SELECT 1 FROM afs.effective_agent_routes r JOIN eps ON eps.id = r.endpoint_id WHERE r.team_id = s.team_id))
SELECT (SELECT count(*) FROM eps),
  (SELECT count(DISTINCT x.team_id) FROM (SELECT tm.team_id FROM afs.team_model_endpoints tm JOIN eps ON eps.id = tm.endpoint_id
     UNION ALL SELECT eps.team_id FROM eps WHERE eps.team_id IS NOT NULL) x),
  (SELECT count(*) FROM affected),
  (SELECT count(*) FROM affected WHERE $1 = 'bedrock' AND default_route IS NULL),
  (SELECT count(*) FROM affected WHERE $1 <> 'bedrock' AND default_route IN (SELECT e.slug FROM afs.model_endpoints e WHERE e.provider = $1)),
  (SELECT count(*) FROM afs.external_systems s WHERE EXISTS (SELECT 1 FROM unnest(s.endpoints) h
     WHERE $1 <> 'other' AND afs.provider_for_host(split_part(h, ':', 1)) = $1)),
  (SELECT count(*) FROM afs.teams t WHERE t.state <> 'deleted' AND $2 <> '' AND NOT EXISTS (
     SELECT 1 FROM afs.team_model_endpoints tm JOIN afs.model_endpoints e ON e.id = tm.endpoint_id
      WHERE tm.team_id = t.id AND e.team_id IS NULL AND e.slug = split_part($2, '/', 1)))`, provider, fallback).
		Scan(&routes, &teams, &agents, &moving, &degraded, &direct, &toSelect)
	if err != nil {
		return im, MapError(err, OpRead, "provider impact")
	}
	im.Routes, im.Teams, im.Agents, im.AgentsMovingToFallback = int(routes), int(teams), int(agents), int(moving)
	im.AgentsDegraded, im.DirectSystems, im.TeamsToSelect = int(degraded), int(direct), int(toSelect)
	return im, nil
}

// SelectForAllTeams implements modelendpoints.Repository.
func (*ModelEndpointsRepo) SelectForAllTeams(ctx context.Context, tx port.Tx, endpointID string) (int, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return 0, err
	}
	tag, err := t.Exec(ctx, `INSERT INTO afs.team_model_endpoints (team_id, endpoint_id)
SELECT t.id, $1 FROM afs.teams t WHERE t.state <> 'deleted' ON CONFLICT DO NOTHING`, endpointID)
	if err != nil {
		return 0, MapError(err, OpWrite, "team model endpoints")
	}
	return int(tag.RowsAffected()), nil
}

// SetServiceDefaultModel implements modelendpoints.Repository.
func (*ModelEndpointsRepo) SetServiceDefaultModel(ctx context.Context, tx port.Tx, value string) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx, `INSERT INTO afs.service_settings (key, value) VALUES ('models.default_model_id', to_jsonb($1::text))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, value)
	return MapError(err, OpWrite, "service setting")
}

// TeamGateway implements modelendpoints.Repository.
func (*ModelEndpointsRepo) TeamGateway(ctx context.Context, tx port.Tx, teamID string, forUpdate bool) (modelendpoints.TeamGateway, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return modelendpoints.TeamGateway{}, err
	}
	q := `SELECT t.id, t.namespace, t.state, t.gateway_deployment, t.may_add_model_endpoints, t.version, coalesce(rs.phase, '')
FROM afs.teams t LEFT JOIN afs.resource_status rs ON rs.resource_kind = '` + StatusKindTeamGateway + `' AND rs.resource_id = t.id
WHERE t.id = $1 AND t.state <> 'deleted'`
	if forUpdate {
		q += ` FOR UPDATE OF t`
	}
	var g modelendpoints.TeamGateway
	var dep string
	var v int64
	err = t.QueryRow(ctx, q, teamID).Scan(&g.TeamID, &g.Namespace, &g.State, &dep, &g.MayAdd, &v, &g.GatewayPhase)
	g.Deployment, g.Version = domain.GatewayDeployment(dep), domain.Version(v)
	return g, MapError(err, OpRead, "team")
}

// PutTeamGateway implements modelendpoints.Repository. Changing may_add_model_endpoints needs a service
// admin (the team guardrail guard answers 42501 otherwise).
func (*ModelEndpointsRepo) PutTeamGateway(ctx context.Context, tx port.Tx, teamID string, d domain.GatewayDeployment, mayAdd bool, expect domain.Version) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := t.Exec(ctx, `UPDATE afs.teams SET gateway_deployment = $2, may_add_model_endpoints = $3
WHERE id = $1 AND version = $4 AND (gateway_deployment, may_add_model_endpoints) IS DISTINCT FROM ($2, $3)`, teamID, string(d), mayAdd, int64(expect))
	if err != nil {
		return MapError(err, OpWrite, "team")
	}
	if tag.RowsAffected() == 0 {
		var same bool
		if err := t.QueryRow(ctx, `SELECT version = $2 FROM afs.teams WHERE id = $1`, teamID, int64(expect)).Scan(&same); err != nil {
			return MapError(err, OpRead, "team")
		}
		if !same {
			return domain.PreconditionFailed()
		}
	}
	return nil
}

// teamEndpointQuery lists the endpoints one team sees: its own and every service endpoint, with the
// team's opt-in, credential and federation state and the per-team route health.
const relationExpr = `CASE WHEN e.team_id IS NOT NULL THEN 'own' WHEN tm.endpoint_id IS NOT NULL THEN 'selected' ELSE 'available' END`

const teamEndpointQuery = `SELECT ` + mepCols + `, ` + healthCols + `, ` + relationExpr + `,
  coalesce(tm.selected_by, ''),
  CASE WHEN e.requires_terms THEN 'service_admin' ELSE coalesce(ip.team_selection, 'team_admin') END,
  ct.status, cs.status, f.status
FROM afs.model_endpoints e
LEFT JOIN afs.team_model_endpoints tm ON tm.team_id = $1 AND tm.endpoint_id = e.id
LEFT JOIN afs.inference_providers ip ON ip.provider = e.provider
LEFT JOIN afs.endpoint_credentials ct ON ct.endpoint_id = e.id AND ct.scope = $1
LEFT JOIN afs.endpoint_credentials cs ON cs.endpoint_id = e.id AND cs.scope = '_shared'
LEFT JOIN afs.endpoint_federations f ON f.endpoint_id = e.id AND f.team_id = $1` + `
LEFT JOIN afs.resource_status rs ON rs.resource_kind = '` + StatusKindTeamRoute + `' AND rs.resource_id = $1 || '/' || e.id
LEFT JOIN LATERAL (SELECT x AS c FROM jsonb_array_elements(rs.conditions) x WHERE x ->> 'type' = '` + routeHealthCondition + `' LIMIT 1) hc ON true
WHERE (e.team_id IS NULL OR e.team_id = $1) AND EXISTS (SELECT 1 FROM afs.teams t WHERE t.id = $1 AND t.state <> 'deleted')`

func scanTeamEndpoint(row scanner) (modelendpoints.TeamEndpoint, error) {
	var te modelendpoints.TeamEndpoint
	var sel string
	var credTeam, credShared, fed *string
	e, err := scanEndpoint(row, &te.Relation, &te.SelectedBy, &sel, &credTeam, &credShared, &fed)
	if err != nil {
		return te, err
	}
	te.Endpoint, te.SelectableBy = e, domain.TeamSelection(sel)
	te.CredentialState, te.FederationState = credentialState(e.ModelEndpoint, credTeam, credShared), federationState(e.ModelEndpoint, fed)
	return te, nil
}

// ListTeamEndpoints implements modelendpoints.Repository.
func (*ModelEndpointsRepo) ListTeamEndpoints(ctx context.Context, tx port.Tx, teamID, relation, after string, limit int) ([]modelendpoints.TeamEndpoint, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, teamEndpointQuery+` AND e.id > $2 AND ($3 = '' OR `+relationExpr+` = $3) ORDER BY e.id LIMIT $4`,
		teamID, after, relation, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "model endpoints")
	}
	defer rows.Close()
	out := []modelendpoints.TeamEndpoint{}
	for rows.Next() {
		te, err := scanTeamEndpoint(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "model endpoints")
		}
		out = append(out, te)
	}
	if err := rows.Err(); err != nil {
		return nil, MapError(err, OpRead, "model endpoints")
	}
	return out, loadTeamModels(ctx, t, out)
}

func loadTeamModels(ctx context.Context, t *Tx, tes []modelendpoints.TeamEndpoint) error {
	es := make([]modelendpoints.Endpoint, len(tes))
	for i, te := range tes {
		es[i] = te.Endpoint
	}
	if err := loadModels(ctx, t, es); err != nil {
		return err
	}
	for i := range tes {
		tes[i].Endpoint = es[i]
	}
	return nil
}

// TeamEndpoint implements modelendpoints.Repository.
func (*ModelEndpointsRepo) TeamEndpoint(ctx context.Context, tx port.Tx, teamID, endpointID string) (modelendpoints.TeamEndpoint, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return modelendpoints.TeamEndpoint{}, err
	}
	te, err := scanTeamEndpoint(t.QueryRow(ctx, teamEndpointQuery+` AND e.id = $2`, teamID, endpointID))
	if err != nil {
		return te, MapError(err, OpRead, "model endpoint")
	}
	tes := []modelendpoints.TeamEndpoint{te}
	return tes[0], loadTeamModels(ctx, t, tes)
}

// TeamSelections implements modelendpoints.Repository.
func (*ModelEndpointsRepo) TeamSelections(ctx context.Context, tx port.Tx, teamID string) ([]modelendpoints.Selection, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT tm.endpoint_id, e.slug, tm.selected_by FROM afs.team_model_endpoints tm
JOIN afs.model_endpoints e ON e.id = tm.endpoint_id WHERE tm.team_id = $1 ORDER BY tm.endpoint_id`, teamID)
	if err != nil {
		return nil, MapError(err, OpRead, "team model endpoints")
	}
	defer rows.Close()
	out := []modelendpoints.Selection{}
	for rows.Next() {
		var s modelendpoints.Selection
		if err := rows.Scan(&s.EndpointID, &s.Slug, &s.SelectedBy); err != nil {
			return nil, MapError(err, OpRead, "team model endpoints")
		}
		out = append(out, s)
	}
	return out, MapError(rows.Err(), OpRead, "team model endpoints")
}

// Select implements modelendpoints.Repository. selected_by is set by the database from the caller.
func (*ModelEndpointsRepo) Select(ctx context.Context, tx port.Tx, teamID, endpointID string) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx, `INSERT INTO afs.team_model_endpoints (team_id, endpoint_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, teamID, endpointID)
	return MapError(err, OpWrite, "team model endpoint")
}

// Deselect implements modelendpoints.Repository.
func (*ModelEndpointsRepo) Deselect(ctx context.Context, tx port.Tx, teamID, endpointID string) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx, `DELETE FROM afs.team_model_endpoints WHERE team_id = $1 AND endpoint_id = $2`, teamID, endpointID)
	return MapError(err, OpDelete, "team model endpoint")
}

// reportDims maps a groupBy dimension to its expression over usage_events u and model_endpoints e.
var reportDims = map[string]string{
	"team":  `u.team_id`,
	"route": `coalesce(e.slug, 'bedrock')`,
	"model": `coalesce(u.model_name, u.model_id, '')`,
	"agent": `coalesce(u.agent_id, '')`,
	"pool":  `coalesce(u.pool_id, '')`,
	"day":   `to_char(date_trunc('day', u.occurred_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD')`,
	"hour":  `to_char(date_trunc('hour', u.occurred_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD"T"HH24:00:00"Z"')`,
}

const reportMeasures = `count(*), count(*) FILTER (WHERE u.outcome = 'error'), count(*) FILTER (WHERE u.http_status = 429),
  count(*) FILTER (WHERE u.outcome LIKE 'rejected%'),
  round(percentile_cont(0.5) WITHIN GROUP (ORDER BY u.latency_ms))::bigint,
  round(percentile_cont(0.95) WITHIN GROUP (ORDER BY u.latency_ms))::bigint,
  round(percentile_cont(0.99) WITHIN GROUP (ORDER BY u.latency_ms))::bigint,
  round(percentile_cont(0.5) WITHIN GROUP (ORDER BY u.ttft_ms))::bigint,
  round(percentile_cont(0.95) WITHIN GROUP (ORDER BY u.ttft_ms))::bigint,
  round(percentile_cont(0.99) WITHIN GROUP (ORDER BY u.ttft_ms))::bigint,
  round(percentile_cont(0.99) WITHIN GROUP (ORDER BY u.gateway_ms))::bigint,
  coalesce(sum(u.input_tokens), 0), coalesce(sum(u.output_tokens), 0), coalesce(sum(u.cache_read_tokens), 0),
  coalesce(sum(u.reasoning_tokens), 0)::bigint,
  round(sum(u.output_tokens) * 1000.0 / nullif(sum(coalesce(u.upstream_ms, u.latency_ms)), 0), 2)::float8,
  count(*) FILTER (WHERE u.usage_source = 'estimated'), round(coalesce(sum(u.cost_usd), 0), 6)::text,
  CASE WHEN count(DISTINCT coalesce(e.pricing_mode, 'per_token')) = 1 THEN min(coalesce(e.pricing_mode, 'per_token')) ELSE '' END`

// InferenceReport implements modelendpoints.Repository over afs.usage_events (row-level security limits
// it to the teams the caller sees).
func (*ModelEndpointsRepo) InferenceReport(ctx context.Context, tx port.Tx, q modelendpoints.ReportQuery) (modelendpoints.Report, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return modelendpoints.Report{}, err
	}
	from := ` FROM afs.usage_events u LEFT JOIN afs.model_endpoints e ON e.id = u.endpoint_id
WHERE u.occurred_at >= $1 AND u.occurred_at < $2 AND ($3 = '' OR u.team_id = $3)
  AND ($4 = '' OR coalesce(e.slug, 'bedrock') = $4) AND ($5 = '' OR coalesce(u.model_name, u.model_id) = $5)
  AND ($6 = '' OR u.agent_id = $6) AND ($7 = '' OR u.pool_id = $7)`
	args := []any{q.From, q.To, q.TeamID, q.Route, q.ModelID, q.AgentID, q.PoolID}
	dims := make([]string, 0, len(q.GroupBy))
	for _, g := range q.GroupBy {
		d, ok := reportDims[g]
		if !ok {
			return modelendpoints.Report{}, fmt.Errorf("inference report: unknown dimension %q", g)
		}
		dims = append(dims, d)
	}
	var out modelendpoints.Report
	sel := `SELECT ` + strings.Join(append(dims, reportMeasures), ", ") + from
	if len(dims) > 0 {
		pos := make([]string, len(dims))
		for i := range dims {
			pos[i] = strconv.Itoa(i + 1)
		}
		sel += ` GROUP BY ` + strings.Join(pos, ", ") + ` ORDER BY ` + strings.Join(pos, ", ")
	}
	sel += fmt.Sprintf(` OFFSET %d LIMIT %d`, q.Offset, q.Limit)
	rows, err := t.Query(ctx, sel, args...)
	if err != nil {
		return out, MapError(err, OpRead, "inference report")
	}
	defer rows.Close()
	out.Rows = []modelendpoints.ReportRow{}
	for rows.Next() {
		r, err := scanReportRow(rows, q.GroupBy)
		if err != nil {
			return out, err
		}
		out.Rows = append(out.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return out, MapError(err, OpRead, "inference report")
	}
	rows.Close()
	tot, err := scanReportRow(t.QueryRow(ctx, `SELECT `+reportMeasures+from, args...), nil)
	if err != nil {
		return out, err
	}
	out.Totals = tot
	return out, nil
}

func scanReportRow(row scanner, groupBy []string) (modelendpoints.ReportRow, error) {
	var r modelendpoints.ReportRow
	keys := make([]string, len(groupBy))
	dest := make([]any, 0, len(groupBy)+20)
	for i := range keys {
		dest = append(dest, &keys[i])
	}
	var cost string
	dest = append(dest, &r.Calls, &r.Errors, &r.Throttled, &r.Refused, &r.LatencyP50, &r.LatencyP95, &r.LatencyP99,
		&r.TTFTP50, &r.TTFTP95, &r.TTFTP99, &r.GatewayP99, &r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.ReasoningTokens,
		&r.OutputTokensPerSecond, &r.Estimated, &cost, &r.PricingMode)
	if err := row.Scan(dest...); err != nil {
		return r, MapError(err, OpRead, "inference report")
	}
	for i, g := range groupBy {
		switch g {
		case "team":
			r.TeamID = keys[i]
		case "route":
			r.Route = keys[i]
		case "model":
			r.Model = keys[i]
		case "agent":
			r.AgentID = keys[i]
		case "pool":
			r.PoolID = keys[i]
		case "day":
			r.Day = keys[i]
		case "hour":
			if h, err := time.Parse(time.RFC3339, keys[i]); err == nil {
				r.Hour = &h
			}
		}
	}
	var err error
	r.CostUSD, err = domain.ParseMoney("costUsd", cost)
	return r, err
}

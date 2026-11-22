package matrix

import (
	"context"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M2-B (agents).

const (
	mxDigest  = "sha256:" + "4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f"
	mxImage   = "registry.example.com/agents/coder@" + mxDigest
	noEventSn = "CD-7: a manual snapshot request has no event type in the API contract"
)

// SeedAgentCatalog makes a model enabled for team A and an enabled template that allows mxDigest.
// It returns the model id and the template name.
func (w *World) SeedAgentCatalog(t *testing.T) (model, tpl string) {
	t.Helper()
	model, tpl = "mx.model-"+unique(), "mx-"+unique()
	w.exec(t, `INSERT INTO afs.models (id, provider, display_name, max_output_tokens) VALUES ($1, 'anthropic', 'Matrix', 4096)`, model)
	w.exec(t, `INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, $2)`, w.TeamA.ID, model)
	w.exec(t, `INSERT INTO afs.agent_templates (id, name, harness, allowed_image_digests, current_image_digest)
VALUES ($1, $2, 'hermes', ARRAY[$3]::text[], $3)`, pgtest.NewID("tpl"), tpl, mxDigest)
	// The shared world must stay as other cases expect it (PutTeamModels empties the model list), so
	// the agents, snapshots, model and template made for this call go away with its subtest.
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		for _, q := range []string{
			`DELETE FROM afs.agent_volume_snapshots WHERE agent_id IN (SELECT id FROM afs.agents WHERE model_id = $1)`,
			`DELETE FROM afs.agents WHERE model_id = $1`,
			`DELETE FROM afs.team_models WHERE model_id = $1`,
			`DELETE FROM afs.models WHERE id = $1`,
		} {
			if _, err := w.Admin.Exec(ctx, q, model); err != nil {
				t.Errorf("matrix cleanup: %v", err)
			}
		}
		if _, err := w.Admin.Exec(ctx, `DELETE FROM afs.agent_templates WHERE name = $1`, tpl); err != nil {
			t.Errorf("matrix cleanup: %v", err)
		}
	})
	return model, tpl
}

// seedAgent inserts an agent of team A and returns the placeholders a case needs.
func (w *World) seedAgent(t *testing.T) map[string]string {
	t.Helper()
	model, tpl := w.SeedAgentCatalog(t)
	id := pgtest.NewID("agt")
	w.exec(t, `INSERT INTO afs.agents (id, team_id, name, template, image, model_id) VALUES ($1, $2, $3, $4, $5, $6)`,
		id, w.TeamA.ID, "mx-"+unique(), tpl, mxImage, model)
	return map[string]string{"agentId": id, "model": model, "tpl": tpl,
		"ver": ver(t, w, `SELECT version FROM afs.agents WHERE id = $1`, id)}
}

func agentPrep(t *testing.T, w *World) map[string]string { return w.seedAgent(t) }

func init() {
	ifm := map[string]string{"If-Match": ifMatch}
	Register(Case{Op: "ListAgents", Path: "/v1/teams/{teamId}/agents", Prepare: agentPrep})
	Register(Case{Op: "GetAgent", Path: "/v1/teams/{teamId}/agents/{agentId}", Prepare: agentPrep})
	Register(Case{Op: "GetAgentStatus", Path: "/v1/teams/{teamId}/agents/{agentId}/status", Prepare: agentPrep})
	Register(Case{Op: "CreateAgent", Path: "/v1/teams/{teamId}/agents",
		Body: `{"name":"mx-{n}","template":"{tpl}","image":"` + mxImage + `","modelId":"{model}"}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			model, tpl := w.SeedAgentCatalog(t)
			return map[string]string{"n": unique(), "model": model, "tpl": tpl}
		}})
	Register(Case{Op: "UpdateAgent", Path: "/v1/teams/{teamId}/agents/{agentId}", Body: `{"description":"matrix"}`, Headers: ifm, Prepare: agentPrep})
	Register(Case{Op: "DeleteAgent", Path: "/v1/teams/{teamId}/agents/{agentId}", Headers: ifm, Prepare: agentPrep})
	Register(Case{Op: "PauseAgent", Path: "/v1/teams/{teamId}/agents/{agentId}:pause", Prepare: agentPrep})
	Register(Case{Op: "RestartAgent", Path: "/v1/teams/{teamId}/agents/{agentId}:restart", Prepare: agentPrep})
	Register(Case{Op: "ResumeAgent", Path: "/v1/teams/{teamId}/agents/{agentId}:resume",
		Prepare: func(t *testing.T, w *World) map[string]string {
			p := w.seedAgent(t)
			w.exec(t, `UPDATE afs.agents SET state = 'paused' WHERE id = $1`, p["agentId"])
			return p
		}})
	Register(Case{Op: "ListAgentSnapshots", Path: "/v1/teams/{teamId}/agents/{agentId}/snapshots", Prepare: agentPrep})
	Register(Case{Op: "CreateAgentSnapshot", Path: "/v1/teams/{teamId}/agents/{agentId}/snapshots", Body: `{"description":"matrix"}`,
		NoOutbox: noEventSn, Prepare: agentPrep})
}

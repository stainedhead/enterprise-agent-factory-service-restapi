package matrix

import (
	"context"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of the Paperclip collaboration change (API contract 4.4b). Contract section 7 has event types for
// collaboration.updated and agent.collaboration_linked/unlinked, so those mutations write the outbox; the
// key rotation has none (CD-7).

// seedCollaboration makes a Paperclip template, the team's collaboration agent and its team_collaboration
// row in team A, and removes them with the subtest. It returns the placeholders a case needs.
func (w *World) seedCollaboration(t *testing.T) map[string]string {
	t.Helper()
	model, tpl := w.SeedAgentCatalog(t)
	w.exec(t, `UPDATE afs.agent_templates SET capabilities = ARRAY['orchestration','tool:paperclip'] WHERE name = $1`, tpl)
	// A second, plain template for the worker agent.
	wtpl := "mx-w-" + unique()
	w.exec(t, `INSERT INTO afs.agent_templates (id, name, harness, allowed_image_digests, current_image_digest)
VALUES ($1, $2, 'hermes', ARRAY[$3]::text[], $3)`, pgtest.NewID("tpl"), wtpl, mxDigest)
	collab := pgtest.NewID("agt")
	w.exec(t, `INSERT INTO afs.agents (id, team_id, name, template, template_id, image, model_id, role, iam_identity)
VALUES ($1, $2, $3, $4, (SELECT id FROM afs.agent_templates WHERE name = $4), $5, $6, 'orchestration', 'dedicated')`,
		collab, w.TeamA.ID, "mx-pc-"+unique(), tpl, mxImage, model)
	w.exec(t, `INSERT INTO afs.team_collaboration (team_id, agent_id) VALUES ($1, $2)`, w.TeamA.ID, collab)
	for _, n := range []string{"paperclip-better-auth-secret", "paperclip-tool-action-signing-secret", "paperclip-secrets-master-key"} {
		w.exec(t, `INSERT INTO afs.secrets (id, team_id, agent_id, name, secrets_manager_arn, origin, managed_by, current_version_id)
VALUES ($1, $2, $3, $4, 'arn:aws:secretsmanager:us-east-1:111122223333:secret:' || afs.secret_path($2, $3, $4) || '-AbCdEf', 'managed', 'afs', 'v1')`,
			pgtest.NewID("sec"), w.TeamA.ID, collab, n)
	}
	worker := pgtest.NewID("agt")
	w.exec(t, `INSERT INTO afs.agents (id, team_id, name, template, image, model_id) VALUES ($1, $2, $3, $4, $5, $6)`,
		worker, w.TeamA.ID, "mx-"+unique(), wtpl, mxImage, model)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		for _, q := range []string{
			`DELETE FROM afs.agent_volume_snapshots WHERE team_id = $1 AND reason = 'pre_tool_change'`,
			`DELETE FROM afs.agent_collaboration WHERE team_id = $1`,
			`DELETE FROM afs.secrets WHERE team_id = $1 AND managed_by = 'afs'`,
			`DELETE FROM afs.team_collaboration WHERE team_id = $1`,
		} {
			if _, err := w.Admin.Exec(ctx, q, w.TeamA.ID); err != nil {
				t.Errorf("matrix cleanup: %v", err)
			}
		}
		for _, q := range []string{
			`DELETE FROM afs.agents WHERE id = $1 OR id = $2`,
		} {
			if _, err := w.Admin.Exec(ctx, q, collab, worker); err != nil {
				t.Errorf("matrix cleanup: %v", err)
			}
		}
		if _, err := w.Admin.Exec(ctx, `DELETE FROM afs.agent_templates WHERE name = $1`, wtpl); err != nil {
			t.Errorf("matrix cleanup: %v", err)
		}
	})
	return map[string]string{"collabId": collab, "agentId": worker,
		"ver": ver(t, w, `SELECT version FROM afs.team_collaboration WHERE team_id = $1`, w.TeamA.ID)}
}

func collabPrep(t *testing.T, w *World) map[string]string { return w.seedCollaboration(t) }

// buzzRuntime makes Buzz an enabled tool with a template and an image repository, for the subtest.
func (w *World) buzzRuntime(t *testing.T) {
	t.Helper()
	tpl := pgtest.NewID("tpl")
	w.exec(t, `INSERT INTO afs.agent_templates (id, name, harness, allowed_image_digests, current_image_digest, capabilities)
VALUES ($1, $2, 'buzz', ARRAY[$3]::text[], $3, '{orchestration,tool:buzz}')`, tpl, "mx-buzz-"+unique(), mxDigest)
	w.exec(t, `UPDATE afs.service_settings SET value = jsonb_set(jsonb_set(jsonb_set(value, '{buzz,templateId}', to_jsonb($1::text)),
 '{buzz,imageRepository}', '"registry.example.com/buzz"'), '{buzz,enabled}', 'true') WHERE key = 'orchestration.tools'`, tpl)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		for _, q := range []string{
			`UPDATE afs.service_settings SET value = jsonb_set(jsonb_set(jsonb_set(value, '{buzz,templateId}', 'null'), '{buzz,imageRepository}', 'null'), '{buzz,enabled}', 'false') WHERE key = 'orchestration.tools'`,
		} {
			if _, err := w.Admin.Exec(ctx, q); err != nil {
				t.Errorf("matrix cleanup: %v", err)
			}
		}
		if _, err := w.Admin.Exec(ctx, `DELETE FROM afs.agent_templates WHERE id = $1`, tpl); err != nil {
			t.Errorf("matrix cleanup: %v", err)
		}
	})
}

func init() {
	ifm := map[string]string{"If-Match": ifMatch}
	Register(Case{Op: "GetTeamOrchestration", Path: "/v1/teams/{teamId}/orchestration", Prepare: collabPrep})
	Register(Case{Op: "PutTeamOrchestration", Path: "/v1/teams/{teamId}/orchestration", Headers: ifm, Prepare: collabPrep,
		Body: `{"webUi":{"exposure":"off"},"webApi":{"exposure":"off"}}`})
	Register(Case{Op: "ChangeCollaborationTool", Path: "/v1/teams/{teamId}/orchestration:change-tool", Body: `{"tool":"buzz"}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.buzzRuntime(t) // first, so its cleanup runs after the agent that uses the template is gone
			return w.seedCollaboration(t)
		}})
	Register(Case{Op: "ListOrchestrationEndpoints", Path: "/v1/admin/orchestration-endpoints"})
	Register(Case{Op: "PutServiceModelAccess", Path: "/v1/admin/model-access", Body: `{"access":"all"}`, NoOutbox: cdOutbox})
	Register(Case{Op: "GetTeamCollaboration", Path: "/v1/teams/{teamId}/collaboration", Prepare: collabPrep})
	Register(Case{Op: "PutTeamCollaboration", Path: "/v1/teams/{teamId}/collaboration", Headers: ifm, Prepare: collabPrep,
		Body: `{"webUiEnabled":false,"restApiEnabled":false}`})
	Register(Case{Op: "ListCollaborationLinks", Path: "/v1/teams/{teamId}/collaboration/links", Prepare: collabPrep})
	Register(Case{Op: "ConnectAgentToCollaboration", Path: "/v1/teams/{teamId}/collaboration/links/{agentId}:connect", NoOutbox: cdOutbox, Prepare: collabPrep})
	Register(Case{Op: "DisconnectAgentFromCollaboration", Path: "/v1/teams/{teamId}/collaboration/links/{agentId}:disconnect", NoOutbox: cdOutbox,
		Prepare: func(t *testing.T, w *World) map[string]string {
			p := w.seedCollaboration(t)
			w.exec(t, `INSERT INTO afs.agent_collaboration (team_id, agent_id, connect) VALUES ($1, $2, true)`, w.TeamA.ID, p["agentId"])
			return p
		}})
	Register(Case{Op: "RotateCollaborationKeys", Path: "/v1/teams/{teamId}/collaboration:rotate-keys", Prepare: collabPrep})
	Register(Case{Op: "ListCollaborationEndpoints", Path: "/v1/admin/collaboration-endpoints"})
}

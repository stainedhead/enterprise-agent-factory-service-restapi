package matrix

import (
	"context"
	"strconv"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M2-D (configuration values) and the agent seeding shared with M2-E.

// seedAgent inserts a live agent of team A (and the model it needs) and returns its id.
func (w *World) seedAgent(t *testing.T) string {
	t.Helper()
	w.exec(t, `INSERT INTO afs.models (id, provider, display_name) VALUES ('mx-agent-model', 'Example', 'Matrix agent model') ON CONFLICT DO NOTHING`)
	w.exec(t, `INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, 'mx-agent-model') ON CONFLICT DO NOTHING`, w.TeamA.ID)
	id := pgtest.NewID("agt")
	w.exec(t, `INSERT INTO afs.agents (id, team_id, name, template, image, model_id) VALUES ($1, $2, $3, 'omp', $4, 'mx-agent-model')`,
		id, w.TeamA.ID, "mx-agent-"+unique(), "ghcr.io/example/omp@"+digestA)
	// Other cases (PutTeamModels) must not find the agent's model assignment, so remove it all again.
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context()) // the test context is already done in a cleanup
		for _, q := range []string{
			`DELETE FROM afs.agent_config_values WHERE agent_id = $1`,
			`DELETE FROM afs.external_system_optouts WHERE agent_id = $1`,
			`DELETE FROM afs.agents WHERE id = $1`,
		} {
			if _, err := w.Admin.Exec(ctx, q, id); err != nil {
				t.Errorf("matrix cleanup: %v", err)
			}
		}
		if _, err := w.Admin.Exec(ctx, `DELETE FROM afs.team_models WHERE team_id = $1 AND model_id = 'mx-agent-model'`, w.TeamA.ID); err != nil {
			t.Errorf("matrix cleanup: %v", err)
		}
	})
	return id
}

func cfgName() string { return "MX_" + strconv.Itoa(nextSeq()) }

func init() {
	Register(Case{Op: "ListNamespaceConfigValues", Path: "/v1/teams/{teamId}/config-values"})
	Register(Case{Op: "PutNamespaceConfigValue", Path: "/v1/teams/{teamId}/config-values/MX_PUT", Body: `{"value":"matrix"}`, NoOutbox: cdOutbox})
	nsValue := func(t *testing.T, w *World) map[string]string {
		name := cfgName()
		w.exec(t, `INSERT INTO afs.namespace_config_values (id, team_id, name, config_value) VALUES ($1, $2, $3, 'x')`, pgtest.NewID("ncv"), w.TeamA.ID, name)
		return map[string]string{"name": name, "ver": ver(t, w, `SELECT version FROM afs.namespace_config_values WHERE team_id = $1 AND name = $2`, w.TeamA.ID, name)}
	}
	Register(Case{Op: "DeleteNamespaceConfigValue", Path: "/v1/teams/{teamId}/config-values/{name}", NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: nsValue})

	Register(Case{Op: "ListAgentConfigValues", Path: "/v1/teams/{teamId}/agents/{agentId}/config-values",
		Prepare: func(t *testing.T, w *World) map[string]string { return map[string]string{"agentId": w.seedAgent(t)} }})
	Register(Case{Op: "PutAgentConfigValue", Path: "/v1/teams/{teamId}/agents/{agentId}/config-values/MX_PUT", NoOutbox: cdOutbox,
		Body:    `{"source":"literal","value":"matrix"}`,
		Prepare: func(t *testing.T, w *World) map[string]string { return map[string]string{"agentId": w.seedAgent(t)} }})
	Register(Case{Op: "DeleteAgentConfigValue", Path: "/v1/teams/{teamId}/agents/{agentId}/config-values/{name}", NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch},
		Prepare: func(t *testing.T, w *World) map[string]string {
			agent, name := w.seedAgent(t), cfgName()
			w.exec(t, `INSERT INTO afs.agent_config_values (id, team_id, agent_id, name, source, config_value) VALUES ($1, $2, $3, $4, 'literal', 'x')`,
				pgtest.NewID("acv"), w.TeamA.ID, agent, name)
			return map[string]string{"agentId": agent, "name": name,
				"ver": ver(t, w, `SELECT version FROM afs.agent_config_values WHERE agent_id = $1 AND name = $2`, agent, name)}
		}})
}

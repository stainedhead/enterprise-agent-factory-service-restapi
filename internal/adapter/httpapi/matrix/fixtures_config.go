package matrix

import (
	"context"
	"strconv"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M2-D (configuration values) and the agent seeding shared with M2-E.

// seedAgentID seeds an agent through M2-B's seedAgent (fixtures_agents.go) and also clears the
// configuration values and opt-outs the cases attach to it before the agent itself is removed.
func (w *World) seedAgentID(t *testing.T) string {
	t.Helper()
	id := w.seedAgent(t)["agentId"]
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context()) // the test context is already done in a cleanup
		for _, q := range []string{
			`DELETE FROM afs.agent_config_values WHERE agent_id = $1`,
			`DELETE FROM afs.external_system_optouts WHERE agent_id = $1`,
		} {
			if _, err := w.Admin.Exec(ctx, q, id); err != nil {
				t.Errorf("matrix cleanup: %v", err)
			}
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
		Prepare: func(t *testing.T, w *World) map[string]string { return map[string]string{"agentId": w.seedAgentID(t)} }})
	Register(Case{Op: "PutAgentConfigValue", Path: "/v1/teams/{teamId}/agents/{agentId}/config-values/MX_PUT", NoOutbox: cdOutbox,
		Body:    `{"source":"literal","value":"matrix"}`,
		Prepare: func(t *testing.T, w *World) map[string]string { return map[string]string{"agentId": w.seedAgentID(t)} }})
	Register(Case{Op: "DeleteAgentConfigValue", Path: "/v1/teams/{teamId}/agents/{agentId}/config-values/{name}", NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch},
		Prepare: func(t *testing.T, w *World) map[string]string {
			agent, name := w.seedAgentID(t), cfgName()
			w.exec(t, `INSERT INTO afs.agent_config_values (id, team_id, agent_id, name, source, config_value) VALUES ($1, $2, $3, $4, 'literal', 'x')`,
				pgtest.NewID("acv"), w.TeamA.ID, agent, name)
			return map[string]string{"agentId": agent, "name": name,
				"ver": ver(t, w, `SELECT version FROM afs.agent_config_values WHERE agent_id = $1 AND name = $2`, agent, name)}
		}})
}

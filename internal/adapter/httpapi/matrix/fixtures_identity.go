package matrix

import (
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M4-A (agent identities, role links, account listings). Contract §7 has no event for these
// mutations (CD-7); they still write an audit record.

const mxAccount = "111122223333"

// seedDedicatedAgent seeds an agent of team A with its own IAM role, a mirrored organization account and
// a team filter that allows it, and returns the agent's id.
func (w *World) seedDedicatedAgent(t *testing.T) string {
	t.Helper()
	id := w.seedAgentID(t)
	w.exec(t, `INSERT INTO afs.org_aws_accounts (id, name, status) VALUES ($1, 'mx-account', 'active') ON CONFLICT (id) DO UPDATE SET status = 'active'`, mxAccount)
	w.exec(t, `UPDATE afs.teams SET allowed_aws_accounts = ARRAY['*'] WHERE id = $1`, w.TeamA.ID)
	w.exec(t, `UPDATE afs.agents SET iam_identity = 'dedicated' WHERE id = $1`, id)
	return id
}

func (w *World) seedLink(t *testing.T, agentID string) map[string]string {
	t.Helper()
	id := pgtest.NewID("arl")
	w.exec(t, `INSERT INTO afs.agent_role_links (id, team_id, agent_id, account_id, role_arn, profile_name, external_id)
VALUES ($1, $2, $3, $4, $5, 'mx-prof', $6)`, id, w.TeamA.ID, agentID, mxAccount, "arn:aws:iam::"+mxAccount+":role/mx", "afs-"+strings.Repeat("a", 20)+strings.ToLower(unique()))
	return map[string]string{"agentId": agentID, "linkId": id, "ver": ver(t, w, `SELECT version FROM afs.agent_role_links WHERE id = $1`, id)}
}

func init() {
	linkPrep := func(t *testing.T, w *World) map[string]string { return w.seedLink(t, w.seedDedicatedAgent(t)) }
	Register(Case{Op: "ListAllowedAccounts", Path: "/v1/teams/{teamId}/allowed-accounts"})
	Register(Case{Op: "ListOrgAccounts", Path: "/v1/admin/org-accounts"})
	Register(Case{Op: "ListAgentRoleLinks", Path: "/v1/teams/{teamId}/agents/{agentId}/role-links",
		Prepare: func(t *testing.T, w *World) map[string]string { return map[string]string{"agentId": w.seedAgentID(t)} }})
	Register(Case{Op: "GetAgentRoleLink", Path: "/v1/teams/{teamId}/agents/{agentId}/role-links/{linkId}", Prepare: linkPrep})
	Register(Case{Op: "CreateAgentRoleLink", Path: "/v1/teams/{teamId}/agents/{agentId}/role-links", NoOutbox: cdOutbox,
		Body: `{"roleArn":"arn:aws:iam::` + mxAccount + `:role/mx-{n}","profileName":"mx-{n}"}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			return map[string]string{"agentId": w.seedDedicatedAgent(t), "n": strings.ToLower(unique())}
		}})
	Register(Case{Op: "DeleteAgentRoleLink", Path: "/v1/teams/{teamId}/agents/{agentId}/role-links/{linkId}", NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: linkPrep})
	Register(Case{Op: "VerifyAgentRoleLink", Path: "/v1/teams/{teamId}/agents/{agentId}/role-links/{linkId}:verify", NoOutbox: cdOutbox, Prepare: linkPrep})

	Register(Case{Op: "ListAgentIdentities", Path: "/v1/teams/{teamId}/agents/{agentId}/identities",
		Prepare: func(t *testing.T, w *World) map[string]string { return map[string]string{"agentId": w.seedAgentID(t)} }})
	Register(Case{Op: "CreateAgentIdentity", Path: "/v1/teams/{teamId}/agents/{agentId}/identities", NoOutbox: cdOutbox,
		Body:    `{"kind":"external","externalRef":"mx-account"}`,
		Prepare: func(t *testing.T, w *World) map[string]string { return map[string]string{"agentId": w.seedAgentID(t)} }})
	Register(Case{Op: "DeleteAgentIdentity", Path: "/v1/teams/{teamId}/agents/{agentId}/identities/{identityId}", NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch},
		Prepare: func(t *testing.T, w *World) map[string]string {
			agent := w.seedAgentID(t)
			id := pgtest.NewID("aid")
			w.exec(t, `INSERT INTO afs.agent_identities (id, team_id, agent_id, kind, external_ref) VALUES ($1, $2, $3, 'external', 'mx')`, id, w.TeamA.ID, agent)
			return map[string]string{"agentId": agent, "identityId": id, "ver": ver(t, w, `SELECT version FROM afs.agent_identities WHERE id = $1`, id)}
		}})
}

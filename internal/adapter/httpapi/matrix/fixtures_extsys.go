package matrix

import (
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
)

// Cases of M2-E (external systems at team and agent level).

// seedTeamSystem inserts a system declared by team A and returns its id and version.
func (w *World) seedTeamSystem(t *testing.T) (id, version string) {
	t.Helper()
	id = pgtest.NewID("ext")
	w.exec(t, `INSERT INTO afs.external_systems (id, team_id, slug, name, category, owner, purpose, endpoints) VALUES ($1, $2, $3, 'Matrix team system', 'other', 'owner', 'purpose', ARRAY['mx.example.com'])`,
		id, w.TeamA.ID, "mx-team-"+strings.ToLower(unique()))
	return id, ver(t, w, `SELECT version FROM afs.external_systems WHERE id = $1`, id)
}

func init() {
	Register(Case{Op: "ListTeamExternalSystems", Path: "/v1/teams/{teamId}/external-systems"})
	Register(Case{Op: "SetTeamExternalSystemState", Path: "/v1/teams/{teamId}/external-systems/{sid}/state", Body: `{"enabled":false}`, NoOutbox: cdOutbox,
		Prepare: func(t *testing.T, w *World) map[string]string {
			id, _ := w.seedCatalogSystem(t)
			return map[string]string{"sid": id}
		}})
	Register(Case{Op: "DeclareTeamExternalSystem", Path: "/v1/teams/{teamId}/external-systems", NoOutbox: cdOutbox,
		Flags:   authz.TeamFlags{MayAddExternalSystems: true},
		Body:    `{"slug":"mx-decl-{n}","name":"Matrix","category":"other","endpoints":["mx.example.com"],"owner":"platform","purpose":"matrix"}`,
		Prepare: func(*testing.T, *World) map[string]string { return map[string]string{"n": strings.ToLower(unique())} }})
	teamSystem := func(t *testing.T, w *World) map[string]string {
		id, v := w.seedTeamSystem(t)
		return map[string]string{"sid": id, "ver": v}
	}
	// The database refuses a team-declared system unless the team's flag is on, so these cases seed with it on.
	declared := authz.TeamFlags{MayAddExternalSystems: true}
	Register(Case{Op: "UpdateTeamExternalSystem", Flags: declared, Path: "/v1/teams/{teamId}/external-systems/{sid}", Body: `{"purpose":"renamed"}`, NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: teamSystem})
	Register(Case{Op: "DeleteTeamExternalSystem", Flags: declared, Path: "/v1/teams/{teamId}/external-systems/{sid}", NoOutbox: cdOutbox,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: teamSystem})

	Register(Case{Op: "ListAgentExternalSystems", Path: "/v1/teams/{teamId}/agents/{agentId}/external-systems",
		Prepare: func(t *testing.T, w *World) map[string]string { return map[string]string{"agentId": w.seedAgent(t)} }})
	Register(Case{Op: "SetAgentExternalSystemState", Path: "/v1/teams/{teamId}/agents/{agentId}/external-systems/{sid}/state", Body: `{"enabled":false}`, NoOutbox: cdOutbox,
		Prepare: func(t *testing.T, w *World) map[string]string {
			id, _ := w.seedCatalogSystem(t)
			return map[string]string{"agentId": w.seedAgent(t), "sid": id}
		}})
}

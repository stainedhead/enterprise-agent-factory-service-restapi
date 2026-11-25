package postgres

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Route members of agents and pools (API contract v1.5.0 §4.13, migration 000023): route_access,
// default_route and the selected routes in agent_model_endpoints or pool_model_endpoints, by slug.

// routeCols returns the select columns of the route members of the subject aliased alias, whose
// selection is in table keyed by col: route_access, default_route (” for NULL) and the selected slugs.
func subjectRouteCols(alias, table, col string) string {
	return alias + `.route_access, coalesce(` + alias + `.default_route, ''),
  coalesce((SELECT array_agg(e.slug ORDER BY e.slug) FROM afs.` + table + ` x JOIN afs.model_endpoints e ON e.id = x.endpoint_id
    WHERE x.team_id = ` + alias + `.team_id AND x.` + col + ` = ` + alias + `.id), '{}')`
}

// routeModels fills the route members of m from the scanned columns of subjectRouteCols.
func routeModels(m *domain.AgentModels, access, defRoute string, slugs []string) {
	m.RouteAccess, m.DefaultRoute, m.Routes = domain.ModelAccess(access), defRoute, slugs
	if m.Routes == nil {
		m.Routes = []string{}
	}
	m.EffectiveRoutes, m.Env = []string{}, []domain.EnvVar{}
}

// nullRoute is default_route for a write: NULL when the default is a Bedrock model.
func nullRoute(m domain.AgentModels) any { return nullable(m.DefaultRoute) }

// replaceRoutes makes the stored route selection of a subject exactly m.Routes while its route access is
// selected, and empty while it is all. A slug names the team's own endpoint or a service endpoint. Only
// missing rows are inserted, so the 50-route limit trigger sees the real count.
func replaceRoutes(ctx context.Context, pt *Tx, table, col, teamID, id string, m domain.AgentModels) error {
	want := []string{}
	if accessOrAll(m.RouteAccess) == domain.ModelAccessSelected {
		want = m.Routes
	}
	if _, err := pt.Exec(ctx, `DELETE FROM afs.`+table+` x USING afs.model_endpoints e
WHERE x.team_id = $1 AND x.`+col+` = $2 AND e.id = x.endpoint_id AND NOT (e.slug = ANY($3))`, teamID, id, want); err != nil {
		return MapError(err, OpWrite, "routes")
	}
	if len(want) == 0 {
		return nil
	}
	_, err := pt.Exec(ctx, `INSERT INTO afs.`+table+` (team_id, `+col+`, endpoint_id)
SELECT $1, $2, e.id FROM afs.model_endpoints e
WHERE e.slug = ANY($3) AND (e.team_id IS NULL OR e.team_id = $1)
  AND NOT EXISTS (SELECT 1 FROM afs.`+table+` x WHERE x.team_id = $1 AND x.`+col+` = $2 AND x.endpoint_id = e.id)`, teamID, id, want)
	return MapError(err, OpWrite, "routes")
}

// effectiveRoutes fills EffectiveRoutes (default first, then by slug) and Env of each subject from
// afs.effective_agent_routes and the Bedrock switch. at returns the models of the subject with an id.
func effectiveRoutes(ctx context.Context, pt *Tx, teamID, kind string, ids []string, at func(id string) *domain.AgentModels) error {
	var bedrockOn bool
	if err := pt.QueryRow(ctx, `SELECT afs.provider_enabled('bedrock')`).Scan(&bedrockOn); err != nil {
		return MapError(err, OpRead, "routes")
	}
	rows, err := pt.Query(ctx, `SELECT DISTINCT subject_id, slug FROM afs.effective_agent_routes
WHERE team_id = $1 AND subject_kind = $2 AND subject_id = ANY($3)`, teamID, kind, ids)
	if err != nil {
		return MapError(err, OpRead, "routes")
	}
	got := map[string][]string{}
	for rows.Next() {
		var sid, slug string
		if err := rows.Scan(&sid, &slug); err != nil {
			rows.Close()
			return MapError(err, OpRead, "routes")
		}
		got[sid] = append(got[sid], slug)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return MapError(err, OpRead, "routes")
	}
	for _, id := range ids {
		m := at(id)
		m.EffectiveRoutes = domain.OrderRoutes(got[id], m.DefaultRoute)
		defModel := ""
		if m.DefaultRoute != "" {
			defModel = m.DefaultModelID
		}
		m.Env = domain.RouteEnv(m.EffectiveRoutes, m.DefaultRoute, defModel, bedrockOn)
	}
	return nil
}

// teamRoutes reads the team's route set: each enabled route on a provider that is on, the team's own or
// opted in, with its callable model names (a per_token model needs a price in force).
func teamRoutes(ctx context.Context, pt *Tx, teamID string) (map[string][]string, error) {
	rows, err := pt.Query(ctx, `SELECT e.slug, coalesce(array_agg(m.model_name ORDER BY m.model_name) FILTER (WHERE m.model_name IS NOT NULL), '{}')
FROM afs.model_endpoints e
LEFT JOIN afs.endpoint_models m ON m.endpoint_id = e.id AND m.enabled
  AND (e.pricing_mode <> 'per_token' OR EXISTS (SELECT 1 FROM afs.endpoint_model_prices pr
       WHERE pr.endpoint_id = e.id AND pr.model_name = m.model_name AND pr.effective_from <= now()))
WHERE e.enabled AND afs.provider_enabled(e.provider)
  AND (e.team_id = $1 OR (e.team_id IS NULL AND EXISTS (SELECT 1 FROM afs.team_model_endpoints tm WHERE tm.team_id = $1 AND tm.endpoint_id = e.id)))
GROUP BY e.slug`, teamID)
	if err != nil {
		return nil, MapError(err, OpRead, "routes")
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var slug string
		var models []string
		if err := rows.Scan(&slug, &models); err != nil {
			return nil, MapError(err, OpRead, "routes")
		}
		out[slug] = models
	}
	return out, MapError(rows.Err(), OpRead, "routes")
}

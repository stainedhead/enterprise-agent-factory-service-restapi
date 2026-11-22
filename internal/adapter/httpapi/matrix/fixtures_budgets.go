package matrix

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M3-E (budgets). Contract §7 has no event for a budget change (CD-7), so PutBudget is exempt
// from the outbox check; it still writes an audit record. Team A's ceiling is 1000.

func init() {
	// budget gives team A a monthly budget of 400 and returns its version.
	budget := func(t *testing.T, w *World) map[string]string {
		w.exec(t, `DELETE FROM afs.budgets WHERE team_id = $1`, w.TeamA.ID)
		w.exec(t, `INSERT INTO afs.budgets (id, team_id, period, amount_usd) VALUES ($1, $2, 'monthly', 400)`, pgtest.NewID("bud"), w.TeamA.ID)
		return map[string]string{"ver": ver(t, w, `SELECT version FROM afs.budgets WHERE team_id = $1`, w.TeamA.ID)}
	}
	Register(Case{Op: "GetBudget", Path: "/v1/teams/{teamId}/budget", Prepare: budget})
	Register(Case{Op: "PutBudget", Path: "/v1/teams/{teamId}/budget", NoOutbox: cdOutbox,
		Body:    `{"period":"monthly","amountUsd":"500.00","thresholds":[50,80,100],"onExhaust":"block"}`,
		Headers: map[string]string{"If-Match": ifMatch}, Prepare: budget})
}

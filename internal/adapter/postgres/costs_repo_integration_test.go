package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/costs"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

func TestCostsRepo(t *testing.T) {
	db := pgtest.Get(t)
	admin := db.Admin(t)
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	runner := postgres.NewTxRunner(appPool(t, db))
	repo := postgres.NewCostsRepo()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	var tsA, tsB string
	if err := admin.QueryRow(t.Context(), `SELECT afs.tag_set_for($1, '{"business-unit":"payments"}'::jsonb), afs.tag_set_for($1, '{"business-unit":"platform"}'::jsonb)`, a.ID).Scan(&tsA, &tsB); err != nil {
		t.Fatal(err)
	}
	d := func(n int) time.Time { return time.Date(2032, 5, n, 0, 0, 0, 0, time.UTC) }
	// Team A on day 1: gateway rows, and a CUR day split by two tag sets (summed in the API view).
	exec(`INSERT INTO afs.cost_daily (team_id, day, source, agent_id, model_id, cost_usd, calls, tag_set_id) VALUES ($1, $2, 'gateway', 'agt_1', 'm1', 2.5, 40, $3)`, a.ID, d(1), tsA)
	exec(`INSERT INTO afs.cost_daily (team_id, day, source, agent_id, model_id, cost_usd, calls, tag_set_id) VALUES ($1, $2, 'gateway', 'agt_1', 'm1', 1, 10, $3)`, a.ID, d(1), tsB)
	exec(`INSERT INTO afs.cost_daily (team_id, day, source, model_id, cost_usd, tag_set_id) VALUES ($1, $2, 'cur_bedrock', 'm1', 3, $3)`, a.ID, d(1), tsA)
	exec(`INSERT INTO afs.cost_daily (team_id, day, source, model_id, cost_usd, tag_set_id) VALUES ($1, $2, 'cur_bedrock', 'm1', 0.75, $3)`, a.ID, d(1), tsB)
	exec(`INSERT INTO afs.cost_daily (team_id, day, source, pool_id, cost_usd) VALUES ($1, $2, 'cur_eks', 'pool_1', 4)`, a.ID, d(2))
	exec(`INSERT INTO afs.cost_daily (team_id, day, source, cost_usd) VALUES ($1, $2, 'cur_other', 9)`, b.ID, d(1))

	read := func(s port.Session, teamID string, offset, limit int) ([]costs.Row, error) {
		var rows []costs.Row
		err := runner.ReadTx(t.Context(), s, func(ctx context.Context, tx port.Tx) error {
			if err := repo.TeamExists(ctx, tx, teamID); err != nil {
				return err
			}
			var err error
			rows, err = repo.ListCosts(ctx, tx, teamID, d(1), d(5), offset, limit)
			return err
		})
		return rows, err
	}
	rows, err := read(userSession(a.ID), a.ID, 0, 10)
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows = %+v err=%v", rows, err)
	}
	c, g, e := rows[0], rows[1], rows[2] // sources in alphabetical order within a day
	if g.Source != "gateway" || g.CostUSD.String() != "3.50" || g.Calls != 50 || g.AgentID == nil || *g.AgentID != "agt_1" || g.PoolID != nil {
		t.Fatalf("gateway row = %+v", g)
	}
	if c.Source != "cur_bedrock" || c.CostUSD.String() != "3.75" || c.Calls != 0 || c.AgentID != nil {
		t.Fatalf("cur row = %+v", c)
	}
	if e.Source != "cur_eks" || e.PoolID == nil || *e.PoolID != "pool_1" || !e.Day.Equal(d(2)) {
		t.Fatalf("eks row = %+v", e)
	}
	if page, _ := read(userSession(a.ID), a.ID, 1, 1); len(page) != 1 || page[0].Source != "gateway" {
		t.Fatalf("offset page = %+v", page)
	}
	// Row-level security: team A's caller does not see team B, and a service admin sees both.
	if _, err := read(userSession(a.ID), b.ID, 0, 10); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("other team: %v", err)
	}
	svc := port.Session{Actor: user, ActorType: port.ActorUser, ServiceAdmin: true, RequestID: "req_test"}
	if rows, err := read(svc, b.ID, 0, 10); err != nil || len(rows) != 1 {
		t.Fatalf("service admin rows = %+v err=%v", rows, err)
	}
	// A deleted team is still readable by service admins (RA-US-16.3).
	exec(`UPDATE afs.teams SET state = 'deleted', deleted_at = now() WHERE id = $1`, b.ID)
	if rows, err := read(svc, b.ID, 0, 10); err != nil || len(rows) != 1 {
		t.Fatalf("deleted team, service admin: rows = %+v err=%v", rows, err)
	}

	// Reconciliations: newest day first, filter by status, keyset paging, admin sees all teams.
	exec(`INSERT INTO afs.cost_reconciliations (team_id, day, gateway_usd, billed_usd, status) VALUES ($1, $3, 100, 80, 'ok'), ($2, $3, 100, 100, 'ok'), ($1, $4, 5, 0, 'ok')`,
		a.ID, b.ID, d(3), d(4))
	var all []costs.Reconciliation
	var after *costs.ReconCursor
	for {
		var page []costs.Reconciliation
		if err := runner.ReadTx(t.Context(), svc, func(ctx context.Context, tx port.Tx) error {
			var err error
			page, err = repo.ListReconciliations(ctx, tx, "", after, 2)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		all = append(all, page...)
		if len(page) < 2 {
			break
		}
		last := page[len(page)-1]
		after = &costs.ReconCursor{Day: last.Day, TeamID: last.TeamID}
	}
	var mine []costs.Reconciliation
	for _, r := range all {
		if r.TeamID == a.ID || r.TeamID == b.ID {
			mine = append(mine, r)
		}
	}
	if len(mine) != 3 || !mine[0].Day.Equal(d(4)) || mine[0].Status != "missing_billing" || mine[0].VariancePct != nil {
		t.Fatalf("reconciliations = %+v", mine)
	}
	if mine[1].Status != "over_threshold" || mine[1].VariancePct == nil || *mine[1].VariancePct != "25.000" || mine[1].GatewayUSD.String() != "100.00" {
		t.Fatalf("over row = %+v", mine[1])
	}
	var onlyOver []costs.Reconciliation
	if err := runner.ReadTx(t.Context(), svc, func(ctx context.Context, tx port.Tx) error {
		var err error
		onlyOver, err = repo.ListReconciliations(ctx, tx, "over_threshold", nil, 50)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range onlyOver {
		if r.Status != "over_threshold" {
			t.Fatalf("filter leaked %+v", r)
		}
	}
	// A team caller sees only its own reconciliations.
	var own []costs.Reconciliation
	if err := runner.ReadTx(t.Context(), userSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		var err error
		own, err = repo.ListReconciliations(ctx, tx, "", nil, 50)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, r := range own {
		if r.TeamID != a.ID {
			t.Fatalf("a team caller saw %+v", r)
		}
	}
}

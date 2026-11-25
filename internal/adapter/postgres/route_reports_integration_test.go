package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// MEG-API-6: usage and costs group by route and filter on it, on the per-call path and on the daily
// rollup, and the gateway rollup writes route calls as source route with <route>/<model>.
func TestReportsByRouteAgainstTheSchema(t *testing.T) {
	db := pgtest.Get(t)
	tm := db.SeedTeam(t)
	admin := db.Admin(t)
	ctx := t.Context()
	slug, ep := mepSlug("rr"), pgtest.NewID("mep")
	mustExec(t, admin, `INSERT INTO afs.model_endpoints (id, slug, display_name, location, base_url, auth_kind, pricing_mode, enabled)
VALUES ($1, $2, 'Reports', 'external', 'https://llm.example.com/v1', 'none', 'free', true)`, ep, slug)
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	at := day.Add(6 * time.Hour)
	agent := pgtest.NewID("agt")
	mustExec(t, admin, `INSERT INTO afs.usage_events (occurred_at, team_id, agent_id, model_id, endpoint_id, model_name, request_id, outcome,
  input_tokens, output_tokens, cost_usd) VALUES
 ($1, $2, $3, NULL, $4, 'm1', $5, 'ok', 100, 10, 0.50),
 ($1, $2, $3, NULL, $4, 'm1', $6, 'ok', 100, 10, 0.50),
 ($1, $2, $3, 'model-b', NULL, NULL, $7, 'ok', 10, 1, 0.25)`, at, tm.ID, agent, ep, pgtest.NewID("req"), pgtest.NewID("req"), pgtest.NewID("req"))

	repo, runner := postgres.NewReportsRepo(), postgres.NewTxRunner(appPool(t, db))
	run := func(kind domain.ReportKind, raw domain.RawReportQuery) []domain.ReportRow {
		t.Helper()
		q, err := domain.ParseReportQuery(kind, raw, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		var rows []domain.ReportRow
		err = runner.InTx(ctx, teamSession(tm.ID), func(ctx context.Context, tx port.Tx) error {
			res, err := repo.Run(ctx, tx, q, 0, 100)
			rows = res.Rows
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	from, to := day.Format(time.RFC3339), day.Add(24*time.Hour).Format(time.RFC3339)
	per := run(domain.ReportKindUsage, domain.RawReportQuery{From: from, To: to, GroupBy: "route,model"})
	if len(per) != 2 || per[0].Group[0] != "bedrock" || per[0].Group[1] != "model-b" ||
		per[1].Group[0] != slug || per[1].Group[1] != slug+"/m1" || per[1].Measures[0] != int64(2) {
		t.Errorf("per-call by route = %+v", per)
	}
	only := run(domain.ReportKindUsage, domain.RawReportQuery{From: from, To: to, GroupBy: "agent", Route: slug})
	if len(only) != 1 || only[0].Measures[0] != int64(2) {
		t.Errorf("route filter = %+v", only)
	}

	// The gateway rollup: Bedrock as gateway, the route as route.
	billing := postgres.NewBillingStore(postgres.NewTxRunner(appPool(t, db)))
	bill, err := postgres.OpenPool(ctx, postgres.Config{URL: db.URLs["afs_billing"], Service: "afs-billing", SystemActor: true, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bill.Close)
	if err := postgres.NewTxRunner(bill).InTx(ctx, postgres.BillingSession(), func(ctx context.Context, tx port.Tx) error {
		return billing.RollupGateway(ctx, tx, day)
	}); err != nil {
		t.Fatal(err)
	}
	var src, model string
	if err := admin.QueryRow(ctx, `SELECT source, model_id FROM afs.cost_daily WHERE team_id = $1 AND day = $2::date AND model_id LIKE '%/%'`,
		tm.ID, day.Format(time.DateOnly)).Scan(&src, &model); err != nil || src != "route" || model != slug+"/m1" {
		t.Errorf("rollup row = %s %s %v", src, model, err)
	}
	daily := run(domain.ReportKindCosts, domain.RawReportQuery{Range: "7d", GroupBy: "route"})
	if len(daily) != 2 || daily[0].Group[0] != "bedrock" || daily[1].Group[0] != slug || daily[1].Measures[0] != "1.00" {
		t.Errorf("daily by route = %+v", daily)
	}
	if rows := run(domain.ReportKindUsage, domain.RawReportQuery{Range: "7d", GroupBy: "day", Route: "bedrock"}); len(rows) != 1 || rows[0].Measures[0] != int64(1) {
		t.Errorf("daily bedrock filter = %+v", rows)
	}
	if _, err := domain.ParseReportQuery(domain.ReportKindActivity, domain.RawReportQuery{Range: "24h", Route: slug}, time.Now()); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("route on activity: %v", err)
	}
}

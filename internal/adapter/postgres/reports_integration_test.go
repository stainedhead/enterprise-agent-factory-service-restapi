package postgres_test

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reports"
)

var rptNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

type rptFixture struct {
	db       *pgtest.DB
	a, b     pgtest.Team
	agt1     string
	agt2     string
	tagged   string // tag set business-unit=retail
	tagged2  string // business-unit=bank
	repo     *postgres.ReportsRepo
	runner   *postgres.TxRunner
	admin    *pgx.Conn
	otherAgt string
}

func (f *rptFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.admin.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (f *rptFixture) tagSet(t *testing.T, team, bu string) string {
	t.Helper()
	var id string
	if err := f.admin.QueryRow(t.Context(), `SELECT afs.tag_set_for($1, jsonb_build_object('business-unit', $2::text))`, team, bu).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func newRptFixture(t *testing.T) *rptFixture {
	t.Helper()
	db := pgtest.Get(t)
	f := &rptFixture{db: db, repo: postgres.NewReportsRepo(), runner: postgres.NewTxRunner(appPool(t, db)), admin: db.Admin(t)}
	f.a, f.b = db.SeedTeam(t), db.SeedTeam(t)
	f.agt1, f.agt2, f.otherAgt = pgtest.NewID("agt"), pgtest.NewID("agt"), pgtest.NewID("agt")
	f.tagged, f.tagged2 = f.tagSet(t, f.a.ID, "retail"), f.tagSet(t, f.a.ID, "bank")
	bTag := f.tagSet(t, f.b.ID, "retail")

	// Per-call usage in the last 20 hours: team A has two agents, two models, tagged and untagged.
	ev := func(team, agent, model, outcome string, ago time.Duration, in, out int, cost string, tag any) {
		f.exec(t, `INSERT INTO afs.usage_events (occurred_at, team_id, agent_id, model_id, request_id, outcome, input_tokens, output_tokens, cost_usd, tag_set_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10)`, rptNow.Add(-ago), team, agent, model, pgtest.NewID("req"), outcome, in, out, cost, tag)
	}
	ev(f.a.ID, f.agt1, "model-x", "ok", time.Hour, 100, 50, "1.25", f.tagged)
	ev(f.a.ID, f.agt1, "model-x", "ok", 2*time.Hour, 200, 70, "2.50", f.tagged)
	ev(f.a.ID, f.agt1, "model-y", "error", 3*time.Hour, 10, 0, "0.10", nil)
	ev(f.a.ID, f.agt2, "model-x", "ok", 4*time.Hour, 300, 90, "3.75", f.tagged2)
	ev(f.a.ID, f.agt2, "model-y", "rejected_budget", 5*time.Hour, 0, 0, "0", nil)
	ev(f.b.ID, f.otherAgt, "model-x", "ok", time.Hour, 999, 999, "9.99", bTag)

	// Daily rollups over ten days: gateway and CUR.
	for d := 1; d <= 10; d++ {
		day := rptNow.AddDate(0, 0, -d).Format("2006-01-02")
		f.exec(t, `INSERT INTO afs.cost_daily (team_id, day, source, agent_id, model_id, calls, input_tokens, output_tokens, cost_usd, tag_set_id) VALUES ($1, $2, 'gateway', $3, 'model-x', 10, 1000, 500, 2.5, $4)`, f.a.ID, day, f.agt1, f.tagged)
		f.exec(t, `INSERT INTO afs.cost_daily (team_id, day, source, agent_id, model_id, calls, input_tokens, output_tokens, cost_usd) VALUES ($1, $2, 'gateway', $3, 'model-y', 5, 400, 100, 1)`, f.a.ID, day, f.agt2)
		f.exec(t, `INSERT INTO afs.cost_daily (team_id, day, source, model_id, cost_usd) VALUES ($1, $2, 'cur_bedrock', 'model-x', 3.9)`, f.a.ID, day)
		f.exec(t, `INSERT INTO afs.cost_daily (team_id, day, source, agent_id, model_id, calls, cost_usd) VALUES ($1, $2, 'gateway', $3, 'model-x', 99, 99)`, f.b.ID, day, f.otherAgt)
	}

	// Activity: every 5 minutes for 3 hours yesterday-ish, plus a long run for the daily view.
	act := func(team, agent, state string, bucket time.Time, calls int, tag any) {
		secs := 0
		if state == "working" {
			secs = 120
		} else {
			calls = 0
		}
		f.exec(t, `INSERT INTO afs.agent_activity (bucket_start, team_id, agent_id, state, active_seconds, model_calls, tag_set_id) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			bucket, team, agent, state, secs, calls, tag)
	}
	base := rptNow.Add(-6 * time.Hour).Truncate(5 * time.Minute)
	for i := range 12 {
		st := "idle"
		calls := 0
		if i%3 == 0 {
			st, calls = "working", 2
		}
		act(f.a.ID, f.agt1, st, base.Add(time.Duration(i)*5*time.Minute), calls, f.tagged)
	}
	act(f.a.ID, f.agt2, "paused", base, 0, nil)
	act(f.a.ID, f.agt2, "unhealthy", base.Add(5*time.Minute), 0, nil)
	for d := 3; d <= 6; d++ {
		for i := range 4 {
			st := "idle"
			if i < 3 {
				st = "working"
			}
			act(f.a.ID, f.agt1, st, rptNow.AddDate(0, 0, -d).Truncate(24*time.Hour).Add(time.Duration(i)*5*time.Minute), 1, f.tagged)
		}
	}

	// Traffic.
	tr := func(team, agent string, bucket time.Time, host, verdict string, conns int, tag any) {
		reason := any(nil)
		if verdict == "denied" {
			reason = "not_in_catalog"
		}
		f.exec(t, `INSERT INTO afs.external_traffic (bucket_start, team_id, agent_id, host, port, verdict, denied_reason, connections, bytes_out, bytes_in, tag_set_id) VALUES ($1, $2, $3, $4, 443, $5, $6, $7, $7 * 10, $7 * 100, $8)`,
			bucket, team, agent, host, verdict, reason, conns, tag)
	}
	tr(f.a.ID, f.agt1, base, "api.example.org", "allowed", 4, f.tagged)
	tr(f.a.ID, f.agt1, base.Add(5*time.Minute), "api.example.org", "allowed", 6, f.tagged)
	tr(f.a.ID, f.agt1, base, "evil.example.net", "denied", 3, f.tagged)
	tr(f.a.ID, f.agt2, base, "api.example.org", "allowed", 1, nil)
	for d := 3; d <= 6; d++ {
		tr(f.a.ID, f.agt1, rptNow.AddDate(0, 0, -d).Truncate(24*time.Hour), "api.example.org", "allowed", 2, f.tagged)
		tr(f.a.ID, f.agt1, rptNow.AddDate(0, 0, -d).Truncate(24*time.Hour), "evil.example.net", "denied", 1, f.tagged)
	}
	return f
}

func (f *rptFixture) run(t *testing.T, sess port.Session, kind domain.ReportKind, raw domain.RawReportQuery) reports.Result {
	t.Helper()
	q, err := domain.ParseReportQuery(kind, raw, rptNow)
	if err != nil {
		t.Fatal(err)
	}
	var res reports.Result
	err = f.runner.InTx(t.Context(), sess, func(ctx context.Context, tx port.Tx) error {
		var err error
		res, err = f.repo.Run(ctx, tx, q, 0, 1000)
		return err
	})
	if err != nil {
		t.Fatalf("%s %+v: %v", kind, raw, err)
	}
	return res
}

func rowMap(r domain.ReportRow, measures []domain.Measure) map[string]any {
	m := map[string]any{"_g": fmt.Sprint(r.Group)}
	for i, ms := range measures {
		m[ms.Name] = r.Measures[i]
	}
	return m
}

func sessOf(team string) port.Session {
	return port.Session{Actor: user, ActorType: port.ActorUser, TeamIDs: []string{team}, RequestID: "req_test"}
}

// RA-US-7.1: totals equal the sums of the underlying tables for the range, at the 5-minute level.
func TestReportsUsageShortRange(t *testing.T) {
	f := newRptFixture(t)
	sess := sessOf(f.a.ID)
	raw := domain.RawReportQuery{Range: "24h", TeamID: f.a.ID, GroupBy: "model"}
	res := f.run(t, sess, domain.ReportKindUsage, raw)
	ms := domain.ReportMeasures(domain.ReportKindUsage)
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %+v", res.Rows)
	}
	x, y := rowMap(res.Rows[0], ms), rowMap(res.Rows[1], ms)
	// model-x: three ok calls; model-y: one error call counts, the rejected one does not.
	if x["calls"] != int64(3) || x["inputTokens"] != int64(600) || x["outputTokens"] != int64(210) || x["costUsd"] != "7.50" {
		t.Errorf("model-x = %v", x)
	}
	if y["calls"] != int64(1) || y["costUsd"] != "0.10" {
		t.Errorf("model-y = %v", y)
	}
	if res.Totals[0] != int64(4) || res.Totals[3] != "7.60" {
		t.Errorf("totals = %v", res.Totals)
	}
	// Another team's rows are never included: the session sees only team A.
	all := f.run(t, sess, domain.ReportKindUsage, domain.RawReportQuery{Range: "24h", GroupBy: "team"})
	if len(all.Rows) != 1 || all.Rows[0].Group[0] != f.a.ID {
		t.Errorf("team A session saw %+v", all.Rows)
	}
	// A service admin sees both and can filter.
	adm := f.run(t, adminSession(), domain.ReportKindUsage, domain.RawReportQuery{Range: "24h", GroupBy: "team"})
	if len(adm.Rows) < 2 {
		t.Errorf("service admin rows = %+v", adm.Rows)
	}
	only := f.run(t, adminSession(), domain.ReportKindUsage, domain.RawReportQuery{Range: "24h", GroupBy: "team", TeamID: f.b.ID})
	if len(only.Rows) != 1 || only.Totals[3] != "9.99" {
		t.Errorf("team B filter = %+v %v", only.Rows, only.Totals)
	}
}

// RA-US-7.4: groupBy=tag:<key> gives one row per value plus an untagged row.
func TestReportsGroupByTag(t *testing.T) {
	f := newRptFixture(t)
	res := f.run(t, sessOf(f.a.ID), domain.ReportKindUsage, domain.RawReportQuery{Range: "24h", GroupBy: "tag:business-unit"})
	ms := domain.ReportMeasures(domain.ReportKindUsage)
	got := map[string]string{}
	for _, r := range res.Rows {
		got[r.Group[0]] = rowMap(r, ms)["costUsd"].(string)
	}
	want := map[string]string{"retail": "3.75", "bank": "3.75", domain.UntaggedValue: "0.10"}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %s, want %s (%v)", k, got[k], v, got)
		}
	}
	// Filter by tag, and by untagged.
	r := f.run(t, sessOf(f.a.ID), domain.ReportKindUsage, domain.RawReportQuery{Range: "24h", GroupBy: "agent", Tags: map[string]string{"business-unit": "retail"}})
	if len(r.Rows) != 1 || r.Totals[3] != "3.75" {
		t.Errorf("retail = %+v %v", r.Rows, r.Totals)
	}
	r = f.run(t, sessOf(f.a.ID), domain.ReportKindUsage, domain.RawReportQuery{Range: "24h", GroupBy: "agent", Tags: map[string]string{"business-unit": domain.UntaggedValue}})
	if r.Totals[3] != "0.10" {
		t.Errorf("untagged = %v", r.Totals)
	}
	// Tags on a daily range read the daily rollup, which carries the tag set.
	d := f.run(t, sessOf(f.a.ID), domain.ReportKindUsage, domain.RawReportQuery{Range: "30d", GroupBy: "tag:business-unit"})
	dm := map[string]string{}
	for _, r := range d.Rows {
		dm[r.Group[0]] = rowMap(r, ms)["costUsd"].(string)
	}
	if dm["retail"] != "25.00" || dm[domain.UntaggedValue] != "10.00" {
		t.Errorf("daily tags = %v", dm)
	}
}

// RA-US-7.1: daily ranges read the rollups; costs split estimated and billed.
func TestReportsDailyUsageAndCosts(t *testing.T) {
	f := newRptFixture(t)
	sess := sessOf(f.a.ID)
	u := f.run(t, sess, domain.ReportKindUsage, domain.RawReportQuery{Range: "30d", GroupBy: "day"})
	if len(u.Rows) != 10 {
		t.Fatalf("days = %d", len(u.Rows))
	}
	// Per day: 10 + 5 gateway calls, 3.5 USD; CUR rows are not usage.
	if u.Totals[0] != int64(150) || u.Totals[1] != int64(14000) || u.Totals[3] != "35.00" {
		t.Errorf("usage totals = %v", u.Totals)
	}
	var sum int64
	for _, r := range u.Rows {
		sum += r.Measures[0].(int64)
	}
	if sum != 150 {
		t.Errorf("rows sum to %d", sum)
	}
	c := f.run(t, sess, domain.ReportKindCosts, domain.RawReportQuery{Range: "30d", GroupBy: "model"})
	cm := domain.ReportMeasures(domain.ReportKindCosts)
	if len(c.Rows) != 2 {
		t.Fatalf("rows = %+v", c.Rows)
	}
	x := rowMap(c.Rows[0], cm)
	if x["estimatedUsd"] != "25.00" || x["billedUsd"] != "39.00" {
		t.Errorf("model-x = %v", x)
	}
	if c.Totals[0] != "35.00" || c.Totals[1] != "39.00" {
		t.Errorf("costs totals = %v", c.Totals)
	}
	// A short costs range is the gateway's estimate only.
	s := f.run(t, sess, domain.ReportKindCosts, domain.RawReportQuery{Range: "24h", GroupBy: "agent"})
	if s.Totals[0] != "7.60" || s.Totals[1] != "0.00" {
		t.Errorf("short costs = %v", s.Totals)
	}
	// Model filter and agent filter narrow it.
	m := f.run(t, sess, domain.ReportKindUsage, domain.RawReportQuery{Range: "30d", GroupBy: "agent", ModelID: "model-y", AgentID: f.agt2})
	if len(m.Rows) != 1 || m.Totals[0] != int64(50) {
		t.Errorf("filtered = %+v %v", m.Rows, m.Totals)
	}
	// hour grouping, short range.
	h := f.run(t, sess, domain.ReportKindUsage, domain.RawReportQuery{Range: "24h", GroupBy: "hour"})
	if len(h.Rows) != 5 {
		t.Errorf("hours = %d", len(h.Rows))
	}
}

func TestReportsActivity(t *testing.T) {
	f := newRptFixture(t)
	sess := sessOf(f.a.ID)
	am := domain.ReportMeasures(domain.ReportKindActivity)
	s := f.run(t, sess, domain.ReportKindActivity, domain.RawReportQuery{Range: "24h", GroupBy: "agent", AgentID: f.agt1})
	// 12 windows: 4 working (i%3==0), 8 idle.
	r := rowMap(s.Rows[0], am)
	if r["workingSeconds"] != int64(1200) || r["idleSeconds"] != int64(2400) || r["modelCalls"] != int64(8) || r["utilizationPct"] != 33.3 {
		t.Errorf("short activity = %v", r)
	}
	st := f.run(t, sess, domain.ReportKindActivity, domain.RawReportQuery{Range: "24h", GroupBy: "agent", AgentID: f.agt2})
	r2 := rowMap(st.Rows[0], am)
	if r2["offSeconds"] != int64(300) || r2["impairedSeconds"] != int64(300) || r2["utilizationPct"] != nil {
		t.Errorf("agt2 = %v", r2)
	}
	// Daily view: four days, 3 working and 1 idle window each, plus the short-run windows of today.
	d := f.run(t, sess, domain.ReportKindActivity, domain.RawReportQuery{Range: "30d", GroupBy: "agent", AgentID: f.agt1})
	dr := rowMap(d.Rows[0], am)
	if dr["workingSeconds"] != int64(1200+4*900) || dr["idleSeconds"] != int64(2400+4*300) {
		t.Errorf("daily activity = %v", dr)
	}
	// With a tag the base table is read: same totals.
	dt := f.run(t, sess, domain.ReportKindActivity, domain.RawReportQuery{Range: "30d", GroupBy: "tag:business-unit"})
	tg := map[string]any{}
	for _, row := range dt.Rows {
		tg[row.Group[0]] = rowMap(row, am)["workingSeconds"]
	}
	if tg["retail"] != int64(1200+4*900) {
		t.Errorf("tagged activity = %v", tg)
	}
}

func TestReportsTraffic(t *testing.T) {
	f := newRptFixture(t)
	sess := sessOf(f.a.ID)
	tm := domain.ReportMeasures(domain.ReportKindTraffic)
	s := f.run(t, sess, domain.ReportKindTraffic, domain.RawReportQuery{Range: "24h", GroupBy: "agent"})
	if len(s.Rows) != 2 {
		t.Fatalf("rows %+v", s.Rows)
	}
	a1 := rowMap(s.Rows[0], tm)
	if s.Rows[0].Group[0] != f.agt1 {
		a1 = rowMap(s.Rows[1], tm)
	}
	if a1["connections"] != int64(10) || a1["deniedConnections"] != int64(3) || a1["requests"] != nil || a1["bytesOut"] != int64(130) {
		t.Errorf("agt1 = %v", a1)
	}
	d := f.run(t, sess, domain.ReportKindTraffic, domain.RawReportQuery{Range: "30d", GroupBy: "system"})
	if d.Totals[0] != int64(19) || d.Totals[1] != int64(7) {
		t.Errorf("daily traffic totals = %v", d.Totals)
	}
	dt := f.run(t, sess, domain.ReportKindTraffic, domain.RawReportQuery{Range: "30d", GroupBy: "tag:business-unit"})
	if dt.Totals[0] != int64(19) || dt.Totals[1] != int64(7) {
		t.Errorf("tagged traffic totals = %v", dt.Totals)
	}
	// No cost column on a traffic report.
	for _, m := range tm {
		if m.Name == "costUsd" {
			t.Error("traffic report has a cost")
		}
	}
}

func TestReportsPaging(t *testing.T) {
	f := newRptFixture(t)
	q, _ := domain.ParseReportQuery(domain.ReportKindUsage, domain.RawReportQuery{Range: "30d", GroupBy: "day"}, rptNow)
	var page []domain.ReportRow
	_ = f.runner.InTx(t.Context(), sessOf(f.a.ID), func(ctx context.Context, tx port.Tx) error {
		res, err := f.repo.Run(ctx, tx, q, 3, 4)
		page = res.Rows
		if res.Totals[0] != int64(150) {
			t.Errorf("totals cover every row, got %v", res.Totals)
		}
		return err
	})
	if len(page) != 4 {
		t.Errorf("page = %d", len(page))
	}
}

func TestReportsTeamAndAgentLookups(t *testing.T) {
	f := newRptFixture(t)
	agent := seedAgent(t, f.db, f.a.ID, "rpt-agent")
	_ = f.runner.InTx(t.Context(), sessOf(f.a.ID), func(ctx context.Context, tx port.Tx) error {
		if eph, err := f.repo.TeamIsEphemeral(ctx, tx, f.a.ID); err != nil || eph {
			t.Errorf("persistent team: %v %v", eph, err)
		}
		wantKind(t, f.repo.AgentExists(ctx, tx, f.a.ID, agent), "")
		wantKind(t, f.repo.AgentExists(ctx, tx, f.a.ID, f.agt1), domain.KindNotFound)
		_, err := f.repo.TeamIsEphemeral(ctx, tx, f.b.ID)
		wantKind(t, err, domain.KindNotFound) // RLS hides team B
		return nil
	})
}

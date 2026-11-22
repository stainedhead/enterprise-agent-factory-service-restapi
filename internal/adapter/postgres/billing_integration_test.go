package postgres_test

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur/curtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/reconcile"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

type billingRig struct {
	t      *testing.T
	db     *pgtest.DB
	admin  *pgx.Conn
	store  *postgres.BillingStore
	runner *postgres.TxRunner
	rec    *reconcile.Reconciler
}

func newBillingRig(t *testing.T) *billingRig { return newBillingRigAs(t, "afs_billing") }

// newBillingRigAs runs the store on another role's login (to read what afs_billing may not yet).
func newBillingRigAs(t *testing.T, role string) *billingRig {
	t.Helper()
	db := pgtest.Get(t)
	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs[role], Service: "afs-billing", SystemActor: true, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	runner := postgres.NewTxRunner(pool)
	store := postgres.NewBillingStore(runner)
	admin := db.Admin(t)
	// Other tests of the package expect an outbox and a tag list holding only their own rows.
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM afs.outbox_events WHERE event_type IN ('cost.variance_detected', 'pricing.proposal_created')`)
		_, _ = admin.Exec(context.Background(), `DELETE FROM afs.service_tags WHERE key LIKE 'bill-key-%'`)
	})
	return &billingRig{t: t, db: db, admin: admin, store: store, runner: runner, rec: &reconcile.Reconciler{
		Tx: runner, Outbox: postgres.NewOutboxWriter(), Clock: &fakes.Clock{T: time.Now().UTC()}, IDs: billingIDs{},
		Repo: store, Session: postgres.BillingSession(),
	}}
}

type billingIDs struct{}

func (billingIDs) NewID(p domain.IDPrefix) (domain.ID, error) {
	return domain.NewID(p, time.Now(), rand.Reader)
}

func (g *billingRig) exec(q string, args ...any) {
	g.t.Helper()
	if _, err := g.admin.Exec(context.Background(), q, args...); err != nil {
		g.t.Fatalf("%v\n%s", err, q)
	}
}

func (g *billingRig) scalar(q string, args ...any) string {
	g.t.Helper()
	var s *string
	if err := g.admin.QueryRow(context.Background(), q, args...).Scan(&s); err != nil {
		g.t.Fatalf("%v\n%s", err, q)
	}
	if s == nil {
		return "<null>"
	}
	return *s
}

// afsDay is far from every other test's data, so a day-wide delete cannot touch it.
func afsDay(n int) time.Time { return time.Date(2031, 3, n, 0, 0, 0, 0, time.UTC) }

func TestBillingDirectory(t *testing.T) {
	g := newBillingRig(t)
	team := g.db.SeedTeam(t)
	g.exec(`INSERT INTO afs.service_tags (key, mode, value, description) VALUES ($1, 'fixed', 'v', '') ON CONFLICT DO NOTHING`, "bill-key-"+team.Slug)

	d, err := g.store.Directory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Teams[team.ID] || d.Namespaces["afs-t-"+team.Slug] != team.ID {
		t.Fatalf("team or namespace missing: %v", d.Namespaces)
	}
	if !d.TagKeys["bill-key-"+team.Slug] {
		t.Fatalf("tag keys = %v", d.TagKeys)
	}
	// -db gap X-DB-9: afs_billing has no SELECT on team_models. Until -db grants it the directory
	// degrades instead of failing; when it does, this assertion is the one to flip.
	if !d.ProfilesUnavailable {
		t.Fatal("afs_billing can now read team_models: drop ProfilesUnavailable and the degraded path")
	}
}

func TestBillingDirectoryReadsInferenceProfiles(t *testing.T) {
	g := newBillingRigAs(t, "afs_owner")
	team := g.db.SeedTeam(t)
	model := "bill-model-" + team.Slug
	arn := "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/" + team.Slug
	g.exec(`INSERT INTO afs.models (id, provider, display_name, context_window, max_output_tokens) VALUES ($1, 'Example', 'M', 1000, 100)`, model)
	g.exec(`INSERT INTO afs.team_models (team_id, model_id, inference_profile_arn) VALUES ($1, $2, $3)`, team.ID, model, arn)
	d, err := g.store.Directory(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if d.ProfilesUnavailable || d.Profiles[arn].TeamID != team.ID || d.Profiles[arn].ModelID != model {
		t.Fatalf("profiles = %+v unavailable=%v", d.Profiles[arn], d.ProfilesUnavailable)
	}
}

// BI-1, BI-7: the replaced day, with tag sets built from the line's tags.
func TestReplaceDayIsIdempotentAndBuildsTagSets(t *testing.T) {
	g := newBillingRig(t)
	team := g.db.SeedTeam(t)
	day := afsDay(1)
	rows := []cur.Row{
		{TeamID: team.ID, Source: cur.SourceBedrock, ModelID: "m1", Cost: domain.MustMoney("10.5"), Tags: map[string]string{"business-unit": "payments"}},
		{TeamID: team.ID, Source: cur.SourceBedrock, ModelID: "m1", Cost: domain.MustMoney("5"), Tags: map[string]string{"business-unit": "platform"}},
		{TeamID: team.ID, Source: cur.SourceEKS, PoolID: "pool_1", Cost: domain.MustMoney("2")},
		{TeamID: team.ID, Source: cur.SourceOther, AgentID: "agt_1", Cost: domain.MustMoney("0.75")},
	}
	for range 2 {
		if err := g.store.ReplaceDay(t.Context(), day, rows); err != nil {
			t.Fatal(err)
		}
		if n := g.scalar(`SELECT count(*)::text FROM afs.cost_daily WHERE team_id = $1 AND day = $2`, team.ID, day); n != "4" {
			t.Fatalf("rows = %s", n)
		}
	}
	// Two tag sets in one day, the untagged rows carry none.
	if n := g.scalar(`SELECT count(DISTINCT tag_set_id)::text FROM afs.cost_daily WHERE team_id = $1 AND day = $2`, team.ID, day); n != "2" {
		t.Fatalf("distinct tag sets = %s", n)
	}
	if n := g.scalar(`SELECT count(*)::text FROM afs.cost_daily WHERE team_id = $1 AND day = $2 AND tag_set_id IS NULL`, team.ID, day); n != "2" {
		t.Fatalf("untagged rows = %s", n)
	}
	if v := g.scalar(`SELECT s.tags ->> 'business-unit' FROM afs.cost_daily c JOIN afs.tag_sets s ON s.id = c.tag_set_id
WHERE c.team_id = $1 AND c.day = $2 AND c.cost_usd = 10.5`, team.ID, day); v != "payments" {
		t.Fatalf("tags of the 10.50 row = %s", v)
	}
	// A restated day replaces the rows; gateway rows and other days stay.
	g.exec(`INSERT INTO afs.cost_daily (team_id, day, source, cost_usd) VALUES ($1, $2, 'gateway', 1)`, team.ID, day)
	g.exec(`INSERT INTO afs.cost_daily (team_id, day, source, cost_usd) VALUES ($1, $2, 'cur_bedrock', 9)`, team.ID, afsDay(2))
	if err := g.store.ReplaceDay(t.Context(), day, rows[:1]); err != nil {
		t.Fatal(err)
	}
	if n := g.scalar(`SELECT count(*)::text FROM afs.cost_daily WHERE team_id = $1 AND day = $2 AND source <> 'gateway'`, team.ID, day); n != "1" {
		t.Fatalf("restated rows = %s", n)
	}
	if n := g.scalar(`SELECT count(*)::text FROM afs.cost_daily WHERE team_id = $1 AND source IN ('gateway', 'cur_bedrock') AND day IN ($2, $3)`, team.ID, day, afsDay(2)); n != "3" {
		t.Fatalf("other rows touched: %s", n)
	}
}

func TestReplaceDayRollsBackOnAFailedRow(t *testing.T) {
	g := newBillingRig(t)
	team := g.db.SeedTeam(t)
	day := afsDay(3)
	good := cur.Row{TeamID: team.ID, Source: cur.SourceBedrock, Cost: domain.MustMoney("4")}
	if err := g.store.ReplaceDay(t.Context(), day, []cur.Row{good}); err != nil {
		t.Fatal(err)
	}
	bad := cur.Row{TeamID: "team_0ZZZZZZZZZZZZZZZZZZZZZZZZZ", Source: cur.SourceBedrock, Cost: domain.MustMoney("1")} // no such team
	if err := g.store.ReplaceDay(t.Context(), day, []cur.Row{good, bad}); err == nil {
		t.Fatal("want an error")
	}
	if n := g.scalar(`SELECT sum(cost_usd)::text FROM afs.cost_daily WHERE team_id = $1 AND day = $2`, team.ID, day); n != "4.000000" {
		t.Fatalf("the old day must survive a failed replace: %s", n)
	}
}

// BI-2 and the calls rule: gateway rollup from usage events, reconciliation by the database's status,
// one event per over-threshold team-day.
func TestReconcileAgainstTheDatabase(t *testing.T) {
	g := newBillingRig(t)
	over, fine, unbilled := g.db.SeedTeam(t), g.db.SeedTeam(t), g.db.SeedTeam(t)
	day := afsDay(10)
	at := day.Add(9 * time.Hour)
	g.exec(`SELECT afs.ensure_usage_partition($1::date)`, day)
	ev := func(team pgtest.Team, outcome, cost string, n int) {
		for i := range n {
			g.exec(`INSERT INTO afs.usage_events (occurred_at, team_id, model_id, request_id, outcome, input_tokens, output_tokens, cost_usd)
VALUES ($1, $2, 'm1', $3, $4, 100, 50, $5)`, at.Add(time.Duration(i)*time.Second), team.ID, pgtest.NewID("req"), outcome, cost)
		}
	}
	ev(over, "ok", "10", 10)            // 100.00 estimated
	ev(over, "error", "0", 2)           // calls too
	ev(over, "rejected_budget", "0", 5) // not calls
	ev(fine, "ok", "10", 10)
	ev(unbilled, "ok", "1", 3)
	// An event just outside the day is not part of it.
	g.exec(`INSERT INTO afs.usage_events (occurred_at, team_id, model_id, request_id, outcome, cost_usd) VALUES ($1, $2, 'm1', $3, 'ok', 500)`,
		day.Add(-time.Second), over.ID, pgtest.NewID("req"))
	g.exec(`INSERT INTO afs.cost_daily (team_id, day, source, cost_usd) VALUES ($1, $2, 'cur_bedrock', 80)`, over.ID, day)
	g.exec(`INSERT INTO afs.cost_daily (team_id, day, source, cost_usd) VALUES ($1, $2, 'cur_bedrock', 100)`, fine.ID, day)
	g.exec(`INSERT INTO afs.cost_daily (team_id, day, source, cost_usd) VALUES ($1, $2, 'cur_eks', 40)`, fine.ID, day) // not compared

	from := g.scalar(`SELECT coalesce(max(id), 0)::text FROM afs.outbox_events`)
	for range 2 {
		res, err := g.rec.RunDay(t.Context(), day)
		if err != nil {
			t.Fatal(err)
		}
		if res.Skipped {
			t.Fatal("skipped")
		}
	}
	status := func(team pgtest.Team) string {
		return g.scalar(`SELECT status || ' ' || gateway_usd::text || ' ' || billed_usd::text FROM afs.cost_reconciliations WHERE team_id = $1 AND day = $2`, team.ID, day)
	}
	if s := status(over); s != "over_threshold 100.000000 80.000000" {
		t.Fatalf("over = %q", s)
	}
	if s := status(fine); s != "ok 100.000000 100.000000" {
		t.Fatalf("fine = %q", s)
	}
	if s := status(unbilled); s != "missing_billing 3.000000 0.000000" {
		t.Fatalf("unbilled = %q", s)
	}
	if c := g.scalar(`SELECT calls::text FROM afs.cost_daily WHERE team_id = $1 AND day = $2 AND source = 'gateway'`, over.ID, day); c != "12" {
		t.Fatalf("calls = %s: ok and error count, rejected do not", c)
	}
	if n := g.scalar(`SELECT count(*)::text FROM afs.outbox_events WHERE id > $1::bigint AND event_type = 'cost.variance_detected' AND team_id = $2`, from, over.ID); n != "1" {
		t.Fatalf("variance events for the over team = %s, want exactly 1 across two runs", n)
	}
	if n := g.scalar(`SELECT count(*)::text FROM afs.outbox_events WHERE id > $1::bigint AND event_type = 'cost.variance_detected' AND team_id IN ($2, $3)`, from, fine.ID, unbilled.ID); n != "0" {
		t.Fatalf("events for the others = %s", n)
	}
	if got := g.scalar(`SELECT payload -> 'data' ->> 'billedUsd' FROM afs.outbox_events WHERE id > $1::bigint AND team_id = $2 AND event_type = 'cost.variance_detected'`, from, over.ID); got != "80.00" {
		t.Fatalf("event billedUsd = %s", got)
	}
}

func TestReconcileSkipsADayWithoutCUR(t *testing.T) {
	g := newBillingRig(t)
	team := g.db.SeedTeam(t)
	day := afsDay(11)
	g.exec(`SELECT afs.ensure_usage_partition($1::date)`, day)
	g.exec(`INSERT INTO afs.usage_events (occurred_at, team_id, model_id, request_id, outcome, cost_usd) VALUES ($1, $2, 'm1', $3, 'ok', 5)`,
		day.Add(time.Hour), team.ID, pgtest.NewID("req"))
	res, err := g.rec.RunDay(t.Context(), day)
	if err != nil || !res.Skipped {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if n := g.scalar(`SELECT count(*)::text FROM afs.cost_reconciliations WHERE team_id = $1`, team.ID); n != "0" {
		t.Fatalf("reconciliations = %s", n)
	}
}

// The ingestor and the store together: a fixture fetcher feeding the real tables.
func TestIngestIntoTheDatabase(t *testing.T) {
	g := newBillingRig(t)
	team := g.db.SeedTeam(t)
	day := afsDay(12)
	f := &curtest.Fetcher{Lines: map[string][]cur.Line{day.Format(time.DateOnly): {
		{Kind: cur.KindPod, Resource: "afs-t-" + team.Slug, Cost: domain.MustMoney("3.5"), Tags: map[string]string{cur.TagPoolID: "pool_9"}},
		{Kind: cur.KindStorage, Cost: domain.MustMoney("1"), Tags: map[string]string{cur.TagTeamID: team.ID}},
		{Kind: cur.KindPod, Resource: "kube-system", Cost: domain.MustMoney("8")},
	}}}
	in := &cur.Ingestor{Fetch: f, Store: g.store}
	res := in.Run(t.Context(), []time.Time{day})
	if res.Err() != nil || res.Days[0].Rows != 2 || res.Days[0].Unattributed.String() != "8.00" {
		t.Fatalf("result = %+v", res)
	}
	if got := g.scalar(`SELECT string_agg(source || ':' || coalesce(pool_id, '-') || ':' || cost_usd::text, ',' ORDER BY source) FROM afs.cost_daily WHERE team_id = $1 AND day = $2`, team.ID, day); got != "cur_eks:pool_9:3.500000,cur_other:-:1.000000" {
		t.Fatalf("rows = %s", got)
	}
}

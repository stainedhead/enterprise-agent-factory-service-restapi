package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

type gwWorld struct {
	db      *pgtest.DB
	pool    *pgxpool.Pool
	team    string
	budget  string
	model   string
	profile string
	period  time.Time
}

func gatewayPool(t *testing.T, db *pgtest.DB) *pgxpool.Pool {
	t.Helper()
	p, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_gateway"], Service: "afs-gateway", SystemActor: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func seedGateway(t *testing.T, amount string) gwWorld {
	t.Helper()
	db := pgtest.Get(t)
	w := gwWorld{db: db, pool: gatewayPool(t, db), team: pgtest.NewID("team"), budget: pgtest.NewID("bud"),
		model: "test.model-" + strings.ToLower(pgtest.NewID("m")[4:12]) + ":0", profile: "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/p1"}
	w.period = time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	adm := db.Admin(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := adm.Exec(t.Context(), sql, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, sql)
		}
	}
	slug := "g" + strings.ToLower(w.team[len(w.team)-8:])
	exec(`INSERT INTO afs.teams (id, slug, display_name, cost_center, owner_email, max_budget_usd) VALUES ($1, $2, $2, 'CC1', 'o@example.com', 100000)`, w.team, slug)
	exec(`INSERT INTO afs.budgets (id, team_id, period, amount_usd, thresholds) VALUES ($1, $2, 'monthly', $3::numeric, '{50,80,100}')`, w.budget, w.team, amount)
	exec(`INSERT INTO afs.models (id, provider, display_name, max_output_tokens, context_window) VALUES ($1, 'test', 'Test', 4096, 200000)`, w.model)
	exec(`INSERT INTO afs.model_prices (model_id, effective_from, input_usd_per_mtok, output_usd_per_mtok, cache_read_usd_per_mtok, cache_write_usd_per_mtok)
VALUES ($1, now() - interval '2 days', 1, 5, 0, 0), ($1, now() - interval '1 hour', 3, 15, 0.3, 3.75), ($1, now() + interval '1 day', 99, 99, 0, 0)`, w.model)
	exec(`INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, $2)`, w.team, w.model)
	exec(`UPDATE afs.team_models SET inference_profile_arn = $3 WHERE team_id = $1 AND model_id = $2`, w.team, w.model, w.profile)
	// Other tests read the outbox as a whole, so what the gateway tests publish is removed afterwards.
	t.Cleanup(func() {
		_, _ = adm.Exec(context.Background(), `DELETE FROM afs.outbox_events WHERE team_id = $1`, w.team)
	})
	return w
}

func TestGatewayStoreReads(t *testing.T) {
	w := seedGateway(t, "10")
	st := postgres.NewGatewayStore(w.pool)
	ctx := t.Context()

	tm, err := st.Team(ctx, w.team)
	if err != nil || tm.State != "active" || !strings.HasPrefix(tm.Namespace, "afs-t-g") {
		t.Fatalf("team = %+v %v", tm, err)
	}
	if _, err := st.Team(ctx, pgtest.NewID("team")); !errors.Is(err, gateway.ErrNotFound) {
		t.Fatalf("missing team err = %v", err)
	}
	m, err := st.TeamModel(ctx, w.team, w.model)
	if err != nil || !m.Enabled || m.ProfileARN != w.profile || m.MaxOutputTokens != 4096 {
		t.Fatalf("model = %+v %v", m, err)
	}
	other := pgtest.NewID("team")
	if _, err := st.TeamModel(ctx, other, w.model); !errors.Is(err, gateway.ErrNotFound) {
		t.Fatalf("another team's model err = %v", err)
	}
	prices, err := st.Prices(ctx, w.model)
	if err != nil || len(prices) != 3 || prices[0].Input.String() != "99.00" || prices[1].Output.String() != "15.00" || prices[1].CacheWrite.String() != "3.75" {
		t.Fatalf("prices = %+v %v", prices, err)
	}
	if p, ok := gateway.EffectivePrice(prices, time.Now()); !ok || p.Input.String() != "3.00" {
		t.Fatalf("effective = %+v", p)
	}
	b, err := st.Budget(ctx, w.team)
	if err != nil || b.ID != w.budget || b.Period != "monthly" || b.OnExhaust != "block" || len(b.Thresholds) != 3 || b.Amount.String() != "10.00" {
		t.Fatalf("budget = %+v %v", b, err)
	}
	if _, err := st.Budget(ctx, other); !errors.Is(err, gateway.ErrNotFound) {
		t.Fatalf("no budget err = %v", err)
	}
	if _, err := w.db.Admin(t).Exec(ctx, `INSERT INTO afs.team_tags (team_id, key, value) VALUES ($1, 'project', 'apollo')`, w.team); err != nil {
		t.Fatal(err)
	}
	svcKey := "sv-" + strings.ToLower(w.team[len(w.team)-8:])
	adm := w.db.Admin(t)
	if _, err := adm.Exec(ctx, `INSERT INTO afs.service_tags (key, mode, default_value) VALUES ($1, 'required', 'dflt')`, svcKey); err != nil {
		t.Fatal(err)
	}
	// service_tags is global: leave it as found for the tests that share the database.
	t.Cleanup(func() {
		_, _ = adm.Exec(context.WithoutCancel(ctx), `DELETE FROM afs.service_tags WHERE key = $1`, svcKey)
	})
	tags, err := st.Tags(ctx, w.team)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tg := range tags {
		got[tg.Key] = tg.Value
		if tg.Service != (tg.Key == svcKey) {
			t.Errorf("tag %s: Service = %v", tg.Key, tg.Service)
		}
	}
	if got["project"] != "apollo" || got["afs-team-id"] != w.team {
		t.Fatalf("tags = %v", got)
	}
	ts1, err := st.TagSetID(ctx, w.team)
	if err != nil || !strings.HasPrefix(ts1, "ts_") {
		t.Fatalf("tag set = %q %v", ts1, err)
	}
	if _, err := w.db.Admin(t).Exec(ctx, `UPDATE afs.team_tags SET value = 'gemini' WHERE team_id = $1 AND key = 'project'`, w.team); err != nil {
		t.Fatal(err)
	}
	ts2, _ := st.TagSetID(ctx, w.team)
	if ts2 == ts1 {
		t.Fatal("a tag change must produce a new tag set")
	}
}

func counterOf(t *testing.T, w gwWorld) (spent, reserved string) {
	t.Helper()
	if err := w.db.Admin(t).QueryRow(t.Context(), `SELECT spent_usd::text, reserved_usd::text FROM afs.spend_counters WHERE budget_id = $1 AND period_start = $2`,
		w.budget, w.period).Scan(&spent, &reserved); err != nil {
		t.Fatal(err)
	}
	return
}

func TestSpendReserveSettle(t *testing.T) {
	w := seedGateway(t, "1.00")
	sp := postgres.NewSpendStore(w.pool)
	ctx := t.Context()
	if err := sp.EnsureCounter(ctx, w.budget, w.period); err != nil {
		t.Fatal(err)
	}
	if err := sp.EnsureCounter(ctx, w.budget, w.period); err != nil {
		t.Fatalf("ensure must be idempotent: %v", err)
	}
	h, err := sp.Reserve(ctx, w.budget, w.period, domain.MustMoney("0.6"), "agent-bot-0")
	if err != nil || h.ID == "" {
		t.Fatalf("reserve = %+v %v", h, err)
	}
	if _, r := counterOf(t, w); r != "0.600000" {
		t.Fatalf("reserved = %s", r)
	}
	var holder string
	if err := w.db.Admin(t).QueryRow(ctx, `SELECT holder FROM afs.spend_reservations WHERE id = $1::uuid`, h.ID).Scan(&holder); err != nil || holder != "agent-bot-0" {
		t.Fatalf("holder = %q %v", holder, err)
	}
	refused, err := sp.Reserve(ctx, w.budget, w.period, domain.MustMoney("0.5"), "agent-bot-1")
	if err != nil || refused.ID != "" || refused.Limit.String() != "1.00" || refused.Reserved.String() != "0.60" || refused.Spent.Micros() != 0 {
		t.Fatalf("refusal = %+v %v", refused, err)
	}
	c, err := sp.Settle(ctx, h.ID, w.budget, w.period, domain.MustMoney("0.25"))
	if err != nil || c.Spent.String() != "0.25" || c.Reserved.Micros() != 0 {
		t.Fatalf("settle = %+v %v", c, err)
	}
	// A second settlement of the same hold still adds the actual cost (a call that outlived the TTL).
	c, err = sp.Settle(ctx, h.ID, w.budget, w.period, domain.MustMoney("0.05"))
	if err != nil || c.Spent.String() != "0.30" {
		t.Fatalf("late settle = %+v %v", c, err)
	}
	// An empty hold id adds the cost without a hold.
	c, err = sp.Settle(ctx, "", w.budget, w.period, domain.MustMoney("0.10"))
	if err != nil || c.Spent.String() != "0.40" {
		t.Fatalf("holdless settle = %+v %v", c, err)
	}
	// A budget or period that does not match the hold is a gateway bug the database refuses.
	h2, _ := sp.Reserve(ctx, w.budget, w.period, domain.MustMoney("0.1"), "p")
	if _, err := sp.Settle(ctx, h2.ID, w.budget, w.period.AddDate(0, -1, 0), domain.MustMoney("0.1")); err == nil {
		t.Fatal("settling against another period must fail")
	}
}

func TestThresholdRecordInsertsOnceAndPublishesOnce(t *testing.T) {
	w := seedGateway(t, "10")
	runner := postgres.NewTxRunner(w.pool)
	ts := postgres.NewThresholdStore(runner, &fakes.IDGen{}, &fakes.Clock{T: time.Now()})
	var wg sync.WaitGroup
	var mu sync.Mutex
	firsts := 0
	for range 50 {
		wg.Go(func() {
			first, err := ts.Record(context.Background(), gateway.Crossing{TeamID: w.team, BudgetID: w.budget, Period: w.period, Threshold: 80,
				Event: domain.EventBudgetThresholdCrossed, Data: map[string]any{"threshold": 80}})
			if err != nil {
				t.Error(err)
				return
			}
			if first {
				mu.Lock()
				firsts++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if firsts != 1 {
		t.Fatalf("first = %d, want exactly 1", firsts)
	}
	var n int
	if err := w.db.Admin(t).QueryRow(t.Context(), `SELECT count(*) FROM afs.outbox_events WHERE event_type = 'budget.threshold_crossed' AND aggregate_id = $1`, w.budget).Scan(&n); err != nil || n != 1 {
		t.Fatalf("outbox rows = %d %v", n, err)
	}
	first, err := ts.Record(t.Context(), gateway.Crossing{TeamID: w.team, BudgetID: w.budget, Period: w.period, Threshold: gateway.ExhaustedMarker,
		Event: domain.EventBudgetExhausted, Data: map[string]any{}})
	if err != nil || !first {
		t.Fatalf("exhausted marker = %v %v", first, err)
	}
}

func TestUsageWriterBatchesWithinASecond(t *testing.T) {
	w := seedGateway(t, "10")
	st := postgres.NewGatewayStore(w.pool)
	tagSet, err := st.TagSetID(t.Context(), w.team)
	if err != nil {
		t.Fatal(err)
	}
	uw := postgres.NewUsageWriter(w.pool, nil, postgres.UsageWriterConfig{})
	uw.Run()
	defer uw.Close()
	now := time.Now().UTC()
	uw.Record(gateway.Record{OccurredAt: now, TeamID: w.team, AgentID: "agt_01JD0000000000000000000A00", ModelID: w.model, RequestID: "r1", Outcome: gateway.OutcomeOK,
		Usage: gateway.Usage{Input: 10, Output: 20, CacheRead: 3, CacheWrite: 4}, Cost: domain.MustMoney("0.000123"), LatencyMS: 42, TagSetID: tagSet})
	uw.Record(gateway.Record{OccurredAt: now, TeamID: w.team, PoolID: "pool_01JD0000000000000000000A00", WorkerPod: "pool-w-7f9", ModelID: w.model, RequestID: "r2",
		Outcome: gateway.OutcomeRejectedBudget, LatencyMS: 3})
	deadline := time.Now().Add(1500 * time.Millisecond)
	for uw.Written() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if uw.Written() != 2 {
		t.Fatalf("written = %d within 1.5 s, failed %d", uw.Written(), uw.Failed())
	}
	type row struct {
		agent, pool, pod, tag *string
		outcome               string
		in, out, cr, cw       int
		cost                  string
		lat                   int
	}
	read := func(req string) row {
		var r row
		if err := w.db.Admin(t).QueryRow(t.Context(), `SELECT agent_id, pool_id, worker_pod, tag_set_id, outcome, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd::text, latency_ms
FROM afs.usage_events WHERE team_id = $1 AND request_id = $2`, w.team, req).Scan(&r.agent, &r.pool, &r.pod, &r.tag, &r.outcome, &r.in, &r.out, &r.cr, &r.cw, &r.cost, &r.lat); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r1, r2 := read("r1"), read("r2")
	if r1.agent == nil || *r1.agent != "agt_01JD0000000000000000000A00" || r1.pool != nil || r1.pod != nil || r1.tag == nil || *r1.tag != tagSet ||
		r1.in != 10 || r1.out != 20 || r1.cr != 3 || r1.cw != 4 || r1.cost != "0.000123" || r1.lat != 42 {
		t.Fatalf("agent row = %+v", r1)
	}
	if r2.agent != nil || r2.pool == nil || r2.pod == nil || *r2.pod != "pool-w-7f9" || r2.outcome != "rejected_budget" || r2.cost != "0.000000" || r2.tag != nil {
		t.Fatalf("pool row = %+v", r2)
	}
}

func TestUsageWriterDropsOnAFullQueueWithoutBlocking(t *testing.T) {
	w := seedGateway(t, "10")
	uw := postgres.NewUsageWriter(w.pool, nil, postgres.UsageWriterConfig{Queue: 1})
	start := time.Now()
	for range 100 {
		uw.Record(gateway.Record{OccurredAt: time.Now(), TeamID: w.team, ModelID: w.model, RequestID: "x", Outcome: gateway.OutcomeOK})
	}
	if time.Since(start) > 100*time.Millisecond || uw.Dropped() != 99 {
		t.Fatalf("took %v dropped %d", time.Since(start), uw.Dropped())
	}
}

func TestServiceAgainstTheRealDatabase(t *testing.T) {
	w := seedGateway(t, "0.50")
	st := gateway.NewCachedStore(postgres.NewGatewayStore(w.pool), time.Minute, nil)
	runner := postgres.NewTxRunner(w.pool)
	thr := postgres.NewThresholdStore(runner, &fakes.IDGen{}, &fakes.Clock{T: time.Now()})
	uw := postgres.NewUsageWriter(w.pool, nil, postgres.UsageWriterConfig{FlushEvery: 100 * time.Millisecond})
	uw.Run()
	defer uw.Close()
	dir := staticDir{team: w.team}
	svc := gateway.NewService(gateway.Deps{Pods: dir, Store: st, Spend: postgres.NewSpendStore(w.pool), Thresholds: thr, Usage: uw},
		gateway.Config{Partition: "aws", AccountID: "111122223333"})
	var mu sync.Mutex
	var okCalls, refused int
	var wg sync.WaitGroup
	for range 60 {
		wg.Go(func() {
			err := svc.Do(context.Background(), dir.request(w.model), func(context.Context, gateway.Authorized) (gateway.Result, error) {
				// 0.05 USD of output at 15 USD per Mtok is 3334 tokens; the reservation is 4096 tokens: 0.06144.
				return gateway.Result{Usage: gateway.Usage{Output: 3334}, UsageReported: true, Started: true}, nil
			})
			mu.Lock()
			defer mu.Unlock()
			switch domain.KindOf(err) {
			case "":
				okCalls++
			case domain.KindBudgetExceeded:
				refused++
			default:
				t.Errorf("unexpected error %v", err)
			}
		})
	}
	wg.Wait()
	svc.Wait()
	// 0.50 / 0.06144 reserved per call: 8 calls fit at once; later ones fit as earlier ones settle at 0.05.
	spent, reserved := counterOf(t, w)
	if reserved != "0.000000" {
		t.Fatalf("reserved after all calls = %s", reserved)
	}
	if okCalls+refused != 60 || okCalls < 8 || refused < 1 {
		t.Fatalf("ok %d refused %d", okCalls, refused)
	}
	var s domain.Money
	s, _ = domain.ParseMoney("spent", spent)
	if s.Cmp(domain.MustMoney("0.50")) > 0 {
		t.Fatalf("spent %s exceeds the 0.50 budget", spent)
	}
	var n int
	_ = w.db.Admin(t).QueryRow(t.Context(), `SELECT count(*) FROM afs.budget_threshold_events WHERE budget_id = $1 AND threshold = 101`, w.budget).Scan(&n)
	if n != 1 {
		t.Fatalf("exhausted markers = %d", n)
	}
	_ = w.db.Admin(t).QueryRow(t.Context(), `SELECT count(*) FROM afs.outbox_events WHERE event_type = 'budget.exhausted' AND aggregate_id = $1`, w.budget).Scan(&n)
	if n != 1 {
		t.Fatalf("budget.exhausted events = %d, want 1", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for uw.Written() < int64(okCalls+refused) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	var rej, oks int
	_ = w.db.Admin(t).QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE outcome = 'rejected_budget'), count(*) FILTER (WHERE outcome = 'ok') FROM afs.usage_events WHERE team_id = $1`, w.team).Scan(&rej, &oks)
	if rej != refused || oks != okCalls {
		t.Fatalf("usage rows: rejected %d ok %d, want %d and %d", rej, oks, refused, okCalls)
	}
}

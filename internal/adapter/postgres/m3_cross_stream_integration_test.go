package postgres_test

import (
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/activity"
)

// M3-X1: the gateway and the budget reconciler both touch a budget's spend counter and both can speak of
// exhaustion. Whichever opens the counter first, the period is announced once; the gateway alone
// announces exhaustion, once, and the reconciler's backstop (condition and IAM deny) adds no event.
func TestGatewayAndBudgetReconcilerShareACounterAndEmitEachEventOnce(t *testing.T) {
	g := newBudgetRig(t)
	w := seedGateway(t, "10")
	ctx := t.Context()
	sp := postgres.NewSpendStore(w.pool)
	events := func(typ string) int {
		return g.count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = $1 AND team_id = $2`, typ, w.team)
	}
	counters := func() int {
		return g.count(`SELECT count(*) FROM afs.spend_counters WHERE budget_id = $1 AND period_start = $2`, w.budget, w.period)
	}

	// The gateway's first call opens the counter and reserves; the reconciler then finds it open.
	if err := sp.EnsureCounter(ctx, w.budget, w.period); err != nil {
		t.Fatal(err)
	}
	h, err := sp.Reserve(ctx, w.budget, w.period, domain.MustMoney("1"), "agent-0")
	if err != nil || h.ID == "" {
		t.Fatalf("reserve = %+v %v", h, err)
	}
	g.now = w.period.Add(time.Hour)
	g.reconcile(g.r)
	g.reconcile(g.leader())
	if counters() != 1 || events("budget.reset") != 1 {
		t.Fatalf("counters %d, budget.reset %d; want one each", counters(), events("budget.reset"))
	}
	if r := g.text(`SELECT reserved_usd::text FROM afs.spend_counters WHERE budget_id = $1 AND period_start = $2`, w.budget, w.period); r != "1.000000" {
		t.Fatalf("the reconciler disturbed the gateway's reservation: reserved %s", r)
	}
	// The gateway asking again after the reconciler is a no-op on the same counter.
	if err := sp.EnsureCounter(ctx, w.budget, w.period); err != nil || counters() != 1 {
		t.Fatalf("ensure after the reconciler: %v, counters %d", err, counters())
	}

	// Exhaustion: the gateway records marker 101 once; the reconciler's backstop marks and denies but
	// publishes no budget.exhausted of its own, however many passes run.
	if _, err := sp.Settle(ctx, h.ID, w.budget, w.period, domain.MustMoney("10")); err != nil {
		t.Fatal(err)
	}
	g.exec(`INSERT INTO afs.resource_status (resource_kind, resource_id, team_id, phase) VALUES ('team', $1, $1, 'Ready') ON CONFLICT (resource_kind, resource_id) DO NOTHING`, w.team)
	ts := postgres.NewThresholdStore(postgres.NewTxRunner(w.pool), &fakes.IDGen{}, &fakes.Clock{T: time.Now()})
	for range 3 {
		if _, err := ts.Record(ctx, gateway.Crossing{TeamID: w.team, BudgetID: w.budget, Period: w.period, Threshold: gateway.ExhaustedMarker,
			Event: domain.EventBudgetExhausted, Data: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		g.reconcile(g.r)
	}
	g.reconcile(g.leader())
	if n := events("budget.exhausted"); n != 1 {
		t.Fatalf("budget.exhausted published %d times, want 1", n)
	}
	if !g.iam.Denied[g.rolePath(w.team)] {
		t.Fatal("the backstop did not deny the team role")
	}
	if events("budget.reset") != 1 {
		t.Fatalf("budget.reset = %d after the backstop passes", events("budget.reset"))
	}
	// The reconciler can also be first: a second team whose counter the reconciler opened.
	w2 := seedGateway(t, "10")
	g.reconcile(g.r)
	if err := postgres.NewSpendStore(w2.pool).EnsureCounter(ctx, w2.budget, w2.period); err != nil {
		t.Fatal(err)
	}
	g.reconcile(g.leader())
	if n := g.count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'budget.reset' AND team_id = $1`, w2.team); n != 1 {
		t.Fatalf("reconciler first: budget.reset %d, want 1", n)
	}
	if n := g.count(`SELECT count(*) FROM afs.spend_counters WHERE budget_id = $1`, w2.budget); n != 1 {
		t.Fatalf("reconciler first: %d counters", n)
	}
}

// M3-X1: the usage rows the gateway writes carry what reports (tag sets) and the activity collector
// (agent, pool, worker pod) read, and both count a call when its outcome is ok or error.
func TestGatewayUsageRowsFeedReportsAndTheActivityCollector(t *testing.T) {
	w := seedGateway(t, "10")
	st := postgres.NewGatewayStore(w.pool)
	tagSet, err := st.TagSetID(t.Context(), w.team)
	if err != nil {
		t.Fatal(err)
	}
	uw := postgres.NewUsageWriter(w.pool, nil, postgres.UsageWriterConfig{FlushEvery: 50 * time.Millisecond})
	uw.Run()
	at := time.Now().UTC().Truncate(5 * time.Minute).Add(time.Minute)
	agent, pool := pgtest.NewID("agt"), pgtest.NewID("pool")
	rec := func(req string, o gateway.Outcome, a, p, pod string) {
		uw.Record(gateway.Record{OccurredAt: at, TeamID: w.team, AgentID: a, PoolID: p, WorkerPod: pod, ModelID: w.model, RequestID: req,
			Outcome: o, Cost: domain.MustMoney("0.01"), TagSetID: tagSet})
	}
	rec("a-ok", gateway.OutcomeOK, agent, "", "")
	rec("a-err", gateway.OutcomeError, agent, "", "")
	rec("a-rej", gateway.OutcomeRejectedBudget, agent, "", "")
	rec("p-ok", gateway.OutcomeOK, "", pool, "pool-w-1")
	rec("p-model", gateway.OutcomeRejectedModel, "", pool, "pool-w-1")
	uw.Close()
	if uw.Written() != 5 {
		t.Fatalf("written %d, failed %d", uw.Written(), uw.Failed())
	}
	var missing int
	if err := w.db.Admin(t).QueryRow(t.Context(), `SELECT count(*) FROM afs.usage_events WHERE team_id = $1 AND tag_set_id IS DISTINCT FROM $2`, w.team, tagSet).Scan(&missing); err != nil || missing != 0 {
		t.Fatalf("rows without the team's tag set: %d %v", missing, err)
	}
	runner, _ := reconcilerRunner(t, w.db)
	got, err := postgres.NewActivityUsage(runner).Window(t.Context(), at.Truncate(5*time.Minute), at.Truncate(5*time.Minute).Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if u := got[activity.UsageKey{AgentID: agent}]; u.Calls != 2 {
		t.Errorf("agent calls = %d, want 2 (ok and error, not rejected_budget)", u.Calls)
	}
	if u := got[activity.UsageKey{PoolID: pool, WorkerPod: "pool-w-1"}]; u.Calls != 1 {
		t.Errorf("pool worker calls = %d, want 1 (rejected_model is not a call)", u.Calls)
	}
}

// rolePath is the team's Bedrock role as the backstop names it (the key of the fake IAM's deny set).
func (g *budgetRig) rolePath(teamID string) string {
	return "/afs/" + g.text(`SELECT namespace FROM afs.teams WHERE id = $1`, teamID) + "/afs-" + teamID[len("team_"):] + "-bedrock"
}

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/activity"
)

// M3-I1: the roster, the metered usage of a window, the threshold and the sample store, as the reconciler.
func TestActivityRepos(t *testing.T) {
	db := pgtest.Get(t)
	runner, _ := reconcilerRunner(t, db)
	admin := db.Admin(t)
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	ctx := t.Context()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	enabled, paused, inB := seedAgent(t, db, a.ID, "act-on"), seedAgent(t, db, a.ID, "act-paused"), seedAgent(t, db, b.ID, "act-b")
	exec(`UPDATE afs.agents SET state = 'paused' WHERE id = $1`, paused)
	// Team B is budget blocked.
	exec(`INSERT INTO afs.resource_status (resource_kind, resource_id, team_id, phase, conditions) VALUES ('team', $1, $1, 'Ready', '[{"type":"BudgetExhausted","status":"True","lastTransitionTime":"2026-11-22T00:00:00Z"}]')
ON CONFLICT (resource_kind, resource_id) DO UPDATE SET conditions = EXCLUDED.conditions`, b.ID)

	roster, err := postgres.NewActivityRoster(runner).Roster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]activity.Agent{}
	for _, x := range roster.Agents {
		got[x.AgentID] = x
	}
	if x := got[enabled]; x.TeamID != a.ID || x.Namespace == "" || x.Paused || x.BudgetBlocked {
		t.Errorf("enabled = %+v", x)
	}
	if !got[paused].Paused || got[paused].BudgetBlocked {
		t.Errorf("paused = %+v", got[paused])
	}
	if x := got[inB]; !x.BudgetBlocked || x.Paused {
		t.Errorf("blocked = %+v", x)
	}
	// A suspended team's agents are paused; a deleted agent and its team's agents are left out.
	// The database is shared by the package's tests: put the team back afterwards.
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `UPDATE afs.teams SET state = 'active' WHERE id = $1`, a.ID)
		_, _ = admin.Exec(context.Background(), `DELETE FROM afs.resource_status WHERE resource_kind = 'team' AND resource_id = $1`, b.ID)
	})
	exec(`UPDATE afs.teams SET state = 'suspended' WHERE id = $1`, a.ID)
	exec(`UPDATE afs.agents SET state = 'deleting' WHERE id = $1`, paused)
	roster, _ = postgres.NewActivityRoster(runner).Roster(ctx)
	got = map[string]activity.Agent{}
	for _, x := range roster.Agents {
		got[x.AgentID] = x
	}
	if !got[enabled].Paused {
		t.Errorf("suspended team: %+v", got[enabled])
	}
	if _, ok := got[paused]; ok {
		t.Error("a deleting agent is not sampled")
	}

	// Usage of a window: ok and error calls count, rejected ones do not; pool workers are keyed by pod.
	w := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	ev := func(team, agent, pool, worker any, outcome string, at time.Time, in int, cost string) {
		exec(`INSERT INTO afs.usage_events (occurred_at, team_id, agent_id, pool_id, worker_pod, model_id, request_id, outcome, input_tokens, output_tokens, cost_usd)
VALUES ($1, $2, $3, $4, $5, 'm', $6, $7, $8, 1, $9::numeric)`, at, team, agent, pool, worker, pgtest.NewID("req"), outcome, in, cost)
	}
	ev(a.ID, enabled, nil, nil, "ok", w, 10, "0.5")
	ev(a.ID, enabled, nil, nil, "error", w.Add(4*time.Minute), 5, "0.25")
	ev(a.ID, enabled, nil, nil, "rejected_budget", w.Add(time.Minute), 100, "0")
	ev(a.ID, enabled, nil, nil, "ok", w.Add(5*time.Minute), 100, "9") // next window
	poolID := pgtest.NewID("pool")
	ev(a.ID, nil, poolID, "worker-1", "ok", w.Add(time.Minute), 7, "1.5")
	usage, err := postgres.NewActivityUsage(runner).Window(ctx, w, w.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if u := usage[activity.UsageKey{AgentID: enabled}]; u.Calls != 2 || u.InputTokens != 15 || u.OutputTokens != 2 || u.Cost.String() != "0.75" {
		t.Errorf("agent usage = %+v", u)
	}
	if u := usage[activity.UsageKey{PoolID: poolID, WorkerPod: "worker-1"}]; u.Calls != 1 || u.Cost.String() != "1.50" {
		t.Errorf("worker usage = %+v", u)
	}

	// The threshold is the setting.
	if n, err := postgres.NewActivityThreshold(runner).IdleCPUMillicores(ctx); err != nil || n != 50 {
		t.Errorf("threshold = %d %v", n, err)
	}

	// Samples are stamped with the team's tag set and stored once.
	var ts string
	if err := admin.QueryRow(ctx, `SELECT afs.tag_set_for($1, '{"business-unit":"retail"}'::jsonb)`, a.ID).Scan(&ts); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO afs.team_tags (team_id, key, value) VALUES ($1, 'business-unit', 'retail')`, a.ID)
	cpu, mem := 120, 256
	store := postgres.NewActivityStore(runner)
	samples := []activity.Sample{
		{BucketStart: w, TeamID: a.ID, AgentID: enabled, State: activity.StateWorking, ActiveSeconds: 90, ModelCalls: 2, InputTokens: 15, OutputTokens: 2,
			Cost: domain.MustMoney("0.75"), CPUAvg: &cpu, MemAvg: &mem, Note: "reviewing PR 482"},
		{BucketStart: w, TeamID: a.ID, PoolID: poolID, WorkerPod: "worker-1", State: activity.StateIdle},
		{BucketStart: w, TeamID: b.ID, AgentID: inB, State: activity.StateBlockedBudget},
	}
	if err := store.Add(ctx, samples); err != nil {
		t.Fatal(err)
	}
	again := append([]activity.Sample(nil), samples...)
	again[0].ModelCalls = 99
	if err := store.Add(ctx, again); err != nil {
		t.Fatal(err)
	}
	var n, calls int
	var tag *string
	var note string
	if err := admin.QueryRow(ctx, `SELECT count(*), max(model_calls), max(tag_set_id), max(activity_note) FROM afs.agent_activity WHERE bucket_start = $1 AND team_id = $2`, w, a.ID).Scan(&n, &calls, &tag, &note); err != nil {
		t.Fatal(err)
	}
	if n != 2 || calls != 2 || tag == nil || *tag != ts || note != "reviewing PR 482" {
		t.Errorf("stored: %d rows, calls %d, tag %v, note %q", n, calls, tag, note)
	}
	var state string
	if err := admin.QueryRow(ctx, `SELECT state FROM afs.agent_activity WHERE agent_id = $1`, inB).Scan(&state); err != nil || state != "blocked_budget" {
		t.Errorf("team B sample: %v %v", state, err)
	}
	if err := store.Add(ctx, nil); err != nil {
		t.Error(err)
	}
}

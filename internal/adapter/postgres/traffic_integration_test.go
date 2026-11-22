package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
)

// seedSwarm creates an ephemeral team (pools run only in those) with one pool.
func seedSwarm(t *testing.T, db *pgtest.DB) (team, pool string) {
	t.Helper()
	team = pgtest.NewID("team")
	tx, err := db.Admin(t).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// The lifetime budget is checked at commit, so team and budget go in one transaction.
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO afs.teams (id, slug, display_name, cost_center, owner_email, max_budget_usd, kind, expires_at, teardown_rule)
VALUES ($1, $2, $2, 'CC100', 'owner@example.com', 100, 'ephemeral', now() + interval '10 days', 'at_cap_or_expiry')`, []any{team, "s" + strings.ToLower(team[len(team)-8:])}},
		{`INSERT INTO afs.budgets (id, team_id, period, amount_usd, on_exhaust) VALUES ($1, $2, 'lifetime', 50, 'block')`, []any{pgtest.NewID("bud"), team}},
		{`INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, 'claude-sonnet')`, []any{team}},
	} {
		if _, err := tx.Exec(t.Context(), q.sql, q.args...); err != nil {
			_ = tx.Rollback(t.Context())
			t.Fatalf("seed swarm team: %v", err)
		}
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("seed swarm team: %v", err)
	}
	id := pgtest.NewID("pool")
	if _, err := db.Admin(t).Exec(t.Context(), `INSERT INTO afs.agent_pools (id, team_id, name, template, image, model_id)
VALUES ($1, $2, 'review', 'omp', 'ghcr.io/x/omp@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'claude-sonnet')`, id, team); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	return team, id
}

func namespaceOf(t *testing.T, db *pgtest.DB, teamID string) string {
	t.Helper()
	var ns string
	if err := db.Admin(t).QueryRow(t.Context(), `SELECT namespace FROM afs.teams WHERE id = $1`, teamID).Scan(&ns); err != nil {
		t.Fatal(err)
	}
	return ns
}

// M2-H2: rows are created, then their counters grow; a row is stamped with the team's tag set; agent
// and pool-worker rows are separate keys; the reconciler role is allowed to do all of it.
func TestTrafficStore(t *testing.T) {
	db := pgtest.Get(t)
	runner := reconcilerPool(t, db)
	store := postgres.NewTrafficStore(runner)
	w := seedEgress(t, db)
	swarm, poolID := seedSwarm(t, db)
	admin := db.Admin(t)
	bucket := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

	agent := traffic.Row{BucketStart: bucket, TeamID: w.team.ID, AgentID: w.agtA, SystemID: w.sysGH, Host: "api.github.com", Port: 443, Verdict: traffic.VerdictAllowed, Connections: 3}
	denied := traffic.Row{BucketStart: bucket, TeamID: w.team.ID, AgentID: w.agtA, Host: "evil.example.net", Port: 443, Verdict: traffic.VerdictDenied, DeniedReason: traffic.ReasonNotInCat, Connections: 2}
	worker := traffic.Row{BucketStart: bucket, TeamID: swarm, PoolID: poolID, WorkerPod: "pool-review-7d9f-x2k4q", Host: "slack.com", Port: 443, Verdict: traffic.VerdictDenied, DeniedReason: traffic.ReasonOffPool, Connections: 1}
	if err := store.Add(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := store.Add(t.Context(), []traffic.Row{agent, denied, worker}); err != nil {
		t.Fatal(err)
	}
	agent.Connections = 4
	if err := store.Add(t.Context(), []traffic.Row{agent, worker}); err != nil {
		t.Fatal(err)
	}

	type row struct {
		agent, pool, worker, system, reason, tag *string
		conns                                    int
	}
	read := func(host string) row {
		var r row
		if err := admin.QueryRow(t.Context(), `SELECT agent_id, pool_id, worker_pod, system_id, denied_reason, tag_set_id, connections
FROM afs.external_traffic WHERE host = $1 AND team_id IN ($2, $3)`, host, w.team.ID, swarm).Scan(&r.agent, &r.pool, &r.worker, &r.system, &r.reason, &r.tag, &r.conns); err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		return r
	}
	if r := read("api.github.com"); r.conns != 7 || r.agent == nil || *r.agent != w.agtA || r.pool != nil || r.worker != nil || r.system == nil || *r.system != w.sysGH || r.reason != nil || r.tag == nil {
		t.Errorf("agent row = %+v", r)
	}
	if r := read("evil.example.net"); r.conns != 2 || r.system != nil || r.reason == nil || *r.reason != traffic.ReasonNotInCat {
		t.Errorf("denied row = %+v", r)
	}
	if r := read("slack.com"); r.conns != 2 || r.agent != nil || r.pool == nil || *r.pool != poolID || r.worker == nil || *r.worker != "pool-review-7d9f-x2k4q" {
		t.Errorf("worker row = %+v", r)
	}
	// One failing row stores none of the batch.
	bad := agent
	bad.Host, bad.DeniedReason = "second.example.org", "bogus"
	bad.Verdict = traffic.VerdictDenied
	good := agent
	good.Host = "third.example.org"
	if err := store.Add(t.Context(), []traffic.Row{good, bad}); err == nil {
		t.Fatal("a row that breaks a constraint must fail the batch")
	}
	var n int
	if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.external_traffic WHERE host = 'third.example.org'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("rows of the failed batch = %d (%v)", n, err)
	}
}

// M2-H2: a flow's claim is checked against the database: the agent or pool must exist in the pod's
// namespace, and its systems carry the level they are off at.
func TestTrafficDirectory(t *testing.T) {
	db := pgtest.Get(t)
	runner := reconcilerPool(t, db)
	dir := postgres.NewTrafficDirectory(runner)
	w := seedEgress(t, db)
	swarm, poolID := seedSwarm(t, db)
	other := db.SeedTeam(t)
	ns := namespaceOf(t, db, w.team.ID)
	swarmNS := namespaceOf(t, db, swarm)
	admin := db.Admin(t)
	if _, err := admin.Exec(t.Context(), `INSERT INTO afs.external_system_optouts (id, team_id, pool_id, system_id) VALUES ($1, $2, $3, $4)`,
		pgtest.NewID("xso"), swarm, poolID, w.sysGH); err != nil {
		t.Fatal(err)
	}

	res, ok, err := dir.ResolveAgent(t.Context(), ns, w.agtA)
	if err != nil || !ok || res.TeamID != w.team.ID {
		t.Fatalf("agent: %+v %v %v", res, ok, err)
	}
	level := func(r traffic.Resolved) string {
		for _, s := range r.Systems {
			if s.ID == w.sysGH {
				return "[" + s.DisabledAt + "]"
			}
		}
		return "missing"
	}
	if got := level(res); got != "[]" {
		t.Errorf("agent view of the system = %s", got)
	}
	pres, ok, err := dir.ResolvePool(t.Context(), swarmNS, poolID)
	if err != nil || !ok || pres.TeamID != swarm {
		t.Fatalf("pool: %+v %v %v", pres, ok, err)
	}
	if got := level(pres); got != "[pool]" {
		t.Errorf("pool view of the system = %s", got)
	}
	// Claims that do not hold.
	if _, ok, err := dir.ResolveAgent(t.Context(), namespaceOf(t, db, other.ID), w.agtA); ok || err != nil {
		t.Errorf("an agent claimed from another team's namespace: ok=%v err=%v", ok, err)
	}
	if _, ok, err := dir.ResolvePool(t.Context(), namespaceOf(t, db, other.ID), poolID); ok || err != nil {
		t.Errorf("a pool claimed from another team's namespace: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := dir.ResolveAgent(t.Context(), ns, pgtest.NewID("agt")); ok {
		t.Error("an unknown agent resolved")
	}
	if _, err := admin.Exec(t.Context(), `UPDATE afs.agents SET state = 'deleted' WHERE id = $1`, w.agtB); err == nil {
		if _, ok, _ := dir.ResolveAgent(t.Context(), ns, w.agtB); ok {
			t.Error("a deleted agent resolved")
		}
	}
}

// M2-H3: the threshold is the service setting (default 20); the spike event reaches the outbox with its
// envelope, and Emitted sees it for its window only.
func TestSpikeEventsAndThreshold(t *testing.T) {
	db := pgtest.Get(t)
	runner := reconcilerPool(t, db)
	w := seedEgress(t, db)
	admin := db.Admin(t)

	th := postgres.NewSpikeThreshold(runner)
	if n, err := th.DeniedSpikeThreshold(t.Context()); err != nil || n != 20 {
		t.Fatalf("default threshold = %d, %v", n, err)
	}
	if _, err := admin.Exec(t.Context(), `UPDATE afs.service_settings SET value = '7' WHERE key = 'egress.denied_spike_threshold'`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), `UPDATE afs.service_settings SET value = '20' WHERE key = 'egress.denied_spike_threshold'`)
	}()
	if n, err := th.DeniedSpikeThreshold(t.Context()); err != nil || n != 7 {
		t.Fatalf("threshold = %d, %v", n, err)
	}

	ev := postgres.NewSpikeEvents(runner, postgres.NewOutboxWriter(), &fakes.IDGen{})
	win := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	if ok, err := ev.Emitted(t.Context(), w.agtA, win); ok || err != nil {
		t.Fatalf("before publish: %v %v", ok, err)
	}
	if err := ev.Publish(t.Context(), traffic.Spike{TeamID: w.team.ID, AgentID: w.agtA, WindowStart: win, Denied: 31, Threshold: 7, Hosts: []string{"a.example.org"}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := ev.Emitted(t.Context(), w.agtA, win); !ok || err != nil {
		t.Fatalf("after publish: %v %v", ok, err)
	}
	if ok, _ := ev.Emitted(t.Context(), w.agtA, win.Add(5*time.Minute)); ok {
		t.Error("another window must not count as announced")
	}
	var kind, id, payload string
	if err := admin.QueryRow(t.Context(), `SELECT aggregate_kind, aggregate_id, payload::text FROM afs.outbox_events WHERE event_type = 'egress.denied_spike' AND team_id = $1`, w.team.ID).Scan(&kind, &id, &payload); err != nil {
		t.Fatal(err)
	}
	if kind != "agent" || id != w.agtA {
		t.Errorf("aggregate = %s/%s", kind, id)
	}
	for _, want := range []string{`"agentId"`, `"windowStart": "2026-11-22T10:00:00Z"`, `"denied": 31`, `"threshold": 7`, `"hosts": ["a.example.org"]`} {
		if !containsStr(payload, want) {
			t.Errorf("payload lacks %s:\n%s", want, payload)
		}
	}
	// A pool's spike names the pool.
	swarm, poolID := seedSwarm(t, db)
	if err := ev.Publish(t.Context(), traffic.Spike{TeamID: swarm, PoolID: poolID, WindowStart: win, Denied: 9, Threshold: 7}); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(t.Context(), `SELECT aggregate_kind FROM afs.outbox_events WHERE event_type = 'egress.denied_spike' AND aggregate_id = $1`, poolID).Scan(&kind); err != nil || kind != "pool" {
		t.Errorf("pool spike aggregate = %q (%v)", kind, err)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

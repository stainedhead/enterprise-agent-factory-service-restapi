package postgres_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

func reconcilerPool(t *testing.T, db *pgtest.DB) *postgres.TxRunner {
	t.Helper()
	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_reconciler"], Service: "afs-reconciler", SystemActor: true, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return postgres.NewTxRunner(pool)
}

type recordedFeed struct {
	mu        sync.Mutex
	changes   []engine.Change
	connected int
}

func (r *recordedFeed) handler() engine.FeedHandler {
	return engine.FeedHandler{
		Change:    func(c engine.Change) { r.mu.Lock(); r.changes = append(r.changes, c); r.mu.Unlock() },
		Connected: func() { r.mu.Lock(); r.connected++; r.mu.Unlock() },
	}
}

func (r *recordedFeed) snapshot() ([]engine.Change, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]engine.Change(nil), r.changes...), r.connected
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// M1-F3: the change feed delivers a notification, and reconnects after the connection is lost.
func TestChangeFeedDeliversAndReconnects(t *testing.T) {
	db := pgtest.Get(t)
	feed := postgres.NewChangeFeed(postgres.Config{URL: db.URLs["afs_reconciler"], Service: "afs-feed-test", SystemActor: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	rec := &recordedFeed{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = feed.Listen(ctx, rec.handler()) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "first connection", func() bool { _, c := rec.snapshot(); return c >= 1 })

	team := db.SeedTeam(t)
	waitFor(t, "teams notification", func() bool {
		cs, _ := rec.snapshot()
		for _, c := range cs {
			if c.Table == "teams" && c.ID == team.ID && c.TeamID == team.ID {
				return true
			}
		}
		return false
	})

	// Kill the listening backend: the feed reconnects and reports Connected again.
	_, err := db.Admin(t).Exec(t.Context(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = 'afs-feed-test' AND pid <> pg_backend_pid()`)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reconnection", func() bool { _, c := rec.snapshot(); return c >= 2 })
	team2 := db.SeedTeam(t)
	waitFor(t, "notification after reconnect", func() bool {
		cs, _ := rec.snapshot()
		for _, c := range cs {
			if c.ID == team2.ID {
				return true
			}
		}
		return false
	})
}

// M1-F3: the poll finds resources whose generation was not observed, and a full list skips deleted ones.
func TestListerStaleAndList(t *testing.T) {
	db := pgtest.Get(t)
	runner := reconcilerPool(t, db)
	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_reconciler"], Service: "afs-lister-test", SystemActor: true})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	lister := postgres.NewLister(pool)
	admin := db.Admin(t)
	team := db.SeedTeam(t)
	gone := db.SeedTeam(t)
	if _, err := admin.Exec(t.Context(), `UPDATE afs.teams SET state = 'deleted', deleted_at = now() WHERE id = $1`, gone.ID); err != nil {
		t.Fatal(err)
	}
	has := func(refs []engine.Ref, id string) bool {
		for _, r := range refs {
			if r.ID == id && r.Kind == engine.KindTeam && r.TeamID == id {
				return true
			}
		}
		return false
	}
	kinds := []engine.Kind{engine.KindTeam, engine.KindAgent, engine.KindPool, engine.KindAdmission}
	stale, err := lister.Stale(t.Context(), kinds)
	if err != nil || !has(stale, team.ID) || has(stale, gone.ID) {
		t.Fatalf("stale = %v, %v", stale, err)
	}
	all, err := lister.List(t.Context(), kinds)
	if err != nil || !has(all, team.ID) || has(all, gone.ID) {
		t.Fatalf("list = %v, %v", all, err)
	}

	// Observed: no longer stale.
	w := postgres.NewStatusWriter(runner, postgres.NewOutboxWriter(), &fakes.IDGen{}, &fakes.Clock{T: time.Now()})
	gen := int64(1)
	rep := engine.Report{Ref: engine.NewRef(engine.KindTeam, team.ID, team.ID), Generation: &gen, Phase: domain.PhaseReady, Replace: true, ReconciledAt: time.Now()}
	if err := w.Apply(t.Context(), rep); err != nil {
		t.Fatal(err)
	}
	if stale, _ := lister.Stale(t.Context(), kinds); has(stale, team.ID) {
		t.Error("observed team still stale")
	}
	// A new generation makes it stale again.
	if _, err := admin.Exec(t.Context(), `UPDATE afs.teams SET generation = generation + 1 WHERE id = $1`, team.ID); err != nil {
		t.Fatal(err)
	}
	if stale, _ := lister.Stale(t.Context(), kinds); !has(stale, team.ID) {
		t.Error("team with a new generation is not stale")
	}
}

type statusRow struct {
	phase string
	gen   int64
	conds []domain.Condition
	team  string
}

func readStatus(t *testing.T, db *pgtest.DB, kind, id string) statusRow {
	t.Helper()
	var (
		r  statusRow
		cs []byte
	)
	err := db.Admin(t).QueryRow(t.Context(), `SELECT phase, observed_generation, conditions, coalesce(team_id, '') FROM afs.resource_status WHERE resource_kind = $1 AND resource_id = $2`, kind, id).Scan(&r.phase, &r.gen, &cs, &r.team)
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if err := json.Unmarshal(cs, &r.conds); err != nil {
		t.Fatal(err)
	}
	return r
}

func statusEvents(t *testing.T, db *pgtest.DB, id string) []map[string]any {
	t.Helper()
	rows, err := db.Admin(t).Query(t.Context(), `SELECT payload FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = 'team.status_changed' ORDER BY id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(p, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// M1-F5: the status writer upserts with the D24 vocabulary, keeps transition times, and writes the
// status_changed event only when the phase changes.
func TestStatusWriter(t *testing.T) {
	db := pgtest.Get(t)
	runner := reconcilerPool(t, db)
	team := db.SeedTeam(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	clock := &fakes.Clock{T: now}
	w := postgres.NewStatusWriter(runner, postgres.NewOutboxWriter(), &fakes.IDGen{}, clock)
	ref := engine.NewRef(engine.KindTeam, team.ID, team.ID)
	gen := int64(1)

	// Reconcile ok: Pending → Ready, one event.
	ok := engine.Report{Ref: ref, Generation: &gen, Phase: domain.PhaseReady, Replace: true, ReconciledAt: now,
		Conditions: []domain.Condition{{Type: domain.CondNamespaceReady, Status: domain.ConditionTrue}, {Type: domain.CondIamReady, Status: domain.ConditionTrue}}}
	if err := w.Apply(t.Context(), ok); err != nil {
		t.Fatal(err)
	}
	got := readStatus(t, db, "team", team.ID)
	if got.phase != "Ready" || got.gen != 1 || got.team != team.ID || len(got.conds) != 2 {
		t.Fatalf("status = %+v", got)
	}
	evs := statusEvents(t, db, team.ID)
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if d := evs[0]["data"].(map[string]any); d["phase"] != "Ready" || d["previousPhase"] != "Pending" || evs[0]["teamId"] != team.ID {
		t.Errorf("event = %v", evs[0])
	}

	// Same phase again, later: no new event, transition times kept.
	later := now.Add(time.Hour)
	ok.ReconciledAt = later
	if err := w.Apply(t.Context(), ok); err != nil {
		t.Fatal(err)
	}
	if n := len(statusEvents(t, db, team.ID)); n != 1 {
		t.Errorf("events after an unchanged phase = %d", n)
	}
	if c := readStatus(t, db, "team", team.ID).conds; !c[0].LastTransitionTime.Equal(now) {
		t.Errorf("transition time moved: %v", c[0].LastTransitionTime)
	}

	// Failure: Degraded with the code as the reason; generation stays; second event.
	fail := engine.Report{Ref: ref, Phase: domain.PhaseDegraded, ReconciledAt: later,
		Conditions: []domain.Condition{{Type: domain.CondIamReady, Status: domain.ConditionFalse, Reason: "AccessDenied", Message: "iam:CreateRole denied"}}}
	if err := w.Apply(t.Context(), fail); err != nil {
		t.Fatal(err)
	}
	got = readStatus(t, db, "team", team.ID)
	if got.phase != "Degraded" || got.gen != 1 {
		t.Errorf("after failure = %+v", got)
	}
	var iam, ns domain.Condition
	for _, c := range got.conds {
		switch c.Type {
		case domain.CondIamReady:
			iam = c
		case domain.CondNamespaceReady:
			ns = c
		}
	}
	if iam.Status != domain.ConditionFalse || iam.Reason != "AccessDenied" || ns.Status != domain.ConditionTrue {
		t.Errorf("conditions = %+v", got.conds)
	}
	evs = statusEvents(t, db, team.ID)
	if len(evs) != 2 || evs[1]["data"].(map[string]any)["previousPhase"] != "Ready" {
		t.Errorf("events = %v", evs)
	}

	// The database refuses a phase outside the vocabulary (the writer defaults an invalid one).
	if err := w.Apply(t.Context(), engine.Report{Ref: ref, Phase: "Weird", ReconciledAt: later, Replace: true}); err != nil {
		t.Fatal(err)
	}
	if got := readStatus(t, db, "team", team.ID); got.phase != "Ready" {
		t.Errorf("invalid phase stored as %q", got.phase)
	}
}

// M1-F5: an agent status change carries the agent id in the event.
func TestStatusWriterAgentEventIDs(t *testing.T) {
	db := pgtest.Get(t)
	runner := reconcilerPool(t, db)
	team := db.SeedTeam(t)
	w := postgres.NewStatusWriter(runner, postgres.NewOutboxWriter(), &fakes.IDGen{}, &fakes.Clock{T: time.Now()})
	agentID := pgtest.NewID("agt")
	gen := int64(2)
	// resource_status has no foreign key to agents, only to teams.
	err := w.Apply(t.Context(), engine.Report{Ref: engine.NewRef(engine.KindAgent, agentID, team.ID), Generation: &gen, Phase: domain.PhaseReady, Replace: true, ReconciledAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	var payload []byte
	err = db.Admin(t).QueryRow(t.Context(), `SELECT payload FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = 'agent.status_changed'`, agentID).Scan(&payload)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(payload, &m)
	if m["agentId"] != agentID || m["teamId"] != team.ID {
		t.Errorf("payload = %v", m)
	}
	// admission has no event type
	if err := w.Apply(t.Context(), engine.Report{Ref: engine.NewRef(engine.KindAdmission, "afs-admission", ""), Phase: domain.PhaseReady, Replace: true, ReconciledAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

// M1-F6: one audit record and one drift.corrected event per correction, written by the reconciler.
func TestDriftRecorder(t *testing.T) {
	db := pgtest.Get(t)
	runner := reconcilerPool(t, db)
	team := db.SeedTeam(t)
	rec := postgres.NewDriftRecorder(runner, postgres.NewAuditWriter(runner), postgres.NewOutboxWriter(), &fakes.IDGen{})
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	err := rec.Record(t.Context(), engine.DriftRecord{Ref: engine.NewRef(engine.KindTeam, team.ID, team.ID), Object: "NetworkPolicy/ns/default-deny-ingress", At: at})
	if err != nil {
		t.Fatal(err)
	}
	admin := db.Admin(t)
	var n int
	var actor, outcome string
	var after []byte
	err = admin.QueryRow(t.Context(), `SELECT count(*), max(actor_id), max(outcome), max(after::text)::bytea FROM afs.audit_log WHERE action = 'drift.corrected' AND team_id = $1`, team.ID).Scan(&n, &actor, &outcome, &after)
	if err != nil || n != 1 || actor != "system:afs-reconciler" || outcome != "allowed" {
		t.Fatalf("audit = %d %q %q %v", n, actor, outcome, err)
	}
	var p []byte
	if err := admin.QueryRow(t.Context(), `SELECT payload FROM afs.outbox_events WHERE event_type = 'drift.corrected' AND team_id = $1`, team.ID).Scan(&p); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(p, &m)
	if m["data"].(map[string]any)["object"] != "NetworkPolicy/ns/default-deny-ingress" {
		t.Errorf("event = %v", m)
	}
}

// M1-G1d: the team source reads the row, quotas and enabled models, and records an inference profile ARN.
func TestTeamSource(t *testing.T) {
	db := pgtest.Get(t)
	tm := db.SeedTeam(t)
	admin := db.Admin(t)
	ctx := t.Context()
	model := "m-" + tm.Slug
	if _, err := admin.Exec(ctx, `INSERT INTO afs.models (id, provider, display_name) VALUES ($1, 'Anthropic', 'M'), ($1 || '-off', 'Anthropic', 'Off')`, model); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE afs.models SET enabled = false WHERE id = $1 || '-off'`, model); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, $2), ($1, $2 || '-off')`, tm.ID, model); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE afs.teams SET quotas = '{"cpu":"8","memory":"16Gi","pods":20}' WHERE id = $1`, tm.ID); err != nil {
		t.Fatal(err)
	}

	src := postgres.NewTeamSource(reconcilerPool(t, db))
	got, err := src.LoadTeam(ctx, tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != tm.Slug || got.Namespace != "afs-t-"+tm.Slug || got.Quotas.CPU != "8" || got.Quotas.Pods != 20 || got.State != "active" {
		t.Errorf("team = %+v", got)
	}
	if len(got.Models) != 1 || got.Models[0].ID != model || got.Models[0].ProfileARN != "" {
		t.Errorf("only enabled catalog models are returned: %+v", got.Models)
	}
	arn := "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/abc"
	if err := src.SetInferenceProfileARN(ctx, tm.ID, model, arn); err != nil {
		t.Fatal(err)
	}
	if got, _ = src.LoadTeam(ctx, tm.ID); got.Models[0].ProfileARN != arn {
		t.Errorf("ARN not recorded: %+v", got.Models)
	}
	if err := src.SetInferenceProfileARN(ctx, tm.ID, "nope", arn); err == nil {
		t.Errorf("unknown model: %v", err)
	}
	if _, err := src.LoadTeam(ctx, "team_01JD0000000000000000009999"); err == nil {
		t.Error("an unknown team must be NotFound")
	}
}

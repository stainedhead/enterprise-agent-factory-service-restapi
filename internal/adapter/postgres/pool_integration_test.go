package postgres_test

import (
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/lifecycle"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/teardown"
)

type seededPool struct {
	teamID, poolID, otherID, linkID string
}

// seedPool inserts an ephemeral team with two pools (a handoff from the first to the second) and a role link.
func seedPool(t *testing.T, db *pgtest.DB) seededPool {
	t.Helper()
	ctx := t.Context()
	admin := db.Admin(t)
	s := seededPool{teamID: pgtest.NewID("team"), poolID: pgtest.NewID("pool"), otherID: pgtest.NewID("pool"), linkID: pgtest.NewID("prl")}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%.60s: %v", sql, err)
		}
	}
	exec(`INSERT INTO afs.org_aws_accounts (id, name, status) VALUES ('111122223333', 'payments-prod', 'active') ON CONFLICT DO NOTHING`)
	exec(`INSERT INTO afs.models (id, provider, display_name) VALUES ('claude-sonnet', 'Anthropic', 'Claude Sonnet') ON CONFLICT DO NOTHING`)
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO afs.teams (id, slug, display_name, kind, cost_center, owner_email, max_budget_usd, expires_at, teardown_rule, allowed_aws_accounts)
	      VALUES ($1, $2, $2, 'ephemeral', 'CC1', 'o@example.com', 100, now() + interval '3 days', 'at_expiry', '{*}')`, s.teamID, "s"+lower(s.teamID[len(s.teamID)-8:])); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO afs.budgets (id, team_id, period, amount_usd, on_exhaust) VALUES ($1, $2, 'lifetime', 50, 'block')`, pgtest.NewID("bud"), s.teamID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO afs.team_models (team_id, model_id) VALUES ($1, 'claude-sonnet')`, s.teamID)
	const img = `ghcr.io/x/omp@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`
	exec(`INSERT INTO afs.agent_pools (id, team_id, name, template, image, model_id, workers, resources, scratch_size_gib, shared_volume)
	      VALUES ($1, $3, 'triage', 'omp', $4, 'claude-sonnet', 3, '{"cpu":"500m","memory":"1Gi"}', 40, 'efs'),
	             ($2, $3, 'fixers', 'omp', $4, 'claude-sonnet', 1, '{}', 20, 'none')`, s.poolID, s.otherID, s.teamID, img)
	exec(`INSERT INTO afs.pool_handoffs (team_id, from_pool_id, to_pool_id) VALUES ($1, $2, $3)`, s.teamID, s.poolID, s.otherID)
	exec(`INSERT INTO afs.pool_role_links (id, team_id, pool_id, account_id, role_name, profile_name, external_id)
	      VALUES ($1, $2, $3, '111122223333', 'afs-pool-shared', 'prod', 'afs-' || $4)`, s.linkID, s.teamID, s.poolID, "X"+s.linkID[4:])
	return s
}

// M5-C: the reconciler reads a pool with its workers, volumes, links and handoff peers.
func TestPoolSourceLoadPool(t *testing.T) {
	db := pgtest.Get(t)
	s := seedPool(t, db)
	src := postgres.NewPoolSource(reconcilerPool(t, db))

	d, err := src.LoadPool(t.Context(), s.poolID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "triage" || d.Workers != 3 || d.State != "enabled" || d.ScratchGiB != 40 || d.SharedVolume != "efs" || d.Resources.CPU != "500m" {
		t.Errorf("pool = %+v", d)
	}
	if d.Team.ID != s.teamID || d.Team.IsolationTier != "shared" || d.Team.State != "active" {
		t.Errorf("team = %+v", d.Team)
	}
	if len(d.RoleLinks) != 1 || d.RoleLinks[0].AccountID != "111122223333" || !d.RoleLinks[0].Allowed || d.RoleLinks[0].Status != domain.RoleLinkStatusPendingTrust {
		t.Errorf("links = %+v", d.RoleLinks)
	}
	if len(d.SendsTo) != 1 || d.SendsTo[0].ID != s.otherID || len(d.ReceivesFrom) != 0 {
		t.Errorf("peers = %+v / %+v", d.SendsTo, d.ReceivesFrom)
	}
	o, err := src.LoadPool(t.Context(), s.otherID)
	if err != nil || len(o.ReceivesFrom) != 1 || o.ReceivesFrom[0].ID != s.poolID {
		t.Errorf("receiver peers = %+v, %v", o.ReceivesFrom, err)
	}

	if _, err := src.LoadPool(t.Context(), pgtest.NewID("pool")); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("missing pool: %v", err)
	}
	list, err := src.ListTeamPools(t.Context(), s.teamID)
	if err != nil || len(list) != 2 {
		t.Errorf("list = %+v, %v", list, err)
	}
}

// M5-C6: the reconciler can write a pool's state and deleted_at (X-DB-5), the completion is audited once, and
// a pool that is not deleting is refused.
func TestPoolLifecycleComplete(t *testing.T) {
	db := pgtest.Get(t)
	runner, audit := reconcilerRunner(t, db)
	lc := postgres.NewPoolLifecycle(runner, audit, postgres.NewOutboxWriter(), budgetIDs{})
	admin := db.Admin(t)
	s := seedPool(t, db)
	tgt := lifecycle.Target{Kind: "pool", ID: s.poolID, TeamID: s.teamID}

	if err := lc.Complete(t.Context(), tgt); err == nil {
		t.Fatal("an enabled pool must not be completed")
	}
	if _, err := admin.Exec(t.Context(), `UPDATE afs.agent_pools SET state = 'deleting' WHERE id = $1`, s.poolID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := lc.Complete(t.Context(), tgt); err != nil {
			t.Fatalf("pass %d: %v", i, err)
		}
	}
	var n int
	if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.agent_pools WHERE id = $1 AND state = 'deleted' AND deleted_at IS NOT NULL`, s.poolID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("pool not deleted: %d %v", n, err)
	}
	if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE resource_id = $1 AND action = 'pool.delete_finalized'`, s.poolID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit = %d %v", n, err)
	}
	if err := lc.Complete(t.Context(), lifecycle.Target{Kind: "team", ID: s.teamID}); err == nil {
		t.Error("a team target must be refused")
	}
	src := postgres.NewPoolSource(reconcilerPool(t, db))
	o, err := src.LoadPool(t.Context(), s.otherID)
	if err != nil || len(o.ReceivesFrom) != 0 {
		t.Errorf("a deleted pool is no longer a peer: %+v %v", o.ReceivesFrom, err)
	}
}

// M5-C3: a verification result is written with audit and the event, never over a status that changed.
func TestPoolRecordLink(t *testing.T) {
	db := pgtest.Get(t)
	runner, audit := reconcilerRunner(t, db)
	lc := postgres.NewPoolLifecycle(runner, audit, postgres.NewOutboxWriter(), budgetIDs{})
	admin := db.Admin(t)
	s := seedPool(t, db)
	now := time.Now().UTC()
	change := pool.LinkChange{TeamID: s.teamID, PoolID: s.poolID, LinkID: s.linkID, From: domain.RoleLinkStatusPendingTrust, To: domain.RoleLinkStatusVerified,
		VerifiedAt: &now, AccountID: "111122223333", RoleARN: "arn:aws:iam::111122223333:role/afs-pool-shared", Event: domain.EventRoleLinkVerified}
	if err := lc.RecordLink(t.Context(), change); err != nil {
		t.Fatal(err)
	}
	var status string
	var n int
	if err := admin.QueryRow(t.Context(), `SELECT status FROM afs.pool_role_links WHERE id = $1`, s.linkID).Scan(&status); err != nil || status != "verified" {
		t.Fatalf("status = %q %v", status, err)
	}
	if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE resource_id = $1`, s.linkID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit = %d %v", n, err)
	}
	if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.outbox_events WHERE team_id = $1 AND event_type = 'role_link.verified'`, s.teamID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("events = %d %v", n, err)
	}
	// The same change again finds the link no longer pending_trust: nothing is written.
	if err := lc.RecordLink(t.Context(), change); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE resource_id = $1`, s.linkID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("stale change wrote audit: %d", n)
	}
}

func tearSeed(t *testing.T, db *pgtest.DB, expiresIn string, budget float64) string {
	t.Helper()
	ctx := t.Context()
	admin := db.Admin(t)
	id := pgtest.NewID("team")
	tx, err := admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO afs.teams (id, slug, display_name, kind, cost_center, owner_email, max_budget_usd, expires_at, teardown_rule)
	      VALUES ($1, $2, $2, 'ephemeral', 'CC1', 'o@example.com', 100, now() + $3::interval, 'at_expiry')`, id, "s"+lower(id[len(id)-8:]), expiresIn); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO afs.budgets (id, team_id, period, amount_usd, on_exhaust) VALUES ($1, $2, 'lifetime', $3, 'block')`, pgtest.NewID("bud"), id, budget); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return id
}

// M5-D1: a due ephemeral team starts its teardown (deleting, torn_down_at, reason, audit, event); when the
// finalizer has deleted it the record is closed with the second event; nothing is repeated.
func TestTeardownStoreFlow(t *testing.T) {
	db := pgtest.Get(t)
	runner, audit := reconcilerRunner(t, db)
	store := postgres.NewTeardownStore(runner, audit, postgres.NewOutboxWriter(), budgetIDs{})
	team := postgres.NewTeamLifecycle(runner, audit)
	admin := db.Admin(t)
	ctx := t.Context()
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := admin.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	due := tearSeed(t, db, "3 days", 50)
	// Past its expiry: set through the table the guard allows for the owner.
	if _, err := admin.Exec(ctx, `ALTER TABLE afs.teams DISABLE TRIGGER teams_lifecycle_guard`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `UPDATE afs.teams SET expires_at = now() - interval '1 minute' WHERE id = $1`, due); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `ALTER TABLE afs.teams ENABLE TRIGGER teams_lifecycle_guard`); err != nil {
		t.Fatal(err)
	}
	notDue := tearSeed(t, db, "3 days", 50)

	list, err := store.Due(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mine *teardown.Due
	for i := range list {
		if list[i].TeamID == notDue {
			t.Errorf("a team that is not due is listed: %+v", list[i])
		}
		if list[i].TeamID == due {
			mine = &list[i]
		}
	}
	if mine == nil || mine.Reason != "expired" || mine.Rule != "at_expiry" {
		t.Fatalf("due = %+v", list)
	}

	for i := 0; i < 2; i++ {
		started, err := store.Begin(ctx, *mine, time.Now())
		if err != nil || started != (i == 0) {
			t.Fatalf("Begin %d = %v, %v (only the first may start)", i, started, err)
		}
	}
	if n := count(`SELECT count(*) FROM afs.teams WHERE id = $1 AND state = 'deleting' AND torn_down_at IS NOT NULL AND teardown_reason = 'expired'`, due); n != 1 {
		t.Fatal("team not deleting with torn_down_at and reason")
	}
	if n := count(`SELECT count(*) FROM afs.audit_log WHERE resource_id = $1 AND action = 'team.teardown_started'`, due); n != 1 {
		t.Errorf("start audit = %d", n)
	}
	if n := count(`SELECT count(*) FROM afs.outbox_events WHERE team_id = $1 AND event_type = 'team.teardown_started'`, due); n != 1 {
		t.Errorf("start events = %d", n)
	}
	if fin, _ := store.Finished(ctx); containsTeam(fin, due) {
		t.Error("a team still deleting is reported finished")
	}
	if list, _ := store.Due(ctx); containsDue(list, due) {
		t.Error("a team being torn down is still due")
	}

	// The finalizer's last step (RC-17) deletes the team.
	if err := team.Complete(ctx, lifecycle.Target{Kind: "team", ID: due, TeamID: due}); err != nil {
		t.Fatal(err)
	}
	fin, err := store.Finished(ctx)
	if err != nil || !containsTeam(fin, due) {
		t.Fatalf("finished = %+v, %v", fin, err)
	}
	for i := 0; i < 2; i++ {
		if err := store.Close(ctx, teardown.Finished{TeamID: due, Reason: "expired"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(`SELECT count(*) FROM afs.outbox_events WHERE team_id = $1 AND event_type = 'team.torn_down'`, due); n != 1 {
		t.Errorf("torn_down events = %d, want 1", n)
	}
	if n := count(`SELECT count(*) FROM afs.audit_log WHERE resource_id = $1 AND action = 'team.torn_down'`, due); n != 1 {
		t.Errorf("torn_down audit = %d", n)
	}
	if fin, _ := store.Finished(ctx); containsTeam(fin, due) {
		t.Error("a closed teardown is reported again")
	}
	// Kept: the team row, its budget, its revisions and the audit.
	if n := count(`SELECT count(*) FROM afs.teams WHERE id = $1 AND state = 'deleted' AND teardown_reason = 'expired' AND torn_down_at = deleted_at`, due); n != 1 {
		t.Error("deleted team row not kept with its teardown record")
	}
	if n := count(`SELECT count(*) FROM afs.budgets WHERE team_id = $1`, due); n != 1 {
		t.Error("budget row not kept")
	}

	// A persistent team is never touched, even when asked.
	pt := db.SeedTeam(t)
	if ok, err := store.Begin(ctx, teardown.Due{TeamID: pt.ID, Reason: "expired"}, time.Now()); err != nil || ok {
		t.Errorf("a persistent team was started: %v %v", ok, err)
	}
	if ok, err := store.Begin(ctx, teardown.Due{TeamID: pgtest.NewID("team"), Reason: "expired"}, time.Now()); err != nil || ok {
		t.Errorf("a missing team was started: %v %v", ok, err)
	}
}

// M5-D1: the whole job drives a due team to deleting and, after the finalizer, to its closing record.
func TestTeardownJobEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	runner, audit := reconcilerRunner(t, db)
	store := postgres.NewTeardownStore(runner, audit, postgres.NewOutboxWriter(), budgetIDs{})
	team := postgres.NewTeamLifecycle(runner, audit)
	admin := db.Admin(t)
	ctx := t.Context()
	id := tearSeed(t, db, "3 days", 50)
	// At its cap: the lifetime limit is spent.
	if _, err := admin.Exec(ctx, `UPDATE afs.teams SET teardown_rule = 'at_cap_or_expiry' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO afs.spend_counters (budget_id, period_start, limit_usd, spent_usd)
	      SELECT b.id, b.starts_at, b.amount_usd, b.amount_usd FROM afs.budgets b WHERE b.team_id = $1`, id); err != nil {
		t.Fatalf("seed spent counter: %v", err)
	}
	list, err := store.Due(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !containsDue(list, id) {
		t.Fatalf("a team at its cap is not due: %+v", list)
	}
	job := &teardown.Job{Store: store, Now: time.Now}
	if err := job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	var state, reason string
	if err := admin.QueryRow(ctx, `SELECT state, teardown_reason FROM afs.teams WHERE id = $1`, id).Scan(&state, &reason); err != nil || state != "deleting" || reason != "cap_reached" {
		t.Fatalf("after the first pass: %q %q %v", state, reason, err)
	}
	if err := team.Complete(ctx, lifecycle.Target{Kind: "team", ID: id, TeamID: id}); err != nil {
		t.Fatal(err)
	}
	if err := job.Run(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM afs.outbox_events WHERE team_id = $1 AND event_type IN ('team.teardown_started', 'team.torn_down')`, id).Scan(&n); err != nil || n != 2 {
		t.Errorf("events = %d, want both", n)
	}
}

func containsTeam(l []teardown.Finished, id string) bool {
	for _, f := range l {
		if f.TeamID == id {
			return true
		}
	}
	return false
}

func containsDue(l []teardown.Due, id string) bool {
	for _, d := range l {
		if d.TeamID == id {
			return true
		}
	}
	return false
}

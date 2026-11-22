package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/housekeeping"
)

func TestHousekeepingOutbox(t *testing.T) {
	db := pgtest.Get(t)
	runner, _ := reconcilerRunner(t, db)
	hk := postgres.NewHousekeeping(runner)
	admin := db.Admin(t)
	ctx := context.Background()
	tm := db.SeedTeam(t)

	for i, typ := range []string{"team.created", "team.updated", "team.suspended"} {
		if _, err := admin.Exec(ctx, `INSERT INTO afs.outbox_events (event_type, aggregate_kind, aggregate_id, team_id, payload, created_at)
VALUES ($1, 'team', $2, $2, '{"id":"evt"}', now() - make_interval(secs => $3))`, typ, tm.ID, 100-i); err != nil {
			t.Fatal(err)
		}
	}
	got, err := hk.Pending(ctx, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("pending %v err %v", got, err)
	}
	if got[0].ID >= got[1].ID || got[0].Type != "team.created" || got[0].TeamID != tm.ID || string(got[0].Payload) != `{"id": "evt"}` {
		t.Fatalf("not in id order / wrong content: %+v", got)
	}
	n, oldest, err := hk.Lag(ctx)
	if err != nil || n < 3 || oldest < 99*time.Second {
		t.Fatalf("lag n=%d oldest=%v err=%v", n, oldest, err)
	}
	if err := hk.RecordFailure(ctx, got[0].ID); err != nil {
		t.Fatal(err)
	}
	again, _ := hk.Pending(ctx, 1)
	if again[0].Attempts != 1 {
		t.Fatalf("attempts = %d", again[0].Attempts)
	}
	if err := hk.MarkPublished(ctx, []int64{got[0].ID, got[1].ID}); err != nil {
		t.Fatal(err)
	}
	if err := hk.MarkPublished(ctx, nil); err != nil {
		t.Fatal(err)
	}
	rest, _ := hk.Pending(ctx, 10)
	for _, e := range rest {
		if e.ID == got[0].ID || e.ID == got[1].ID {
			t.Fatal("published event still pending")
		}
	}
	// the publisher's role cannot touch the payload or delete rows
	if err := runner.InTx(ctx, port.SystemSession("afs-reconciler"), func(ctx context.Context, tx port.Tx) error {
		pt, err := postgres.Unwrap(tx)
		if err != nil {
			return err
		}
		_, err = pt.Exec(ctx, `UPDATE afs.outbox_events SET payload = '{}'`)
		return err
	}); err == nil {
		t.Fatal("the reconciler changed an outbox payload")
	}
	// and a full publisher pass over the real store marks everything with a fake bus
	var sent int
	sink := sinkFn(func(es []housekeeping.Entry) ([]error, error) { sent += len(es); return make([]error, len(es)), nil })
	if err := housekeeping.NewPublisher(hk, sink, nil, nil).Drain(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := hk.Lag(ctx); n != 0 || sent == 0 {
		t.Fatalf("pending after drain = %d, sent %d", n, sent)
	}
}

func TestHousekeepingPartitionsAndPurge(t *testing.T) {
	db := pgtest.Get(t)
	runner, _ := reconcilerRunner(t, db)
	hk := postgres.NewHousekeeping(runner)
	admin := db.Admin(t)
	ctx := context.Background()

	exists := func(name string) bool {
		var ok bool
		if err := admin.QueryRow(ctx, `SELECT to_regclass('afs.' || $1) IS NOT NULL`, name).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	t.Run("ensure creates the next month on the last day of a month", func(t *testing.T) {
		pm := &housekeeping.PartitionManager{Ops: hk, Clock: func() time.Time { return time.Date(2031, 3, 31, 23, 59, 0, 0, time.UTC) }}
		for i := 0; i < 2; i++ { // second run: already there, still fine
			if err := pm.Ensure(ctx); err != nil {
				t.Fatal(err)
			}
		}
		for _, p := range []string{"usage_events", "agent_activity", "external_traffic"} {
			for _, m := range []string{"203103", "203104", "203105"} {
				if !exists(p + "_" + m) {
					t.Errorf("partition %s_%s missing", p, m)
				}
			}
		}
	})

	t.Run("drop removes old partitions and never touches audit_log", func(t *testing.T) {
		tm := db.SeedTeam(t)
		if _, err := admin.Exec(ctx, `INSERT INTO afs.audit_log (actor_type, actor_id, action, outcome, team_id)
VALUES ('system', 'system:test', 'test.keep', 'allowed', $1)`, tm.ID); err != nil {
			t.Fatal(err)
		}
		old := time.Now().UTC().AddDate(-2, 0, 0)
		for _, tab := range housekeeping.Tables() {
			if _, err := hk.EnsurePartition(ctx, tab, old); err != nil {
				t.Fatal(err)
			}
		}
		oldSuffix := old.Format("200601")
		recent := time.Now().UTC().AddDate(0, -3, 0).Format("200601")
		if _, err := hk.EnsurePartition(ctx, housekeeping.TableUsage, time.Now().UTC().AddDate(0, -3, 0)); err != nil {
			t.Fatal(err)
		}
		pm := &housekeeping.PartitionManager{Ops: hk, Clock: time.Now}
		if err := pm.DropMonthly(ctx); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"usage_events", "agent_activity", "external_traffic"} {
			if exists(p + "_" + oldSuffix) {
				t.Errorf("%s_%s survived retention", p, oldSuffix)
			}
		}
		if !exists("usage_events_" + recent) {
			t.Error("a partition inside the retention window was dropped")
		}
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM afs.audit_log WHERE action = 'test.keep'`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("audit rows = %d err %v", n, err)
		}
		if _, err := hk.EnsurePartition(ctx, "audit_log", old); err == nil {
			t.Fatal("audit_log accepted as a partitioned table")
		}
	})

	t.Run("purge removes only expired idempotency keys", func(t *testing.T) {
		tm := db.SeedTeam(t)
		ins := func(key string, created, expires string) {
			if _, err := admin.Exec(ctx, `INSERT INTO afs.idempotency_keys (principal, key, team_id, operation, request_sha256, created_at, expires_at)
VALUES ('usr_x', $1, $2, 'CreateAgent', decode(repeat('ab', 32), 'hex'), now() - $3::interval, now() - $4::interval)`, key, tm.ID, created, expires); err != nil {
				t.Fatal(err)
			}
		}
		ins("expired-1", "2 hours", "1 hour")
		ins("expired-2", "3 hours", "30 minutes")
		ins("live", "1 hour", "-1 hour")
		n, err := hk.PurgeIdempotencyKeys(ctx)
		if err != nil || n != 2 {
			t.Fatalf("purged %d err %v", n, err)
		}
		var left int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM afs.idempotency_keys WHERE team_id = $1`, tm.ID).Scan(&left); err != nil || left != 1 {
			t.Fatalf("left %d err %v", left, err)
		}
	})
}

type sinkFn func([]housekeeping.Entry) ([]error, error)

func (f sinkFn) PutEvents(_ context.Context, es []housekeeping.Entry) ([]error, error) { return f(es) }

// The rollup job's database side: afs_reconciler runs afs.rollup_reports_recent (-db 000018) and the
// samples of today show in the rollup; a role without the grant is refused.
func TestHousekeepingRollupRecent(t *testing.T) {
	db := pgtest.Get(t)
	runner, _ := reconcilerRunner(t, db)
	hk := postgres.NewHousekeeping(runner)
	admin := db.Admin(t)
	tm := db.SeedTeam(t)
	agent := pgtest.NewID("agt")
	if _, err := admin.Exec(t.Context(), `INSERT INTO afs.agent_activity (bucket_start, team_id, agent_id, state, active_seconds, model_calls)
VALUES (date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', $1, $2, 'working', 120, 3)`, tm.ID, agent); err != nil {
		t.Fatal(err)
	}
	if n, err := hk.Recent(t.Context(), 2); err != nil || n < 1 {
		t.Fatalf("rollup recent = %d, %v", n, err)
	}
	var working, calls int64
	if err := admin.QueryRow(t.Context(), `SELECT sum(working_seconds), sum(model_calls) FROM afs.agent_activity_rollup_daily WHERE agent_id = $1`, agent).Scan(&working, &calls); err != nil || working != 300 || calls != 3 {
		t.Fatalf("rollup row = %d %d, %v", working, calls, err)
	}
	if _, err := hk.Recent(t.Context(), 0); err == nil {
		t.Fatal("0 days accepted")
	}
	// The API's role cannot run it.
	if _, err := appPool(t, db).Exec(t.Context(), `SELECT afs.rollup_reports_recent(2)`); err == nil {
		t.Fatal("afs_app ran the rollup")
	}
}

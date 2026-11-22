package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/lifecycle"
)

func reconcilerRunner(t *testing.T, db *pgtest.DB) (*postgres.TxRunner, *postgres.AuditWriter) {
	t.Helper()
	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_reconciler"], Service: "afs-reconciler", SystemActor: true, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r := postgres.NewTxRunner(pool)
	return r, postgres.NewAuditWriter(r)
}

// M1-I1: the finalizer's last step against a real database.
func TestTeamLifecycleComplete(t *testing.T) {
	db := pgtest.Get(t)
	runner, audit := reconcilerRunner(t, db)
	lc := postgres.NewTeamLifecycle(runner, audit)
	admin := db.Admin(t)
	ctx := context.Background()

	count := func(q string, args ...any) int {
		var n int
		if err := admin.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("deleting becomes deleted with audit, idempotently", func(t *testing.T) {
		tm := db.SeedTeam(t)
		if _, err := admin.Exec(ctx, `UPDATE afs.teams SET state = 'deleting' WHERE id = $1`, tm.ID); err != nil {
			t.Fatal(err)
		}
		tgt := lifecycle.Target{Kind: "team", ID: tm.ID, TeamID: tm.ID}
		for i := 0; i < 2; i++ {
			if err := lc.Complete(ctx, tgt); err != nil {
				t.Fatalf("pass %d: %v", i, err)
			}
		}
		if n := count(`SELECT count(*) FROM afs.teams WHERE id = $1 AND state = 'deleted' AND deleted_at IS NOT NULL`, tm.ID); n != 1 {
			t.Fatal("team not deleted")
		}
		if n := count(`SELECT count(*) FROM afs.audit_log WHERE resource_id = $1 AND action = 'team.delete_finalized' AND actor_type = 'system'`, tm.ID); n != 1 {
			t.Fatalf("audit records = %d, want exactly 1 (retry must not duplicate)", n)
		}
	})

	t.Run("an active team is refused", func(t *testing.T) {
		tm := db.SeedTeam(t)
		if err := lc.Complete(ctx, lifecycle.Target{Kind: "team", ID: tm.ID}); err == nil {
			t.Fatal("completed a team that is not deleting")
		}
		if n := count(`SELECT count(*) FROM afs.teams WHERE id = $1 AND state = 'active'`, tm.ID); n != 1 {
			t.Fatal("state changed")
		}
		if n := count(`SELECT count(*) FROM afs.audit_log WHERE resource_id = $1`, tm.ID); n != 0 {
			t.Fatal("audit written for a refused completion")
		}
	})

	t.Run("not a team", func(t *testing.T) {
		if err := lc.Complete(ctx, lifecycle.Target{Kind: "agent", ID: "agt_x"}); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("unknown team", func(t *testing.T) {
		if err := lc.Complete(ctx, lifecycle.Target{Kind: "team", ID: pgtest.NewID("team")}); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("lists every secret row of the team and only its own", func(t *testing.T) {
		a, b := db.SeedTeam(t), db.SeedTeam(t)
		ins := func(team, name string) {
			// the ARN must end in the path the database derives, plus six characters
			if _, err := admin.Exec(ctx, `INSERT INTO afs.secrets (id, team_id, name, secrets_manager_arn, origin)
VALUES ($1, $2, $3, 'arn:aws:secretsmanager:us-east-1:111122223333:secret:' || afs.secret_path($2, NULL, $3) || '-AbCdEf', 'managed')`,
				pgtest.NewID("sec"), team, name); err != nil {
				t.Fatal(err)
			}
		}
		ins(a.ID, "one")
		ins(a.ID, "two")
		ins(b.ID, "one")
		got, err := lc.ListTeamSecrets(ctx, a.ID)
		if err != nil || len(got) != 2 {
			t.Fatalf("got %v err %v", got, err)
		}
	})
}

// M1-I1: the finalizer's condition store reads and writes resource_status, keeps the phase Deleting and
// publishes team.status_changed once when the phase first changes.
func TestConditionStoreOverResourceStatus(t *testing.T) {
	db := pgtest.Get(t)
	runner, _ := reconcilerRunner(t, db)
	st := postgres.NewConditionStore(postgres.NewStatusWriter(runner, postgres.NewOutboxWriter(), &fakes.IDGen{}, &fakes.Clock{T: time.Now()}), runner)
	admin := db.Admin(t)
	ctx := context.Background()
	tm := db.SeedTeam(t)
	tgt := lifecycle.Target{Kind: "team", ID: tm.ID, TeamID: tm.ID}

	got, err := st.Conditions(ctx, "team", tm.ID)
	if err != nil || len(got) != 0 {
		t.Fatalf("no row yet: %v %v", got, err)
	}
	conds := []domain.Condition{
		{Type: "DeleteScheduleSecretDeletion", Status: domain.ConditionTrue, Reason: "Done"},
		{Type: "DeleteNamespace", Status: domain.ConditionFalse, Reason: "Pending", Message: "terminating"},
	}
	for i := 0; i < 2; i++ {
		if err := st.Write(ctx, tgt, domain.PhaseDeleting, conds); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	got, err = st.Conditions(ctx, "team", tm.ID)
	byType := map[string]domain.Condition{}
	for _, c := range got {
		byType[c.Type] = c
	}
	if err != nil || len(got) != 2 || byType["DeleteNamespace"].Status != domain.ConditionFalse || byType["DeleteScheduleSecretDeletion"].Status != domain.ConditionTrue {
		t.Fatalf("conditions = %+v, %v", got, err)
	}
	var phase string
	if err := admin.QueryRow(ctx, `SELECT phase FROM afs.resource_status WHERE resource_kind = 'team' AND resource_id = $1`, tm.ID).Scan(&phase); err != nil || phase != "Deleting" {
		t.Fatalf("phase %q err %v", phase, err)
	}
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM afs.outbox_events WHERE aggregate_id = $1 AND event_type = 'team.status_changed'`, tm.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("status_changed events = %d (%v), want 1", n, err)
	}
}

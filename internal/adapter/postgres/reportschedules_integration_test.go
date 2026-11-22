package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/schedules"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

func seedChannel(t *testing.T, db *pgtest.DB, teamID string) string {
	t.Helper()
	id := pgtest.NewID("nch")
	var team any = teamID
	if teamID == "" {
		team = nil
	}
	if _, err := db.Admin(t).Exec(t.Context(), `INSERT INTO afs.notification_channels (id, team_id, name, kind, config) VALUES ($1, $2, $3, 'email', '{"to":["ops@example.com"]}')`,
		id, team, "ch-"+id); err != nil {
		t.Fatal(err)
	}
	return id
}

func billingTx(t *testing.T, db *pgtest.DB) *postgres.TxRunner {
	t.Helper()
	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_billing"], Service: "afs-billing", SystemActor: true, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return postgres.NewTxRunner(pool)
}

// M4-E2: schedule rows round trip with optimistic concurrency, unique names, team isolation and the
// same-team channel rule (RA-US-7a.1, RA-US-15.6).
func TestReportSchedulesRepo(t *testing.T) {
	db := pgtest.Get(t)
	repo := postgres.NewReportSchedulesRepo()
	a, b := db.SeedTeam(t), db.SeedTeam(t)
	chA, chB, chSvc := seedChannel(t, db, a.ID), seedChannel(t, db, b.ID), seedChannel(t, db, "")
	now := time.Now().UTC().Truncate(time.Second)
	mk := func(name, ch, team string) domain.ReportSchedule {
		s, err := domain.NewScheduleParams{Name: name, Report: domain.ReportKindUsage, Cadence: domain.CadenceWeekly, ChannelID: ch,
			Query: domain.ScheduleQuery{Range: "7d", GroupBy: []string{"agent", "tag:env"}, Filters: map[string]string{"tag.env": "prod"}}}.New(pgtest.NewID("rsc"), team, now)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s1, s2, foreign := mk("one", chA, a.ID), mk("two", chA, a.ID), mk("one", chB, b.ID)
	if err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		for _, s := range []domain.ReportSchedule{s1, s2, foreign} {
			if err := repo.Insert(ctx, tx, s); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A duplicate name in the team conflicts; the same name in another team is fine (inserted above).
	_ = inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		wantKind(t, repo.Insert(ctx, tx, mk("one", chA, a.ID)), domain.KindConflict)
		return context.Canceled
	})
	// The database refuses another team's channel and a service-level channel (composite foreign key).
	for name, ch := range map[string]string{"other team's channel": chB, "service channel": chSvc} {
		_ = inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
			if err := repo.Insert(ctx, tx, mk("x-"+name, ch, a.ID)); err == nil {
				t.Errorf("%s accepted", name)
			}
			return context.Canceled
		})
	}

	if err := inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		rows, err := repo.List(ctx, tx, a.ID, "", 10)
		if err != nil || len(rows) != 2 {
			t.Fatalf("list: %d %v", len(rows), err)
		}
		got, err := repo.Get(ctx, tx, a.ID, s1.ID, false)
		if err != nil || got.Name != "one" || got.Version != 1 || got.Query.Range != "7d" || got.Query.Filters["tag.env"] != "prod" ||
			len(got.Query.GroupBy) != 2 || got.Cadence != domain.CadenceWeekly || !got.NextRunAt.Equal(s1.NextRunAt) || got.Format != "csv" || got.LastStatus != "" {
			t.Fatalf("get: %+v %v", got, err)
		}
		if ok, _ := repo.ChannelOwnedByTeam(ctx, tx, a.ID, chA); !ok {
			t.Error("own channel not found")
		}
		for _, ch := range []string{chB, chSvc, pgtest.NewID("nch")} {
			if ok, err := repo.ChannelOwnedByTeam(ctx, tx, a.ID, ch); ok || err != nil {
				t.Errorf("channel %s owned by the team: %v %v", ch, ok, err)
			}
		}
		if _, err := repo.Get(ctx, tx, a.ID, foreign.ID, false); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("foreign schedule through the team path: %v", err)
		}
		if _, err := repo.Get(ctx, tx, b.ID, foreign.ID, false); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("another team's schedule: %v", err)
		}
		if ok, _ := repo.TeamExists(ctx, tx, b.ID); ok {
			t.Error("another team is visible")
		}
		if ok, _ := repo.TeamExists(ctx, tx, a.ID); !ok {
			t.Error("own team is not visible")
		}
		// Keyset paging.
		first, _ := repo.List(ctx, tx, a.ID, "", 1)
		rest, _ := repo.List(ctx, tx, a.ID, first[0].ID, 10)
		if len(first) != 1 || len(rest) != 1 || first[0].ID >= rest[0].ID {
			t.Errorf("paging: %v %v", first, rest)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A team caller cannot write another team's schedules.
	_ = inTx(t, db, teamSession(b.ID), func(ctx context.Context, tx port.Tx) error {
		if err := repo.Insert(ctx, tx, mk("sneaky", chA, a.ID)); err == nil {
			t.Error("a team inserted into another team")
		}
		return context.Canceled
	})

	// Update and delete with the version.
	if err := inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		next, err := s1.Apply(domain.SchedulePatch{Name: strp("renamed")}, now)
		if err != nil {
			return err
		}
		if _, err := repo.Update(ctx, tx, next, 9); domain.KindOf(err) != domain.KindPreconditionFailed {
			t.Errorf("stale update: %v", err)
		}
		up, err := repo.Update(ctx, tx, next, 1)
		if err != nil || up.Version != 2 || up.Name != "renamed" {
			t.Fatalf("update: %+v %v", up, err)
		}
		ghost := next
		ghost.ID = pgtest.NewID("rsc")
		if _, err := repo.Update(ctx, tx, ghost, 1); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("update of a missing schedule: %v", err)
		}
		wantKind(t, repo.Delete(ctx, tx, a.ID, s1.ID, 1), domain.KindPreconditionFailed)
		if err := repo.Delete(ctx, tx, a.ID, s1.ID, 2); err != nil {
			t.Errorf("delete: %v", err)
		}
		wantKind(t, repo.Delete(ctx, tx, a.ID, s1.ID, 2), domain.KindNotFound)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A channel in use cannot be deleted (RESTRICT): the API answers 409.
	if err := inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		_, err := postgres.NewChannelRepo().Get(ctx, tx, a.ID, chA, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Admin(t).Exec(t.Context(), `DELETE FROM afs.notification_channels WHERE id = $1`, chA); err == nil {
		t.Error("a channel named by a schedule was deleted")
	}
}

func strp(s string) *string { return &s }

// M4-E3: the runner's queue works under the billing role: due rows only, claims skip rows another
// runner holds, and the outcome writes only the four columns afs_billing may update.
func TestReportScheduleRunStore(t *testing.T) {
	db := pgtest.Get(t)
	repo := postgres.NewReportSchedulesRepo()
	tm := db.SeedTeam(t)
	ch := seedChannel(t, db, tm.ID)
	past := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	insert := func(name string, enabled bool, next time.Time) string {
		id := pgtest.NewID("rsc")
		if _, err := db.Admin(t).Exec(t.Context(), `INSERT INTO afs.report_schedules (id, team_id, name, report, query, cadence, channel_id, enabled, next_run_at)
VALUES ($1, $2, $3, 'usage', '{"range":"7d"}', 'daily', $4, $5, $6)`, id, tm.ID, name, ch, enabled, next); err != nil {
			t.Fatal(err)
		}
		return id
	}
	due1 := insert("due1", true, past)
	due2 := insert("due2", true, past.Add(time.Minute))
	_ = insert("future", true, time.Now().Add(time.Hour))
	_ = insert("off", false, past)

	runner := billingTx(t, db)
	sess := port.SystemSession("billing")
	now := time.Now().UTC()
	var ids []string
	if err := runner.ReadTx(t.Context(), sess, func(ctx context.Context, tx port.Tx) error {
		var err error
		ids, err = repo.DueIDs(ctx, tx, now, 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != due1 || ids[1] != due2 {
		t.Fatalf("due = %v, want %s then %s", ids, due1, due2)
	}

	// While one runner holds a claim, a second runner skips the row.
	holding, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runner.InTx(context.Background(), sess, func(ctx context.Context, tx port.Tx) error {
			_, ok, err := repo.Claim(ctx, tx, due1, now)
			if err != nil || !ok {
				t.Errorf("first claim: %v %v", ok, err)
			}
			close(holding)
			<-release
			return repo.Record(ctx, tx, due1, schedules.Outcome{Status: domain.ScheduleSent, RunAt: now, NextRunAt: ptrTime(domain.NextRun(domain.CadenceDaily, now))})
		})
	}()
	<-holding
	if err := runner.InTx(t.Context(), sess, func(ctx context.Context, tx port.Tx) error {
		if _, ok, err := repo.Claim(ctx, tx, due1, now); ok || err != nil {
			t.Errorf("a locked schedule was claimed: %v %v", ok, err)
		}
		if _, ok, err := repo.Claim(ctx, tx, due2, now); !ok || err != nil {
			t.Errorf("an unlocked schedule was not claimed: %v %v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// The sent run advanced next_run_at and cleared the error; the schedule is no longer due.
	if err := runner.ReadTx(t.Context(), sess, func(ctx context.Context, tx port.Tx) error {
		s, err := repo.Get(ctx, tx, tm.ID, due1, false)
		if err != nil {
			return err
		}
		if s.LastStatus != domain.ScheduleSent || s.LastError != "" || s.LastRunAt == nil || !s.NextRunAt.After(now) {
			t.Errorf("after a sent run: %+v", s)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.InTx(t.Context(), sess, func(ctx context.Context, tx port.Tx) error {
		if _, ok, err := repo.Claim(ctx, tx, due1, now); ok || err != nil {
			t.Errorf("a schedule that ran was claimed again: %v %v", ok, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// A failed run records the error and leaves next_run_at; it stays due.
	long := make([]byte, 700)
	for i := range long {
		long[i] = 'e'
	}
	if err := runner.InTx(t.Context(), sess, func(ctx context.Context, tx port.Tx) error {
		return repo.Record(ctx, tx, due2, schedules.Outcome{Status: domain.ScheduleFailed, Error: string(long), RunAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.ReadTx(t.Context(), sess, func(ctx context.Context, tx port.Tx) error {
		s, err := repo.Get(ctx, tx, tm.ID, due2, false)
		if err != nil {
			return err
		}
		if s.LastStatus != domain.ScheduleFailed || len(s.LastError) != schedules.MaxErrorLength || !s.NextRunAt.Equal(past.Add(time.Minute)) {
			t.Errorf("after a failed run: status=%s error=%d bytes next=%s", s.LastStatus, len(s.LastError), s.NextRunAt)
		}
		ids, err = repo.DueIDs(ctx, tx, now, 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != due2 {
		t.Errorf("due after the runs = %v, want only %s (retry)", ids, due2)
	}
	_ = runner.InTx(t.Context(), sess, func(ctx context.Context, tx port.Tx) error {
		wantKind(t, repo.Record(ctx, tx, pgtest.NewID("rsc"), schedules.Outcome{Status: domain.ScheduleSent, RunAt: now}), domain.KindNotFound)
		return context.Canceled
	})
}

func ptrTime(t time.Time) *time.Time { return &t }

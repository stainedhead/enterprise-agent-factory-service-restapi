package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// M3-E: budget rows round trip with optimistic concurrency, the database keeps the ceiling and the
// notify guard behind the use case, another team sees nothing, and the counter of the current period
// is read.
func TestBudgetRepo(t *testing.T) {
	db := pgtest.Get(t)
	repo := postgres.NewBudgetRepo()
	a, b := db.SeedTeam(t), db.SeedTeam(t) // each has a ceiling of 1000
	now := time.Now().UTC()
	put := func(amount string) domain.PutBudgetParams {
		return domain.PutBudgetParams{Period: domain.BudgetPeriodMonthly, Amount: domain.MustMoney(amount), Thresholds: []int{50, 80, 100}}
	}
	tm := domain.BudgetTeam{Kind: domain.TeamKindPersistent, MaxBudget: domain.MustMoney("1000")}
	first, err := domain.NewBudget(pgtest.NewID("bud"), a.ID, put("400"), tm, now)
	if err != nil {
		t.Fatal(err)
	}

	if err := inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		got, err := repo.Team(ctx, tx, a.ID)
		if err != nil || got.Kind != domain.TeamKindPersistent || got.MaxBudget.String() != "1000.00" || got.CeilingOverride != nil {
			t.Errorf("team: %+v %v", got, err)
		}
		if _, err := repo.Get(ctx, tx, a.ID, false); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("no budget yet: %v", err)
		}
		if err := repo.Insert(ctx, tx, first); err != nil {
			return err
		}
		stored, err := repo.Get(ctx, tx, a.ID, true)
		if err != nil {
			return err
		}
		if stored.Version != 1 || stored.Amount.String() != "400.00" || stored.Period != domain.BudgetPeriodMonthly ||
			stored.OnExhaust != domain.OnExhaustBlock || len(stored.Thresholds) != 3 || stored.Thresholds[1] != 80 ||
			stored.CreatedBy != user || stored.ExpiresAt != nil || !stored.StartsAt.Truncate(time.Microsecond).Equal(first.StartsAt.Truncate(time.Microsecond)) {
			t.Errorf("stored: %+v", stored)
		}
		// One budget per team.
		dup, _ := domain.NewBudget(pgtest.NewID("bud"), a.ID, put("100"), tm, now)
		wantKind(t, repo.Insert(ctx, tx, dup), domain.KindConflict)
		return context.Canceled // roll back the failed insert's transaction
	}); err == nil {
		t.Fatal("expected the rollback")
	}
	if err := inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error { return repo.Insert(ctx, tx, first) }); err != nil {
		t.Fatal(err)
	}

	// Update: the version is bumped, a stale version is PreconditionFailed, the ceiling still holds.
	if err := inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		cur, err := repo.Get(ctx, tx, a.ID, true)
		if err != nil {
			return err
		}
		next, err := cur.Replace(domain.PutBudgetParams{Period: domain.BudgetPeriodLifetime, Amount: domain.MustMoney("900"), Thresholds: []int{90}}, tm, now)
		if err != nil {
			return err
		}
		if err := repo.Update(ctx, tx, next, cur.Version); err != nil {
			return err
		}
		got, err := repo.Get(ctx, tx, a.ID, false)
		if err != nil {
			return err
		}
		if got.Version != 2 || got.Amount.String() != "900.00" || got.Period != domain.BudgetPeriodLifetime || len(got.Thresholds) != 1 || got.Thresholds[0] != 90 {
			t.Errorf("updated: %+v", got)
		}
		wantKind(t, repo.Update(ctx, tx, next, 1), domain.KindPreconditionFailed)
		over := next
		over.Amount = domain.MustMoney("1000.01") // the domain would refuse it; the database trigger must too
		wantKind(t, repo.Update(ctx, tx, over, got.Version), domain.KindValidation)
		return context.Canceled
	}); err == nil {
		t.Fatal("expected the rollback")
	}

	// Notify: a service admin chooses it. (The 000013 guard against other callers tests current_user =
	// 'afs_app', which the per-run LOGIN users of pgtest are not, so the use case's own check is what
	// the unit and end-to-end tests prove.)
	if err := inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		cur, err := repo.Get(ctx, tx, a.ID, true)
		if err != nil {
			return err
		}
		cur.OnExhaust = domain.OnExhaustNotify
		return repo.Update(ctx, tx, cur, cur.Version)
	}); err != nil {
		t.Fatalf("service admin chooses notify: %v", err)
	}

	// Another team's session sees neither the budget nor the team.
	_ = inTx(t, db, teamSession(b.ID), func(ctx context.Context, tx port.Tx) error {
		_, err := repo.Get(ctx, tx, a.ID, false)
		wantKind(t, err, domain.KindNotFound)
		_, err = repo.Team(ctx, tx, a.ID)
		wantKind(t, err, domain.KindNotFound)
		return nil
	})

	// Counter: none yet is the zero counter; with one for the period, its spent and reserved amount.
	var id string
	var start time.Time
	_ = inTx(t, db, adminSession(), func(ctx context.Context, tx port.Tx) error {
		cur, err := repo.Get(ctx, tx, a.ID, false)
		if err != nil {
			return err
		}
		id = cur.ID
		start, _ = cur.CurrentPeriod(now)
		c, err := repo.Counter(ctx, tx, id, start)
		if err != nil || c.Spent.Micros() != 0 || c.Reserved.Micros() != 0 {
			t.Errorf("no counter: %+v %v", c, err)
		}
		return nil
	})
	if _, err := db.Admin(t).Exec(t.Context(), `INSERT INTO afs.spend_counters (budget_id, period_start, limit_usd, spent_usd, reserved_usd) VALUES ($1, $2, 900, 12.345678, 3.2)`, id, start); err != nil {
		t.Fatal(err)
	}
	_ = inTx(t, db, teamSession(a.ID), func(ctx context.Context, tx port.Tx) error {
		c, err := repo.Counter(ctx, tx, id, start)
		if err != nil || c.Spent.String() != "12.345678" || c.Reserved.String() != "3.20" {
			t.Errorf("counter: %+v %v", c, err)
		}
		return nil
	})
	_ = inTx(t, db, teamSession(b.ID), func(ctx context.Context, tx port.Tx) error {
		c, err := repo.Counter(ctx, tx, id, start)
		if err != nil || c.Spent.Micros() != 0 {
			t.Errorf("another team reads another team's counter: %+v %v", c, err)
		}
		return nil
	})
}

// Package budget is the reconciler's budget work (RC-6, RC-14): it opens each budget's spend period,
// carries a changed amount into the period's limit, sweeps reservations a dead gateway left behind,
// expires ceiling overrides, and is the backstop behind the gateway's hard cap.
//
// Everything is a periodic leader-only job (Register), set-based and idempotent, so a lost leader or a
// repeated pass changes nothing. The gateway enforces the cap per call by reserving spend before it
// invokes a model; the backstop is the second line: while a block-mode budget is spent, the team's
// Bedrock role carries an explicit deny of model invocation and the team, its agents and its pools carry
// the condition BudgetExhausted=True (shown as "Budget blocked"). Both are removed within a pass of the
// budget being raised, an override being set or the period rolling over.
//
// One pass (Reconcile) runs in this order, because each step feeds the next: expire overrides (lowers
// a budget above the base ceiling), carry amounts into limits, open periods, apply or lift the backstop.
package budget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Intervals of the jobs.
const (
	// Interval is how often a reconcile pass runs. A budget change, an override expiry or an exhaustion
	// is seen within one interval and acted on in the same pass, so the 60 s of RC-US-12.2 and RC-US-12.3
	// hold with room for a slow pass.
	Interval = 30 * time.Second
	// SweepInterval is how often expired reservations are released (RC-US-12.4).
	SweepInterval = time.Minute
	// PreOpen is how long before a monthly period starts its counter is opened, so it exists by the
	// first second of the period even when a pass is late.
	PreOpen = 2 * time.Minute
)

// LimitChange is a counter whose limit now follows its budget.
type LimitChange struct {
	BudgetID string
	TeamID   string
	// LimitUSD is the new limit.
	LimitUSD domain.Money
}

// OverrideExpiry is a ceiling override that was cleared.
type OverrideExpiry struct {
	TeamID string
	// BaseCeiling is the team's maxBudgetUsd, the ceiling in force after the expiry.
	BaseCeiling domain.Money
	// Lowered is true when the team's budget was above the base ceiling and was lowered to it.
	Lowered bool
}

// TeamBudget is a team whose backstop state may need to change: its block-mode budget is spent, or it
// still carries the condition from an earlier exhaustion.
type TeamBudget struct {
	TeamID    string
	Namespace string
	// Exhausted is true while the team's budget is in block mode and the period's spend has reached its limit.
	Exhausted bool
	// Marked is true while the team, one of its agents or one of its pools carries BudgetExhausted=True.
	Marked bool
}

// Store is the database side of the jobs. Each method is one transaction and is safe to repeat.
type Store interface {
	// LiveBudgets returns the budgets of teams that are not deleting or deleted. Only ID, TeamID,
	// Period and StartsAt are set.
	LiveBudgets(ctx context.Context) ([]domain.Budget, error)
	// OpenPeriod makes sure the counter of the period starting at start exists (afs.ensure_spend_counter).
	// With announce it also publishes budget.reset for that period and audits it, once: a period that is
	// already announced (by this or an earlier leader) is left alone. It reports whether it published.
	OpenPeriod(ctx context.Context, b domain.Budget, start time.Time, announce bool) (announced bool, err error)
	// CarryLimits sets limit_usd of each live budget's current and later counters to the budget's amount
	// and returns the counters it changed.
	CarryLimits(ctx context.Context, now time.Time) ([]LimitChange, error)
	// ReleaseExpired releases reservations past their expiry (afs.release_expired_reservations) and
	// returns how many; a non-zero count is audited in the same transaction.
	ReleaseExpired(ctx context.Context) (int64, error)
	// ExpireOverrides clears every ceiling override that has expired at now, lowering a budget above the
	// base ceiling to it first, and publishes budget.ceiling_override_expired and an audit record, all in
	// one transaction per team, so each expiry is announced once.
	ExpireOverrides(ctx context.Context, now time.Time) ([]OverrideExpiry, error)
	// Exhaustion returns the teams whose backstop may need to change at now.
	Exhaustion(ctx context.Context, now time.Time) ([]TeamBudget, error)
	// MarkExhausted sets BudgetExhausted=True on the status of the team, its agents and its pools that
	// lack it. A transition (some row changed) is audited.
	MarkExhausted(ctx context.Context, teamID string) error
	// ClearExhausted removes BudgetExhausted from them. A transition is audited.
	ClearExhausted(ctx context.Context, teamID string) error
}

// RoleDeny places and lifts the explicit deny on a team's Bedrock role (aws/iam). Both are idempotent.
type RoleDeny interface {
	DenyBedrock(ctx context.Context, path, roleName string) error
	LiftBedrockDeny(ctx context.Context, path, roleName string) error
}

// Reconciler runs the budget jobs.
type Reconciler struct {
	Store Store
	IAM   RoleDeny
	Clock func() time.Time
	Log   *slog.Logger
	// Released, when set, receives the number of reservations each sweep released (the alert metric).
	Released func(n int64)

	mu        sync.Mutex
	opened    map[string]time.Time // budget id → start of the period last opened and announced
	preopened map[string]time.Time // budget id → start of the next period last pre-opened
}

func (r *Reconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock().UTC()
	}
	return time.Now().UTC()
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// Reconcile runs one pass. A failing step does not stop the later ones; the errors are joined.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	now := r.now()
	var errs []error
	if err := r.expireOverrides(ctx, now); err != nil {
		errs = append(errs, err)
	}
	if err := r.carryLimits(ctx, now); err != nil {
		errs = append(errs, err)
	}
	if err := r.openPeriods(ctx, now); err != nil {
		errs = append(errs, err)
	}
	if err := r.backstop(ctx, now); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r *Reconciler) expireOverrides(ctx context.Context, now time.Time) error {
	done, err := r.Store.ExpireOverrides(ctx, now)
	for _, o := range done {
		r.log().InfoContext(ctx, "ceiling override expired", "team", o.TeamID, "baseCeilingUsd", o.BaseCeiling.String(), "budgetLowered", o.Lowered)
	}
	if err != nil {
		return fmt.Errorf("budget: expire ceiling overrides: %w", err)
	}
	return nil
}

func (r *Reconciler) carryLimits(ctx context.Context, now time.Time) error {
	changed, err := r.Store.CarryLimits(ctx, now)
	for _, c := range changed {
		r.log().InfoContext(ctx, "budget limit carried into the spend counter", "team", c.TeamID, "budget", c.BudgetID, "limitUsd", c.LimitUSD.String())
	}
	if err != nil {
		return fmt.Errorf("budget: carry limits: %w", err)
	}
	return nil
}

// openPeriods makes each budget's current counter exist and announces the period once. A monthly
// budget's next counter is opened PreOpen ahead of the boundary.
func (r *Reconciler) openPeriods(ctx context.Context, now time.Time) error {
	budgets, err := r.Store.LiveBudgets(ctx)
	if err != nil {
		return fmt.Errorf("budget: list budgets: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.opened == nil {
		r.opened, r.preopened = map[string]time.Time{}, map[string]time.Time{}
	}
	live := make(map[string]bool, len(budgets))
	var errs []error
	for _, b := range budgets {
		live[b.ID] = true
		start, end := b.CurrentPeriod(now)
		if !r.opened[b.ID].Equal(start) {
			if _, err := r.Store.OpenPeriod(ctx, b, start, true); err != nil {
				errs = append(errs, fmt.Errorf("budget: open period of %s: %w", b.ID, err))
			} else {
				r.opened[b.ID] = start
			}
		}
		if b.Period == domain.BudgetPeriodMonthly && end != nil && end.Sub(now) <= PreOpen && !r.preopened[b.ID].Equal(*end) {
			if _, err := r.Store.OpenPeriod(ctx, b, *end, false); err != nil {
				errs = append(errs, fmt.Errorf("budget: pre-open next period of %s: %w", b.ID, err))
			} else {
				r.preopened[b.ID] = *end
			}
		}
	}
	for id := range r.opened {
		if !live[id] {
			delete(r.opened, id)
			delete(r.preopened, id)
		}
	}
	return errors.Join(errs...)
}

// backstop applies the deny and the condition to every team whose budget is spent, and lifts both
// from teams that carry them but are no longer spent. The condition is set before the deny and the deny
// lifted before the condition is cleared, so a pass that stops half way leaves a state the next pass
// repairs, never a deny that nothing records.
func (r *Reconciler) backstop(ctx context.Context, now time.Time) error {
	teams, err := r.Store.Exhaustion(ctx, now)
	if err != nil {
		return fmt.Errorf("budget: find exhausted budgets: %w", err)
	}
	var errs []error
	for _, tb := range teams {
		if err := r.backstopTeam(ctx, tb); err != nil {
			errs = append(errs, fmt.Errorf("budget: backstop of %s: %w", tb.TeamID, err))
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) backstopTeam(ctx context.Context, tb TeamBudget) error {
	if !tb.Exhausted && !tb.Marked {
		return nil
	}
	role, err := team.BedrockRoleName(tb.TeamID)
	if err != nil {
		return err
	}
	path := team.RolePath(tb.Namespace)
	if tb.Exhausted {
		if err := r.Store.MarkExhausted(ctx, tb.TeamID); err != nil {
			return err
		}
		return r.IAM.DenyBedrock(ctx, path, role)
	}
	if err := r.IAM.LiftBedrockDeny(ctx, path, role); err != nil {
		return err
	}
	return r.Store.ClearExhausted(ctx, tb.TeamID)
}

// Sweep releases reservations past their expiry (RC-US-12.4). A gateway that was killed between
// reserving and settling leaves holds that block the budget; they expire after
// gateway.reservation_ttl_seconds and this sweep, every minute, frees them, so a hold is released within
// the TTL plus two minutes. A release is audited by the store and counted for the alert on it.
func (r *Reconciler) Sweep(ctx context.Context) error {
	n, err := r.Store.ReleaseExpired(ctx)
	if err != nil {
		return fmt.Errorf("budget: release expired reservations: %w", err)
	}
	if n > 0 {
		r.log().WarnContext(ctx, "released expired spend reservations", "count", n)
		if r.Released != nil {
			r.Released(n)
		}
	}
	return nil
}

// Jobs returns the leader-only jobs of the reconciler.
func (r *Reconciler) Jobs() []engine.Job {
	return []engine.Job{
		{Name: "budget-reconcile", Every: Interval, Immediate: true, Fn: r.Reconcile},
		{Name: "reservation-sweeper", Every: SweepInterval, Immediate: true, Fn: r.Sweep},
	}
}

// Adder is the part of the engine Register needs.
type Adder interface{ AddJob(engine.Job) }

// Register adds the jobs to the engine.
func (r *Reconciler) Register(e Adder) {
	for _, j := range r.Jobs() {
		e.AddJob(j)
	}
}

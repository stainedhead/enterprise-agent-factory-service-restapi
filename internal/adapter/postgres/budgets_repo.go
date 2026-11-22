package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/budgets"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// BudgetRepo implements budgets.Repository on afs.budgets and afs.spend_counters. Every method runs
// in the transaction it is given, so row-level security applies with the caller's session: a team's
// budget is invisible to anyone not bound to the team (a service admin sees all).
type BudgetRepo struct{}

// NewBudgetRepo returns a BudgetRepo.
func NewBudgetRepo() *BudgetRepo { return &BudgetRepo{} }

var _ budgets.Repository = (*BudgetRepo)(nil)

const budgetSelect = `SELECT id, team_id, period, amount_usd::text, thresholds, on_exhaust, starts_at, expires_at,
  version, created_at, created_by, updated_at FROM afs.budgets`

func scanBudget(row pgx.Row) (domain.Budget, error) {
	var (
		b       domain.Budget
		period  string
		amount  string
		th      []int16
		exhaust string
		version int64
	)
	if err := row.Scan(&b.ID, &b.TeamID, &period, &amount, &th, &exhaust, &b.StartsAt, &b.ExpiresAt,
		&version, &b.CreatedAt, &b.CreatedBy, &b.UpdatedAt); err != nil {
		return domain.Budget{}, err
	}
	m, err := domain.ParseMoney("amountUsd", amount)
	if err != nil {
		return domain.Budget{}, err
	}
	b.Period, b.Amount, b.OnExhaust, b.Version = domain.BudgetPeriod(period), m, domain.OnExhaust(exhaust), domain.Version(version)
	b.Thresholds = make([]int, len(th))
	for i, v := range th {
		b.Thresholds[i] = int(v)
	}
	return b, nil
}

func thresholds(b domain.Budget) []int16 {
	out := make([]int16, len(b.Thresholds))
	for i, v := range b.Thresholds {
		out[i] = int16(v) //nolint:gosec // validated to 1..100 by domain.PutBudgetParams
	}
	return out
}

// Team implements budgets.Repository.
func (*BudgetRepo) Team(ctx context.Context, tx port.Tx, teamID string) (domain.BudgetTeam, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.BudgetTeam{}, err
	}
	var (
		t        domain.BudgetTeam
		kind     string
		maxB     string
		override *string
	)
	err = pt.QueryRow(ctx, `SELECT kind, max_budget_usd::text, ceiling_override_usd::text, ceiling_override_expires_at
FROM afs.teams WHERE id = $1`, teamID).Scan(&kind, &maxB, &override, &t.CeilingOverrideExpiresAt)
	if err != nil {
		return domain.BudgetTeam{}, MapError(err, OpRead, "team")
	}
	t.Kind = domain.TeamKind(kind)
	if t.MaxBudget, err = domain.ParseMoney("maxBudgetUsd", maxB); err != nil {
		return domain.BudgetTeam{}, err
	}
	if override != nil {
		m, err := domain.ParseMoney("ceilingOverrideUsd", *override)
		if err != nil {
			return domain.BudgetTeam{}, err
		}
		t.CeilingOverride = &m
	}
	return t, nil
}

// Get implements budgets.Repository.
func (*BudgetRepo) Get(ctx context.Context, tx port.Tx, teamID string, forUpdate bool) (domain.Budget, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.Budget{}, err
	}
	q := budgetSelect + ` WHERE team_id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	b, err := scanBudget(pt.QueryRow(ctx, q, teamID))
	if err != nil {
		return domain.Budget{}, MapError(err, OpRead, "budget")
	}
	return b, nil
}

// Insert implements budgets.Repository. A second budget for a team is a Conflict (one per team).
func (*BudgetRepo) Insert(ctx context.Context, tx port.Tx, b domain.Budget) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.budgets (id, team_id, period, amount_usd, thresholds, on_exhaust, starts_at, expires_at)
VALUES ($1, $2, $3, $4::numeric, $5, $6, $7, $8)`, b.ID, b.TeamID, string(b.Period), b.Amount.String(), thresholds(b),
		string(b.OnExhaust), b.StartsAt.Truncate(time.Microsecond), b.ExpiresAt)
	return MapError(err, OpWrite, "budget")
}

// Update implements budgets.Repository.
func (*BudgetRepo) Update(ctx context.Context, tx port.Tx, b domain.Budget, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.budgets SET period = $2, amount_usd = $3::numeric, thresholds = $4, on_exhaust = $5, expires_at = $6
WHERE id = $1 AND version = $7`, b.ID, string(b.Period), b.Amount.String(), thresholds(b), string(b.OnExhaust), b.ExpiresAt, int64(expect))
	if err != nil {
		return MapError(err, OpWrite, "budget")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "budget", `SELECT 1 FROM afs.budgets WHERE id = $1`, b.ID)
	}
	return nil
}

// Counter implements budgets.Repository.
func (*BudgetRepo) Counter(ctx context.Context, tx port.Tx, budgetID string, periodStart time.Time) (domain.SpendCounter, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.SpendCounter{}, err
	}
	var spent, reserved string
	err = pt.QueryRow(ctx, `SELECT spent_usd::text, reserved_usd::text FROM afs.spend_counters WHERE budget_id = $1 AND period_start = $2`,
		budgetID, periodStart).Scan(&spent, &reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SpendCounter{}, nil
	}
	if err != nil {
		return domain.SpendCounter{}, MapError(err, OpRead, "spend counter")
	}
	var c domain.SpendCounter
	if c.Spent, err = domain.ParseMoney("spentUsd", spent); err != nil {
		return domain.SpendCounter{}, err
	}
	if c.Reserved, err = domain.ParseMoney("reservedUsd", reserved); err != nil {
		return domain.SpendCounter{}, err
	}
	return c, nil
}

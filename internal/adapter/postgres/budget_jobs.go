package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/budget"
)

// BudgetJobs implements budget.Store for the reconciler's login (afs_reconciler, database contract §3):
// it opens spend periods through afs.ensure_spend_counter, updates limit_usd (the only spend column it
// may update), releases reservations through afs.release_expired_reservations, clears expired ceiling
// overrides after lowering the budget above the base ceiling, and writes the BudgetExhausted condition to
// resource_status. Every method is one transaction and safe to repeat.
type BudgetJobs struct {
	runner *TxRunner
	audit  port.AuditWriter
	outbox port.OutboxWriter
	ids    port.IDGen
}

// NewBudgetJobs returns a BudgetJobs on the reconciler's pool.
func NewBudgetJobs(runner *TxRunner, audit port.AuditWriter, outbox port.OutboxWriter, ids port.IDGen) *BudgetJobs {
	return &BudgetJobs{runner: runner, audit: audit, outbox: outbox, ids: ids}
}

var _ budget.Store = (*BudgetJobs)(nil)

// Audit actions of the budget reconciler.
const (
	// AuditPeriodOpened marks that budget.reset was published for a budget's period; the next leader
	// finds the mark and does not publish it again.
	AuditPeriodOpened = "budget.period_opened"
	// AuditReleasedReservations records a sweep that released at least one reservation.
	AuditReleasedReservations = "ReleaseExpiredReservations"
	// AuditOverrideExpired records a ceiling override the reconciler cleared.
	AuditOverrideExpired = "ExpireCeilingOverride"
	// AuditBudgetExhausted and AuditBudgetRestored record a team entering and leaving the backstop.
	AuditBudgetExhausted = "BudgetExhausted"
	AuditBudgetRestored  = "BudgetRestored"
)

// currentPeriodSQL is the start of a budget's current period as a SQL expression over the aliases b
// (afs.budgets) and the parameter $1 (now): the UTC month of a monthly budget, starts_at of a lifetime
// one. It is budget.CurrentPeriod in SQL.
const currentPeriodSQL = `(CASE b.period WHEN 'monthly' THEN date_trunc('month', $1::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' ELSE b.starts_at END)`

// LiveBudgets implements budget.Store.
func (j *BudgetJobs) LiveBudgets(ctx context.Context) ([]domain.Budget, error) {
	var out []domain.Budget
	err := j.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT b.id, b.team_id, b.period, b.starts_at FROM afs.budgets b
JOIN afs.teams t ON t.id = b.team_id AND t.state NOT IN ('deleting', 'deleted') ORDER BY b.id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b domain.Budget
			var period string
			if err := rows.Scan(&b.ID, &b.TeamID, &period, &b.StartsAt); err != nil {
				return err
			}
			b.Period = domain.BudgetPeriod(period)
			out = append(out, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, MapError(err, OpRead, "budgets")
	}
	return out, nil
}

// OpenPeriod implements budget.Store.
func (j *BudgetJobs) OpenPeriod(ctx context.Context, b domain.Budget, start time.Time, announce bool) (bool, error) {
	published := false
	err := j.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		published = false
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var limit string
		if err := t.QueryRow(ctx, `SELECT afs.ensure_spend_counter($1, $2)::text`, b.ID, start).Scan(&limit); err != nil {
			return err
		}
		if !announce {
			return nil
		}
		stamp := start.UTC().Format(time.RFC3339)
		var seen int
		err = t.QueryRow(ctx, `SELECT 1 FROM afs.audit_log WHERE team_id = $1 AND action = $2 AND resource_id = $3
  AND after ->> 'periodStart' = $4 LIMIT 1`, b.TeamID, AuditPeriodOpened, b.ID, stamp).Scan(&seen)
		if err == nil {
			return nil // announced by this or an earlier leader
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		lim, err := domain.ParseMoney("limitUsd", limit)
		if err != nil {
			return err
		}
		data := map[string]any{"budgetId": b.ID, "period": string(b.Period), "periodStart": stamp, "limitUsd": lim.String()}
		if err := j.audit.Write(ctx, tx, port.AuditRecord{Action: AuditPeriodOpened, Outcome: port.OutcomeAllowed,
			ResourceKind: "budgets", ResourceID: b.ID, TeamID: b.TeamID, After: data}); err != nil {
			return err
		}
		if err := j.emit(ctx, tx, domain.EventBudgetReset, b.TeamID, b.ID, data); err != nil {
			return err
		}
		published = true
		return nil
	})
	if err != nil {
		return false, MapError(err, OpWrite, "spend counter")
	}
	return published, nil
}

// emit publishes a budget event with the budget as its aggregate.
func (j *BudgetJobs) emit(ctx context.Context, tx port.Tx, typ domain.EventType, teamID, budgetID string, data map[string]any) error {
	id, err := j.ids.NewID(eventIDPrefix)
	if err != nil {
		return err
	}
	ev, err := domain.NewEvent(typ, id.String(), time.Now(), tx.Session().Actor, teamID, "budget", budgetID, data)
	if err != nil {
		return err
	}
	return j.outbox.Publish(ctx, tx, ev)
}

// CarryLimits implements budget.Store.
func (j *BudgetJobs) CarryLimits(ctx context.Context, now time.Time) ([]budget.LimitChange, error) {
	var out []budget.LimitChange
	err := j.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		out = nil
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `UPDATE afs.spend_counters c SET limit_usd = b.amount_usd
FROM afs.budgets b JOIN afs.teams tm ON tm.id = b.team_id AND tm.state NOT IN ('deleting', 'deleted')
WHERE c.budget_id = b.id AND c.limit_usd <> b.amount_usd AND c.period_start >= `+currentPeriodSQL+`
RETURNING c.budget_id, b.team_id, c.limit_usd::text`, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c budget.LimitChange
			var lim string
			if err := rows.Scan(&c.BudgetID, &c.TeamID, &lim); err != nil {
				return err
			}
			if c.LimitUSD, err = domain.ParseMoney("limitUsd", lim); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, MapError(err, OpWrite, "spend counter")
	}
	return out, nil
}

// ReleaseExpired implements budget.Store.
func (j *BudgetJobs) ReleaseExpired(ctx context.Context) (int64, error) {
	var n int64
	err := j.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		if err := t.QueryRow(ctx, `SELECT afs.release_expired_reservations()`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		return j.audit.Write(ctx, tx, port.AuditRecord{Action: AuditReleasedReservations, Outcome: port.OutcomeAllowed,
			ResourceKind: "spend_reservations", After: map[string]any{"released": n}})
	})
	if err != nil {
		return 0, MapError(err, OpWrite, "spend reservation")
	}
	return n, nil
}

// ExpireOverrides implements budget.Store: one transaction per team, so one team's failure leaves the
// others expired and the failing team's override in place for the next pass.
func (j *BudgetJobs) ExpireOverrides(ctx context.Context, now time.Time) ([]budget.OverrideExpiry, error) {
	var ids []string
	err := j.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT id FROM afs.teams WHERE ceiling_override_expires_at IS NOT NULL
  AND ceiling_override_expires_at <= $1 ORDER BY id`, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, MapError(err, OpRead, "teams")
	}
	var out []budget.OverrideExpiry
	var errs []error
	for _, id := range ids {
		var done *budget.OverrideExpiry
		err := j.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
			var err error
			done, err = j.expireOverride(ctx, tx, id, now)
			return err
		})
		if err != nil {
			errs = append(errs, MapError(err, OpWrite, "team"))
			continue
		}
		if done != nil {
			out = append(out, *done)
		}
	}
	return out, errors.Join(errs...)
}

func (j *BudgetJobs) expireOverride(ctx context.Context, tx port.Tx, teamID string, now time.Time) (*budget.OverrideExpiry, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	var base, over *string
	var expires *time.Time
	err = t.QueryRow(ctx, `SELECT max_budget_usd::text, ceiling_override_usd::text, ceiling_override_expires_at
FROM afs.teams WHERE id = $1 FOR UPDATE`, teamID).Scan(&base, &over, &expires)
	if err != nil {
		return nil, err
	}
	if expires == nil || expires.After(now) { // renewed or cleared since the scan
		return nil, nil
	}
	baseMoney, err := domain.ParseMoney("maxBudgetUsd", *base)
	if err != nil {
		return nil, err
	}
	// Lower the budget first: the ceiling in force may not fall below it (database guard).
	var budgetID string
	var before string
	lowered := false
	err = t.QueryRow(ctx, `SELECT id, amount_usd::text FROM afs.budgets WHERE team_id = $1 AND amount_usd > $2::numeric FOR UPDATE`, teamID, *base).Scan(&budgetID, &before)
	switch {
	case err == nil:
		if _, err := t.Exec(ctx, `UPDATE afs.budgets SET amount_usd = $2::numeric WHERE id = $1`, budgetID, *base); err != nil {
			return nil, err
		}
		lowered = true
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, err
	}
	if _, err := t.Exec(ctx, `UPDATE afs.teams SET ceiling_override_usd = NULL, ceiling_override_expires_at = NULL WHERE id = $1`, teamID); err != nil {
		return nil, err
	}
	data := map[string]any{"baseCeilingUsd": baseMoney.String(), "budgetLowered": lowered}
	beforeAudit := map[string]any{"ceilingOverrideUsd": over, "ceilingOverrideExpiresAt": expires.UTC().Format(time.RFC3339)}
	afterAudit := map[string]any{"baseCeilingUsd": baseMoney.String(), "budgetLowered": lowered}
	if lowered {
		data["budgetId"], data["previousAmountUsd"] = budgetID, before
		afterAudit["budgetId"], afterAudit["previousAmountUsd"] = budgetID, before
	}
	if err := j.audit.Write(ctx, tx, port.AuditRecord{Action: AuditOverrideExpired, Outcome: port.OutcomeAllowed,
		ResourceKind: "teams", ResourceID: teamID, TeamID: teamID, Before: beforeAudit, After: afterAudit}); err != nil {
		return nil, err
	}
	aggregate := teamID
	if lowered {
		aggregate = budgetID
	}
	if err := j.emit(ctx, tx, domain.EventBudgetCeilingOverrideExpired, teamID, aggregate, data); err != nil {
		return nil, err
	}
	return &budget.OverrideExpiry{TeamID: teamID, BaseCeiling: baseMoney, Lowered: lowered}, nil
}

const exhaustedCondition = `[{"type":"BudgetExhausted","status":"True"}]`

// Exhaustion implements budget.Store. A block-mode budget is exhausted when its current counter has
// spent its limit; a notify-mode budget never is.
func (j *BudgetJobs) Exhaustion(ctx context.Context, now time.Time) ([]budget.TeamBudget, error) {
	var out []budget.TeamBudget
	err := j.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT tm.id, tm.namespace, s.exhausted, s.marked FROM afs.teams tm
JOIN afs.budgets b ON b.team_id = tm.id
LEFT JOIN afs.spend_counters c ON c.budget_id = b.id AND c.period_start = `+currentPeriodSQL+`
CROSS JOIN LATERAL (SELECT coalesce(b.on_exhaust = 'block' AND c.spent_usd >= c.limit_usd, false) AS exhausted,
  EXISTS (SELECT 1 FROM afs.resource_status rs WHERE rs.team_id = tm.id AND rs.conditions @> $2::jsonb) AS marked) s
WHERE tm.state NOT IN ('deleting', 'deleted') AND (s.exhausted OR s.marked) ORDER BY tm.id`, now, exhaustedCondition)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tb budget.TeamBudget
			if err := rows.Scan(&tb.TeamID, &tb.Namespace, &tb.Exhausted, &tb.Marked); err != nil {
				return err
			}
			out = append(out, tb)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, MapError(err, OpRead, "budgets")
	}
	return out, nil
}

// dropCondition is the SQL expression for conditions without BudgetExhausted.
const dropCondition = `(SELECT coalesce(jsonb_agg(c), '[]'::jsonb) FROM jsonb_array_elements(conditions) c WHERE c ->> 'type' <> 'BudgetExhausted')`

// MarkExhausted implements budget.Store.
func (j *BudgetJobs) MarkExhausted(ctx context.Context, teamID string) error {
	cond, err := json.Marshal(domain.Condition{Type: domain.CondBudgetExhausted, Status: domain.ConditionTrue, Reason: "BudgetExhausted",
		Message:            "the team's budget for the period is spent; model calls are refused until the period resets or the budget is raised",
		LastTransitionTime: time.Now().UTC()})
	if err != nil {
		return err
	}
	return j.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		tag, err := t.Exec(ctx, `UPDATE afs.resource_status SET conditions = `+dropCondition+` || jsonb_build_array($2::jsonb)
WHERE team_id = $1 AND resource_kind IN ('team', 'agent', 'pool') AND NOT conditions @> $3::jsonb`, teamID, string(cond), exhaustedCondition)
		if err != nil {
			return MapError(err, OpWrite, "resource status")
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return j.audit.Write(ctx, tx, port.AuditRecord{Action: AuditBudgetExhausted, Outcome: port.OutcomeAllowed,
			ResourceKind: "teams", ResourceID: teamID, TeamID: teamID, After: map[string]any{"resources": tag.RowsAffected()}})
	})
}

// ClearExhausted implements budget.Store.
func (j *BudgetJobs) ClearExhausted(ctx context.Context, teamID string) error {
	return j.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		tag, err := t.Exec(ctx, `UPDATE afs.resource_status SET conditions = `+dropCondition+`
WHERE team_id = $1 AND conditions @> '[{"type":"BudgetExhausted"}]'::jsonb`, teamID)
		if err != nil {
			return MapError(err, OpWrite, "resource status")
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return j.audit.Write(ctx, tx, port.AuditRecord{Action: AuditBudgetRestored, Outcome: port.OutcomeAllowed,
			ResourceKind: "teams", ResourceID: teamID, TeamID: teamID, After: map[string]any{"resources": tag.RowsAffected()}})
	})
}

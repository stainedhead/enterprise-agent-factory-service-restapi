package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/costs"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// CostsRepo implements costs.Repository on cost_daily and cost_reconciliations under the caller's
// session: row-level security shows a team caller its own rows and a service admin every row, deleted
// teams included.
type CostsRepo struct{}

// NewCostsRepo returns a CostsRepo.
func NewCostsRepo() *CostsRepo { return &CostsRepo{} }

var _ costs.Repository = (*CostsRepo)(nil)

// TeamExists implements costs.Repository.
func (*CostsRepo) TeamExists(ctx context.Context, tx port.Tx, teamID string) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	var one int
	err = t.QueryRow(ctx, `SELECT 1 FROM afs.teams WHERE id = $1`, teamID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound("team")
	}
	return MapError(err, OpRead, "team")
}

// ListCosts implements costs.Repository. A day split by tag sets is summed back into one row per
// source, subject and model.
func (*CostsRepo) ListCosts(ctx context.Context, tx port.Tx, teamID string, fromDay, toDay time.Time, offset, limit int) ([]costs.Row, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT day, source, agent_id, pool_id, model_id, sum(cost_usd)::text, sum(calls)::bigint
FROM afs.cost_daily
WHERE team_id = $1 AND day >= $2::date AND day <= $3::date
GROUP BY day, source, agent_id, pool_id, model_id
ORDER BY day, source, agent_id NULLS FIRST, pool_id NULLS FIRST, model_id NULLS FIRST
OFFSET $4 LIMIT $5`, teamID, fromDay.Format(time.DateOnly), toDay.Format(time.DateOnly), offset, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "cost_daily")
	}
	defer rows.Close()
	var out []costs.Row
	for rows.Next() {
		var r costs.Row
		var cost string
		if err := rows.Scan(&r.Day, &r.Source, &r.AgentID, &r.PoolID, &r.ModelID, &cost, &r.Calls); err != nil {
			return nil, MapError(err, OpRead, "cost_daily")
		}
		r.Day = r.Day.UTC()
		if r.CostUSD, err = domain.ParseMoney("costUsd", cost); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, MapError(rows.Err(), OpRead, "cost_daily")
}

// ListReconciliations implements costs.Repository.
func (*CostsRepo) ListReconciliations(ctx context.Context, tx port.Tx, status string, after *costs.ReconCursor, limit int) ([]costs.Reconciliation, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	var afterDay, afterTeam any
	if after != nil {
		afterDay, afterTeam = after.Day.Format(time.DateOnly), after.TeamID
	}
	rows, err := t.Query(ctx, `SELECT team_id, day, gateway_usd::text, billed_usd::text, variance_pct::text, status, reconciled_at
FROM afs.cost_reconciliations
WHERE ($1 = '' OR status = $1) AND ($2::date IS NULL OR (day, team_id) < ($2::date, $3::text))
ORDER BY day DESC, team_id DESC LIMIT $4`, status, afterDay, afterTeam, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "cost_reconciliations")
	}
	defer rows.Close()
	var out []costs.Reconciliation
	for rows.Next() {
		var r costs.Reconciliation
		var g, b string
		if err := rows.Scan(&r.TeamID, &r.Day, &g, &b, &r.VariancePct, &r.Status, &r.ReconciledAt); err != nil {
			return nil, MapError(err, OpRead, "cost_reconciliations")
		}
		r.Day, r.ReconciledAt = r.Day.UTC(), r.ReconciledAt.UTC()
		if r.GatewayUSD, err = domain.ParseMoney("gatewayUsd", g); err != nil {
			return nil, err
		}
		if r.BilledUSD, err = domain.ParseMoney("billedUsd", b); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, MapError(rows.Err(), OpRead, "cost_reconciliations")
}

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/reconcile"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// BillingSession is the session of afs-billing's jobs (actor system:afs-billing).
func BillingSession() port.Session { return port.SystemSession("afs-billing") }

// BillingStore implements cur.Store and reconcile.Repository for afs-billing's login (afs_billing,
// database contract §3): it reads teams, models, inference profiles, tags and usage, and writes
// cost_daily and cost_reconciliations. It never inserts audit or outbox rows with RETURNING.
type BillingStore struct {
	runner *TxRunner
}

// NewBillingStore returns a BillingStore on the billing pool's runner.
func NewBillingStore(runner *TxRunner) *BillingStore { return &BillingStore{runner: runner} }

var (
	_ cur.Store            = (*BillingStore)(nil)
	_ reconcile.Repository = (*BillingStore)(nil)
)

const curSources = `('cur_bedrock', 'cur_eks', 'cur_other')`

func dayArg(day time.Time) string { return day.UTC().Format(time.DateOnly) }

func startArg(day time.Time) string {
	return day.UTC().Truncate(24 * time.Hour).Format(time.RFC3339)
}

// Directory implements cur.Store. The inference profiles are read in a transaction of their own: the
// afs_billing role has no SELECT on team_models yet (-db gap X-DB-9), and a refused statement would
// abort the rest. Without them Bedrock lines are attributed by their afs-team-id tag and carry no model.
func (s *BillingStore) Directory(ctx context.Context) (cur.Directory, error) {
	d := cur.Directory{Namespaces: map[string]string{}, Teams: map[string]bool{}, Profiles: map[string]cur.ProfileRef{}, TagKeys: map[string]bool{}}
	each := func(ctx context.Context, tx port.Tx, q string, f func(a, b, c string)) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a, b, c string
			if err := rows.Scan(&a, &b, &c); err != nil {
				return err
			}
			f(a, b, c)
		}
		return rows.Err()
	}
	err := s.runner.ReadTx(ctx, BillingSession(), func(ctx context.Context, tx port.Tx) error {
		d.Namespaces, d.Teams, d.TagKeys = map[string]string{}, map[string]bool{}, map[string]bool{}
		if err := each(ctx, tx, `SELECT id, namespace, '' FROM afs.teams`, func(id, ns, _ string) {
			d.Teams[id] = true
			d.Namespaces[ns] = id
		}); err != nil {
			return err
		}
		return each(ctx, tx, `SELECT key, '', '' FROM afs.service_tags UNION SELECT key, '', '' FROM afs.team_tags`,
			func(k, _, _ string) { d.TagKeys[k] = true })
	})
	if err != nil {
		return cur.Directory{}, MapError(err, OpRead, "billing directory")
	}
	err = s.runner.ReadTx(ctx, BillingSession(), func(ctx context.Context, tx port.Tx) error {
		d.Profiles = map[string]cur.ProfileRef{}
		return each(ctx, tx, `SELECT inference_profile_arn, team_id, model_id FROM afs.team_models WHERE inference_profile_arn IS NOT NULL`,
			func(arn, team, model string) { d.Profiles[arn] = cur.ProfileRef{TeamID: team, ModelID: model} })
	})
	if err != nil {
		return cur.Directory{}, MapError(err, OpRead, "inference profiles")
	}
	return d, nil
}

// ReplaceDay implements cur.Store: one transaction deletes the day's CUR rows and inserts the new ones,
// so a reader sees the old day or the new day, never a mixture.
func (s *BillingStore) ReplaceDay(ctx context.Context, day time.Time, rows []cur.Row) error {
	err := s.runner.InTx(ctx, BillingSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		if _, err := t.Exec(ctx, `DELETE FROM afs.cost_daily WHERE day = $1::date AND source IN `+curSources, dayArg(day)); err != nil {
			return err
		}
		for _, r := range rows {
			var tags any // NULL: AWS billed the spend with none of the managed tags
			if len(r.Tags) > 0 {
				b, err := json.Marshal(r.Tags)
				if err != nil {
					return err
				}
				tags = string(b)
			}
			if _, err := t.Exec(ctx, `INSERT INTO afs.cost_daily (team_id, day, source, agent_id, pool_id, model_id, cost_usd, tag_set_id)
VALUES ($1, $2::date, $3, nullif($4, ''), nullif($5, ''), nullif($6, ''), $7::numeric,
        CASE WHEN $8::jsonb IS NULL THEN NULL ELSE afs.tag_set_for($1, $8::jsonb) END)`,
				r.TeamID, dayArg(day), r.Source, r.AgentID, r.PoolID, r.ModelID, r.Cost.String(), tags); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return MapError(err, OpWrite, "cost_daily")
	}
	return nil
}

// BillingPresent implements reconcile.Repository.
func (s *BillingStore) BillingPresent(ctx context.Context, tx port.Tx, day time.Time) (bool, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var ok bool
	err = t.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.cost_daily WHERE day = $1::date AND source IN `+curSources+`)`, dayArg(day)).Scan(&ok)
	return ok, MapError(err, OpRead, "cost_daily")
}

// RollupGateway implements reconcile.Repository. A call is an event with outcome ok or error; rejected
// attempts are not calls (the same rule as the reports and the activity collector).
func (s *BillingStore) RollupGateway(ctx context.Context, tx port.Tx, day time.Time) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	if _, err := t.Exec(ctx, `DELETE FROM afs.cost_daily WHERE day = $1::date AND source = 'gateway'`, dayArg(day)); err != nil {
		return MapError(err, OpWrite, "cost_daily")
	}
	_, err = t.Exec(ctx, `INSERT INTO afs.cost_daily (team_id, day, source, agent_id, pool_id, model_id, input_tokens, output_tokens, cost_usd, calls, tag_set_id)
SELECT e.team_id, $1::date, 'gateway', e.agent_id, e.pool_id, e.model_id, sum(e.input_tokens), sum(e.output_tokens), sum(e.cost_usd), count(*), e.tag_set_id
FROM afs.usage_events e
JOIN afs.teams t ON t.id = e.team_id
WHERE e.occurred_at >= $2::timestamptz AND e.occurred_at < $2::timestamptz + interval '1 day' AND e.outcome IN ('ok', 'error')
GROUP BY e.team_id, e.agent_id, e.pool_id, e.model_id, e.tag_set_id`, dayArg(day), startArg(day))
	return MapError(err, OpWrite, "cost_daily")
}

// Totals implements reconcile.Repository.
func (s *BillingStore) Totals(ctx context.Context, tx port.Tx, day time.Time) ([]reconcile.TeamDay, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT team_id,
  coalesce(sum(cost_usd) FILTER (WHERE source = 'gateway'), 0)::text,
  coalesce(sum(cost_usd) FILTER (WHERE source = 'cur_bedrock'), 0)::text
FROM afs.cost_daily WHERE day = $1::date AND source IN ('gateway', 'cur_bedrock') GROUP BY team_id ORDER BY team_id`, dayArg(day))
	if err != nil {
		return nil, MapError(err, OpRead, "cost_daily")
	}
	defer rows.Close()
	var out []reconcile.TeamDay
	for rows.Next() {
		var team, g, b string
		if err := rows.Scan(&team, &g, &b); err != nil {
			return nil, MapError(err, OpRead, "cost_daily")
		}
		td := reconcile.TeamDay{TeamID: team}
		if td.GatewayUSD, err = domain.ParseMoney("gateway", g); err != nil {
			return nil, err
		}
		if td.BilledUSD, err = domain.ParseMoney("billed", b); err != nil {
			return nil, err
		}
		out = append(out, td)
	}
	return out, MapError(rows.Err(), OpRead, "cost_daily")
}

// Previous implements reconcile.Repository.
func (s *BillingStore) Previous(ctx context.Context, tx port.Tx, teamID string, day time.Time) (string, bool, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return "", false, err
	}
	var st string
	err = t.QueryRow(ctx, `SELECT status FROM afs.cost_reconciliations WHERE team_id = $1 AND day = $2::date`, teamID, dayArg(day)).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return st, err == nil, MapError(err, OpRead, "cost_reconciliations")
}

// Upsert implements reconcile.Repository. The status written is a placeholder: the trigger
// cost_reconciliations_status derives it from the amounts and billing.variance_alert_pct, and the row
// is read back as stored.
func (s *BillingStore) Upsert(ctx context.Context, tx port.Tx, day time.Time, td reconcile.TeamDay) (reconcile.Reconciliation, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return reconcile.Reconciliation{}, err
	}
	var status string
	var variance *string
	err = t.QueryRow(ctx, `INSERT INTO afs.cost_reconciliations (team_id, day, gateway_usd, billed_usd, status)
VALUES ($1, $2::date, $3::numeric, $4::numeric, 'ok')
ON CONFLICT (team_id, day) DO UPDATE SET gateway_usd = EXCLUDED.gateway_usd, billed_usd = EXCLUDED.billed_usd, reconciled_at = now()
RETURNING status, variance_pct::text`, td.TeamID, dayArg(day), td.GatewayUSD.String(), td.BilledUSD.String()).Scan(&status, &variance)
	if err != nil {
		return reconcile.Reconciliation{}, MapError(err, OpWrite, "cost_reconciliations")
	}
	rec := reconcile.Reconciliation{TeamID: td.TeamID, Day: day.UTC().Truncate(24 * time.Hour), GatewayUSD: td.GatewayUSD, BilledUSD: td.BilledUSD, Status: status}
	if variance != nil {
		rec.VariancePct = *variance
	}
	return rec, nil
}

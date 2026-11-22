package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/activity"
)

// ActivityRoster implements activity.RosterSource: the agents and pools the desired state says exist
// (M3-I1). An agent is paused when it or its team is; the team's BudgetExhausted condition blocks it.
type ActivityRoster struct{ runner *TxRunner }

// NewActivityRoster returns an ActivityRoster on the afs_reconciler pool's TxRunner.
func NewActivityRoster(runner *TxRunner) *ActivityRoster { return &ActivityRoster{runner: runner} }

var _ activity.RosterSource = (*ActivityRoster)(nil)

const (
	blockedCond = `coalesce(rs.conditions @> '[{"type": "BudgetExhausted", "status": "True"}]'::jsonb, false)`
	rosterJoin  = ` JOIN afs.teams t ON t.id = x.team_id
LEFT JOIN afs.resource_status rs ON rs.resource_kind = 'team' AND rs.resource_id = t.id
WHERE x.state IN ('enabled', 'paused') AND t.state IN ('active', 'suspended')`
	rosterAgentsSQL = `SELECT x.team_id, x.id, t.namespace, x.state = 'paused' OR t.state = 'suspended', ` + blockedCond + ` FROM afs.agents x` + rosterJoin
	rosterPoolsSQL  = `SELECT x.team_id, x.id, t.namespace, x.state = 'paused' OR t.state = 'suspended', ` + blockedCond + ` FROM afs.agent_pools x` + rosterJoin
)

// Roster implements activity.RosterSource.
func (r *ActivityRoster) Roster(ctx context.Context) (activity.Roster, error) {
	var out activity.Roster
	err := r.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, rosterAgentsSQL)
		if err != nil {
			return MapError(err, OpRead, "agents")
		}
		for rows.Next() {
			var a activity.Agent
			if err := rows.Scan(&a.TeamID, &a.AgentID, &a.Namespace, &a.Paused, &a.BudgetBlocked); err != nil {
				rows.Close()
				return MapError(err, OpRead, "agents")
			}
			out.Agents = append(out.Agents, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return MapError(err, OpRead, "agents")
		}
		prows, err := t.Query(ctx, rosterPoolsSQL)
		if err != nil {
			return MapError(err, OpRead, "pools")
		}
		defer prows.Close()
		for prows.Next() {
			var p activity.Pool
			if err := prows.Scan(&p.TeamID, &p.PoolID, &p.Namespace, &p.Paused, &p.BudgetBlocked); err != nil {
				return MapError(err, OpRead, "pools")
			}
			out.Pools = append(out.Pools, p)
		}
		return MapError(prows.Err(), OpRead, "pools")
	})
	return out, err
}

// ActivityUsage implements activity.UsageSource over afs.usage_events: calls the gateway sent upstream
// (outcome ok or error) in the window, with their tokens and cost.
type ActivityUsage struct{ runner *TxRunner }

// NewActivityUsage returns an ActivityUsage.
func NewActivityUsage(runner *TxRunner) *ActivityUsage { return &ActivityUsage{runner: runner} }

var _ activity.UsageSource = (*ActivityUsage)(nil)

const windowUsageSQL = `SELECT agent_id, pool_id, worker_pod, count(*), coalesce(sum(input_tokens), 0)::bigint,
  coalesce(sum(output_tokens), 0)::bigint, coalesce(sum(cost_usd), 0)::text
FROM afs.usage_events
WHERE occurred_at >= $1 AND occurred_at < $2 AND outcome IN ('ok', 'error')
GROUP BY agent_id, pool_id, worker_pod`

// Window implements activity.UsageSource.
func (u *ActivityUsage) Window(ctx context.Context, from, to time.Time) (map[activity.UsageKey]activity.Usage, error) {
	out := map[activity.UsageKey]activity.Usage{}
	err := u.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, windowUsageSQL, from, to)
		if err != nil {
			return MapError(err, OpRead, "usage")
		}
		defer rows.Close()
		for rows.Next() {
			var agent, pool, worker *string
			var calls int
			var in, outTok int64
			var cost string
			if err := rows.Scan(&agent, &pool, &worker, &calls, &in, &outTok, &cost); err != nil {
				return MapError(err, OpRead, "usage")
			}
			m, err := domain.ParseMoney("cost", cost)
			if err != nil {
				return err
			}
			k := activity.UsageKey{}
			if agent != nil {
				k.AgentID = *agent
			}
			if pool != nil && worker != nil {
				k.PoolID, k.WorkerPod = *pool, *worker
			}
			out[k] = activity.Usage{Calls: calls, InputTokens: in, OutputTokens: outTok, Cost: m}
		}
		return MapError(rows.Err(), OpRead, "usage")
	})
	return out, err
}

// ActivityThreshold implements activity.ThresholdSource on service_settings.
type ActivityThreshold struct{ settings *SettingsReader }

// NewActivityThreshold returns an ActivityThreshold.
func NewActivityThreshold(runner *TxRunner) *ActivityThreshold {
	return &ActivityThreshold{settings: NewSettingsReader(runner)}
}

var _ activity.ThresholdSource = (*ActivityThreshold)(nil)

// IdleCPUMillicores implements activity.ThresholdSource: reports.idle_cpu_millicores.
func (a *ActivityThreshold) IdleCPUMillicores(ctx context.Context) (int, error) {
	raw, err := a.settings.Setting(ctx, domain.SettingReportsIdleCpuMillicores)
	if err != nil {
		return 0, err
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("postgres: %s: %w", domain.SettingReportsIdleCpuMillicores, err)
	}
	return n, nil
}

// ActivityStore implements activity.Store on afs.agent_activity. A sample that exists is left as it is,
// so a window finalized twice (a leader change) is stored once; a new sample is stamped with the team's
// tag set at that moment.
type ActivityStore struct{ runner *TxRunner }

// NewActivityStore returns an ActivityStore.
func NewActivityStore(runner *TxRunner) *ActivityStore { return &ActivityStore{runner: runner} }

var _ activity.Store = (*ActivityStore)(nil)

const addActivitySQL = `INSERT INTO afs.agent_activity
  (bucket_start, team_id, agent_id, pool_id, worker_pod, state, active_seconds, model_calls, input_tokens, output_tokens,
   cost_usd, cpu_millicores_avg, memory_mib_avg, activity_note, tag_set_id)
VALUES ($1, $2, nullif($3, ''), nullif($4, ''), nullif($5, ''), $6, $7, $8, $9, $10, $11::numeric, $12, $13, nullif($14, ''), $15)
ON CONFLICT (bucket_start, agent_id, pool_id, worker_pod) DO NOTHING`

// Add implements activity.Store. All samples are written in one transaction.
func (s *ActivityStore) Add(ctx context.Context, samples []activity.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	return s.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		tagSets := map[string]any{}
		for _, x := range samples {
			if _, ok := tagSets[x.TeamID]; ok {
				continue
			}
			var id *string
			if err := t.QueryRow(ctx, `SELECT afs.current_team_tag_set($1)`, x.TeamID).Scan(&id); err != nil {
				return MapError(err, OpRead, "tag set")
			}
			if id != nil {
				tagSets[x.TeamID] = *id
			} else {
				tagSets[x.TeamID] = nil
			}
		}
		for _, x := range samples {
			if _, err := t.Exec(ctx, addActivitySQL, x.BucketStart, x.TeamID, x.AgentID, x.PoolID, x.WorkerPod, string(x.State), x.ActiveSeconds,
				x.ModelCalls, x.InputTokens, x.OutputTokens, x.Cost.String(), x.CPUAvg, x.MemAvg, x.Note, tagSets[x.TeamID]); err != nil {
				return MapError(err, OpWrite, "agent activity")
			}
		}
		return nil
	})
}

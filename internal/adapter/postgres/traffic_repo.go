package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
)

// TrafficStore implements traffic.Store on afs.external_traffic (M2-H2). Each row is created or its
// counters increased, and a new row is stamped with the team's current tag set.
type TrafficStore struct{ runner *TxRunner }

// NewTrafficStore returns a TrafficStore on the afs_reconciler pool's TxRunner.
func NewTrafficStore(runner *TxRunner) *TrafficStore { return &TrafficStore{runner: runner} }

var _ traffic.Store = (*TrafficStore)(nil)

const addTrafficSQL = `INSERT INTO afs.external_traffic
  (bucket_start, team_id, agent_id, pool_id, worker_pod, system_id, host, port, verdict, denied_reason,
   connections, bytes_out, bytes_in, tag_set_id)
VALUES ($1, $2, nullif($3, ''), nullif($4, ''), nullif($5, ''), nullif($6, ''), $7, $8, $9, nullif($10, ''), $11, $12, $13, $14)
ON CONFLICT (bucket_start, agent_id, pool_id, worker_pod, host, port, verdict) DO UPDATE
SET connections = afs.external_traffic.connections + EXCLUDED.connections,
    bytes_out   = afs.external_traffic.bytes_out + EXCLUDED.bytes_out,
    bytes_in    = afs.external_traffic.bytes_in + EXCLUDED.bytes_in,
    system_id   = coalesce(afs.external_traffic.system_id, EXCLUDED.system_id),
    recorded_at = now()`

// Add implements traffic.Store. All rows are written in one transaction, so a failure stores none and
// the collector keeps its counts for the next flush.
func (s *TrafficStore) Add(ctx context.Context, rows []traffic.Row) error {
	if len(rows) == 0 {
		return nil
	}
	return s.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		tagSets := map[string]string{}
		for _, r := range rows {
			if _, ok := tagSets[r.TeamID]; ok {
				continue
			}
			var id *string
			if err := t.QueryRow(ctx, `SELECT afs.current_team_tag_set($1)`, r.TeamID).Scan(&id); err != nil {
				return MapError(err, OpRead, "tag set")
			}
			if id != nil {
				tagSets[r.TeamID] = *id
			} else {
				tagSets[r.TeamID] = ""
			}
		}
		for _, r := range rows {
			var tag any
			if id := tagSets[r.TeamID]; id != "" {
				tag = id
			}
			if _, err := t.Exec(ctx, addTrafficSQL, r.BucketStart, r.TeamID, r.AgentID, r.PoolID, r.WorkerPod, r.SystemID, r.Host, r.Port,
				r.Verdict, r.DeniedReason, r.Connections, r.BytesOut, r.BytesIn, tag); err != nil {
				return MapError(err, OpWrite, "external traffic")
			}
		}
		return nil
	})
}

// TrafficDirectory implements traffic.Directory: it checks that the agent or pool a flow's pod claims
// exists in the pod's namespace, and returns its team and the systems it may see.
type TrafficDirectory struct{ runner *TxRunner }

// NewTrafficDirectory returns a TrafficDirectory.
func NewTrafficDirectory(runner *TxRunner) *TrafficDirectory {
	return &TrafficDirectory{runner: runner}
}

var _ traffic.Directory = (*TrafficDirectory)(nil)

const (
	resolveAgentSQL = `SELECT t.id FROM afs.agents a JOIN afs.teams t ON t.id = a.team_id
WHERE a.id = $1 AND t.namespace = $2 AND a.state <> 'deleted' AND t.state <> 'deleted'`
	resolvePoolSQL = `SELECT t.id FROM afs.agent_pools p JOIN afs.teams t ON t.id = p.team_id
WHERE p.id = $1 AND t.namespace = $2 AND p.state <> 'deleted' AND t.state <> 'deleted'`
	agentSystemsSQL = `SELECT system_id, slug, category, endpoints, coalesce(disabled_at, '') FROM afs.agent_external_access WHERE agent_id = $1 ORDER BY slug`
	poolSystemsSQL  = `SELECT system_id, slug, category, endpoints, coalesce(disabled_at, '') FROM afs.pool_external_access WHERE pool_id = $1 ORDER BY slug`
)

// ResolveAgent implements traffic.Directory.
func (d *TrafficDirectory) ResolveAgent(ctx context.Context, namespace, agentID string) (traffic.Resolved, bool, error) {
	return d.resolve(ctx, resolveAgentSQL, agentSystemsSQL, agentID, namespace)
}

// ResolvePool implements traffic.Directory.
func (d *TrafficDirectory) ResolvePool(ctx context.Context, namespace, poolID string) (traffic.Resolved, bool, error) {
	return d.resolve(ctx, resolvePoolSQL, poolSystemsSQL, poolID, namespace)
}

func (d *TrafficDirectory) resolve(ctx context.Context, ownerSQL, systemsSQL, id, namespace string) (traffic.Resolved, bool, error) {
	var res traffic.Resolved
	found := false
	err := d.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		err = t.QueryRow(ctx, ownerSQL, id, namespace).Scan(&res.TeamID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return MapError(err, OpRead, "flow source")
		}
		found = true
		rows, err := t.Query(ctx, systemsSQL, id)
		if err != nil {
			return MapError(err, OpRead, "external systems")
		}
		defer rows.Close()
		for rows.Next() {
			var s egress.System
			if err := rows.Scan(&s.ID, &s.Slug, &s.Category, &s.Endpoints, &s.DisabledAt); err != nil {
				return MapError(err, OpRead, "external systems")
			}
			res.Systems = append(res.Systems, s)
		}
		return MapError(rows.Err(), OpRead, "external systems")
	})
	return res, found, err
}

// SpikeThreshold implements traffic.ThresholdSource on service_settings.
type SpikeThreshold struct{ settings *SettingsReader }

// NewSpikeThreshold returns a SpikeThreshold.
func NewSpikeThreshold(runner *TxRunner) *SpikeThreshold {
	return &SpikeThreshold{settings: NewSettingsReader(runner)}
}

var _ traffic.ThresholdSource = (*SpikeThreshold)(nil)

// DeniedSpikeThreshold implements traffic.ThresholdSource: egress.denied_spike_threshold.
func (s *SpikeThreshold) DeniedSpikeThreshold(ctx context.Context) (int, error) {
	raw, err := s.settings.Setting(ctx, domain.SettingEgressDeniedSpikeThreshold)
	if err != nil {
		return 0, err
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("postgres: %s: %w", domain.SettingEgressDeniedSpikeThreshold, err)
	}
	return n, nil
}

// SpikeEvents implements traffic.SpikeEvents: the egress.denied_spike event goes to the outbox in the
// reconciler's transaction, like every event.
type SpikeEvents struct {
	runner *TxRunner
	outbox port.OutboxWriter
	ids    port.IDGen
}

// NewSpikeEvents returns SpikeEvents.
func NewSpikeEvents(runner *TxRunner, outbox port.OutboxWriter, ids port.IDGen) *SpikeEvents {
	return &SpikeEvents{runner: runner, outbox: outbox, ids: ids}
}

var _ traffic.SpikeEvents = (*SpikeEvents)(nil)

const spikeEmittedSQL = `SELECT EXISTS (SELECT 1 FROM afs.outbox_events
WHERE event_type = $1 AND aggregate_id = $2 AND payload -> 'data' ->> 'windowStart' = $3)`

// Emitted implements traffic.SpikeEvents.
func (e *SpikeEvents) Emitted(ctx context.Context, subjectID string, window time.Time) (bool, error) {
	var exists bool
	err := e.runner.ReadTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return t.QueryRow(ctx, spikeEmittedSQL, string(domain.EventEgressDeniedSpike), subjectID, window.UTC().Format(time.RFC3339)).Scan(&exists)
	})
	return exists, MapError(err, OpRead, "outbox")
}

// Publish implements traffic.SpikeEvents.
func (e *SpikeEvents) Publish(ctx context.Context, s traffic.Spike) error {
	return e.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		id, err := e.ids.NewID(eventIDPrefix)
		if err != nil {
			return err
		}
		kind, subject := "agent", s.AgentID
		if s.PoolID != "" {
			kind, subject = "pool", s.PoolID
		}
		data := map[string]any{
			"windowStart": s.WindowStart.UTC().Format(time.RFC3339), "windowSeconds": int(traffic.BucketSize / time.Second),
			"denied": s.Denied, "threshold": s.Threshold, "hosts": s.Hosts,
		}
		ev, err := domain.NewEvent(domain.EventEgressDeniedSpike, id.String(), time.Now().UTC(), tx.Session().Actor, s.TeamID, kind, subject, data)
		if err != nil {
			return err
		}
		ev.Detail.AgentID, ev.Detail.PoolID = s.AgentID, s.PoolID
		return e.outbox.Publish(ctx, tx, ev)
	})
}

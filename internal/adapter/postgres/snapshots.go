package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/lifecycle"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/snapshot"
)

// SnapshotStore implements snapshot.Store on afs.agent_volume_snapshots for the reconciler's login. The
// reconciler may insert rows and update state, ebs_snapshot_id, size_gib and completed_at only; rows are
// history and are never removed.
type SnapshotStore struct {
	runner port.TxRunner
	now    func() time.Time
}

// NewSnapshotStore returns a SnapshotStore.
func NewSnapshotStore(runner port.TxRunner) *SnapshotStore {
	return &SnapshotStore{runner: runner, now: time.Now}
}

var _ snapshot.Store = (*SnapshotStore)(nil)

const snapshotCols = `id, team_id, agent_id, reason, state, coalesce(ebs_snapshot_id, ''), coalesce(size_gib, 0), requested_at`

func (s *SnapshotStore) rows(ctx context.Context, what, query string, args ...any) ([]snapshot.Row, error) {
	var out []snapshot.Row
	err := s.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rs, err := t.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r snapshot.Row
			if err := rs.Scan(&r.ID, &r.TeamID, &r.AgentID, &r.Reason, &r.State, &r.EBSID, &r.SizeGiB, &r.RequestedAt); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rs.Err()
	})
	if err != nil {
		return nil, MapError(err, OpRead, what)
	}
	return out, nil
}

// Scheduled implements snapshot.Store: live agents with a schedule, and the request time of their latest
// scheduled snapshot.
func (s *SnapshotStore) Scheduled(ctx context.Context) ([]snapshot.Scheduled, error) {
	var out []snapshot.Scheduled
	err := s.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rs, err := t.Query(ctx, `
SELECT a.id, a.snapshot_schedule,
       (SELECT max(v.requested_at) FROM afs.agent_volume_snapshots v WHERE v.agent_id = a.id AND v.reason = 'scheduled')
  FROM afs.agents a
 WHERE a.snapshot_schedule <> 'off' AND a.state IN ('enabled', 'paused') AND a.deleted_at IS NULL
 ORDER BY a.id`)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var sc snapshot.Scheduled
			if err := rs.Scan(&sc.AgentID, &sc.Schedule, &sc.Last); err != nil {
				return err
			}
			out = append(out, sc)
		}
		return rs.Err()
	})
	if err != nil {
		return nil, MapError(err, OpRead, "scheduled snapshots")
	}
	return out, nil
}

// Open implements snapshot.Store.
func (s *SnapshotStore) Open(ctx context.Context) ([]snapshot.Row, error) {
	return s.rows(ctx, "open snapshots", `SELECT `+snapshotCols+` FROM afs.agent_volume_snapshots
 WHERE state IN ('requested', 'pending') ORDER BY requested_at, id`)
}

// Completed implements snapshot.Store.
func (s *SnapshotStore) Completed(ctx context.Context, agentID string) ([]snapshot.Row, error) {
	return s.rows(ctx, "completed snapshots", `SELECT `+snapshotCols+` FROM afs.agent_volume_snapshots
 WHERE agent_id = $1 AND state = 'completed' ORDER BY requested_at DESC, id DESC`, agentID)
}

// ForAgent implements snapshot.Store.
func (s *SnapshotStore) ForAgent(ctx context.Context, agentID, reason string) ([]snapshot.Row, error) {
	return s.rows(ctx, "agent snapshots", `SELECT `+snapshotCols+` FROM afs.agent_volume_snapshots
 WHERE agent_id = $1 AND reason = $2 AND state <> 'deleted' ORDER BY requested_at DESC, id DESC`, agentID, reason)
}

// Create implements snapshot.Store.
func (s *SnapshotStore) Create(ctx context.Context, teamID, agentID, reason string) (string, error) {
	id, err := domain.NewID(domain.IDPrefix("vsnap"), s.now(), nil)
	if err != nil {
		return "", err
	}
	err = s.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		_, err = t.Exec(ctx, `INSERT INTO afs.agent_volume_snapshots (id, team_id, agent_id, reason) VALUES ($1, $2, $3, $4)`,
			string(id), teamID, agentID, reason)
		return err
	})
	if err != nil {
		return "", MapError(err, OpWrite, "snapshot")
	}
	return string(id), nil
}

func (s *SnapshotStore) update(ctx context.Context, id, set string, args ...any) error {
	err := s.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		tag, err := t.Exec(ctx, `UPDATE afs.agent_volume_snapshots SET `+set+` WHERE id = $1`, append([]any{id}, args...)...)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
	return MapError(err, OpWrite, "snapshot")
}

// MarkPending implements snapshot.Store.
func (s *SnapshotStore) MarkPending(ctx context.Context, id, ebsID string) error {
	return s.update(ctx, id, `state = 'pending', ebs_snapshot_id = $2`, ebsID)
}

// MarkCompleted implements snapshot.Store.
func (s *SnapshotStore) MarkCompleted(ctx context.Context, id, ebsID string, sizeGiB int, at time.Time) error {
	return s.update(ctx, id, `state = 'completed', ebs_snapshot_id = $2, size_gib = nullif($3, 0), completed_at = $4`, ebsID, sizeGiB, at)
}

// MarkFailed implements snapshot.Store.
func (s *SnapshotStore) MarkFailed(ctx context.Context, id string) error {
	return s.update(ctx, id, `state = 'failed'`)
}

// MarkDeleted implements snapshot.Store.
func (s *SnapshotStore) MarkDeleted(ctx context.Context, id string) error {
	return s.update(ctx, id, `state = 'deleted'`)
}

// AgentLifecycle is the database side of the agent delete finalizer: the last step, and the record of a
// retained volume.
type AgentLifecycle struct {
	runner port.TxRunner
	audit  port.AuditWriter
}

// NewAgentLifecycle returns an AgentLifecycle.
func NewAgentLifecycle(runner port.TxRunner, audit port.AuditWriter) *AgentLifecycle {
	return &AgentLifecycle{runner: runner, audit: audit}
}

var _ lifecycle.Completer = (*AgentLifecycle)(nil)

// Complete implements lifecycle.Completer for an agent: state becomes `deleted` with deleted_at set and one
// audit record is written in the same transaction. The row stays (usage, snapshots and audit refer to it).
// An already deleted agent is a no-op.
func (l *AgentLifecycle) Complete(ctx context.Context, tgt lifecycle.Target) error {
	if tgt.Kind != "agent" {
		return fmt.Errorf("postgres: agent lifecycle completer handles agents, not %q", tgt.Kind)
	}
	err := l.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var state, teamID string
		if err := t.QueryRow(ctx, `SELECT state, team_id FROM afs.agents WHERE id = $1 FOR UPDATE`, tgt.ID).Scan(&state, &teamID); err != nil {
			return err
		}
		switch state {
		case "deleted":
			return nil
		case "deleting":
		default:
			return domain.Conflict("agent_not_deleting", fmt.Sprintf("agent %s is %s, not deleting", tgt.ID, state), nil)
		}
		if _, err := t.Exec(ctx, `UPDATE afs.agents SET state = 'deleted', deleted_at = now() WHERE id = $1`, tgt.ID); err != nil {
			return err
		}
		return l.audit.Write(ctx, tx, port.AuditRecord{
			Action: "agent.delete_finalized", Outcome: port.OutcomeAllowed,
			ResourceKind: "agent", ResourceID: tgt.ID, TeamID: teamID,
			Before: map[string]any{"state": "deleting"}, After: map[string]any{"state": "deleted"},
		})
	})
	return MapError(err, OpWrite, "agent")
}

// RecordRetained implements lifecycle.VolumeRecorder: an audit record names the retained volume, so a
// service admin can find it (the volume outlives the agent, the claim and the namespace).
func (l *AgentLifecycle) RecordRetained(ctx context.Context, d agent.Desired, v lifecycle.RetainedVolume) error {
	err := l.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		return l.audit.Write(ctx, tx, port.AuditRecord{
			Action: "agent.volume_retained", Outcome: port.OutcomeAllowed,
			ResourceKind: "agent", ResourceID: d.ID, TeamID: d.Team.ID,
			After: map[string]any{"namespace": d.Team.Namespace, "claim": v.Claim, "volume": v.Volume},
		})
	})
	return MapError(err, OpWrite, "agent volume")
}

var _ lifecycle.VolumeRecorder = (*AgentLifecycle)(nil)

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/lifecycle"
)

// reconcilerSession is the session of the reconciler's lifecycle writes.
var reconcilerSession = port.SystemSession("afs-reconciler")

// TeamLifecycle implements the database side of the team delete finalizer (M1-I1) for the
// reconciler's login (afs_reconciler).
type TeamLifecycle struct {
	runner port.TxRunner
	audit  port.AuditWriter
}

// NewTeamLifecycle returns a TeamLifecycle.
func NewTeamLifecycle(runner port.TxRunner, audit port.AuditWriter) *TeamLifecycle {
	return &TeamLifecycle{runner: runner, audit: audit}
}

// ListTeamSecrets implements lifecycle.TeamSecrets' listing: every Secrets Manager ARN the team's
// secret rows reference, managed or registered.
func (l *TeamLifecycle) ListTeamSecrets(ctx context.Context, teamID string) ([]string, error) {
	var arns []string
	err := l.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT secrets_manager_arn FROM afs.secrets WHERE team_id = $1 ORDER BY secrets_manager_arn`, teamID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				return err
			}
			arns = append(arns, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, MapError(err, OpRead, "team secrets")
	}
	return arns, nil
}

// Complete implements lifecycle.Completer for a team: state becomes `deleted` with deleted_at set,
// and one audit record is written in the same transaction. Rows, revisions, usage and audit stay.
// Completing an already deleted team is a no-op (idempotent retry after a lost response).
func (l *TeamLifecycle) Complete(ctx context.Context, tgt lifecycle.Target) error {
	if tgt.Kind != "team" {
		return fmt.Errorf("postgres: lifecycle completer handles teams, not %q", tgt.Kind)
	}
	err := l.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var state string
		if err := t.QueryRow(ctx, `SELECT state FROM afs.teams WHERE id = $1 FOR UPDATE`, tgt.ID).Scan(&state); err != nil {
			return err
		}
		switch state {
		case "deleted":
			return nil
		case "deleting":
		default:
			return domain.Conflict("team_not_deleting", fmt.Sprintf("team %s is %s, not deleting", tgt.ID, state), nil)
		}
		if _, err := t.Exec(ctx, `UPDATE afs.teams SET state = 'deleted', deleted_at = now() WHERE id = $1`, tgt.ID); err != nil {
			return err
		}
		return l.audit.Write(ctx, tx, port.AuditRecord{
			Action: "team.delete_finalized", Outcome: port.OutcomeAllowed,
			ResourceKind: "team", ResourceID: tgt.ID, TeamID: tgt.ID,
			Before: map[string]any{"state": "deleting"}, After: map[string]any{"state": "deleted"},
		})
	})
	return MapError(err, OpWrite, "team")
}

var (
	_ lifecycle.Completer = (*TeamLifecycle)(nil)
)

// ConditionStore implements lifecycle.ConditionStore on resource_status. Writes go through the engine's
// StatusWriter, so the phase change to Deleting publishes <kind>.status_changed once and the stored
// observedGeneration is kept.
type ConditionStore struct {
	status *StatusWriter
	runner port.TxRunner
}

// NewConditionStore returns a ConditionStore.
func NewConditionStore(status *StatusWriter, runner port.TxRunner) *ConditionStore {
	return &ConditionStore{status: status, runner: runner}
}

// Conditions implements lifecycle.ConditionStore; a resource without a status row has none.
func (s *ConditionStore) Conditions(ctx context.Context, kind, id string) ([]domain.Condition, error) {
	var conds []domain.Condition
	err := s.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var raw []byte
		err = t.QueryRow(ctx, `SELECT conditions FROM afs.resource_status WHERE resource_kind = $1 AND resource_id = $2`, kind, id).Scan(&raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &conds)
	})
	if err != nil {
		return nil, MapError(err, OpRead, "resource status")
	}
	return conds, nil
}

// Write implements lifecycle.ConditionStore.
func (s *ConditionStore) Write(ctx context.Context, t lifecycle.Target, phase domain.Phase, conds []domain.Condition) error {
	return s.status.Apply(ctx, engine.Report{
		Ref:   engine.NewRef(engine.Kind(t.Kind), t.ID, t.TeamID),
		Phase: phase, Conditions: conds, Replace: true, ReconciledAt: s.status.clock.Now(),
	})
}

var _ lifecycle.ConditionStore = (*ConditionStore)(nil)

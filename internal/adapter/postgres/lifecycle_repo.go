package postgres

import (
	"context"
	"fmt"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
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

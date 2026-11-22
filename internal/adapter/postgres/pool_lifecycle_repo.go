package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/lifecycle"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

// PoolLifecycle implements the database side of the pool delete finalizer (M5-C6) and the pool role link
// recorder for the reconciler's login (afs_reconciler may update agent_pools.state/deleted_at and the status
// columns of pool_role_links, and append audit and outbox rows).
type PoolLifecycle struct {
	runner port.TxRunner
	audit  port.AuditWriter
	outbox port.OutboxWriter
	ids    port.IDGen
}

// NewPoolLifecycle returns a PoolLifecycle.
func NewPoolLifecycle(runner port.TxRunner, audit port.AuditWriter, outbox port.OutboxWriter, ids port.IDGen) *PoolLifecycle {
	return &PoolLifecycle{runner: runner, audit: audit, outbox: outbox, ids: ids}
}

var (
	_ lifecycle.Completer = (*PoolLifecycle)(nil)
	_ pool.LinkRecorder   = (*PoolLifecycle)(nil)
)

// Complete implements lifecycle.Completer for a pool: state becomes `deleted` with deleted_at set and one
// audit record is written in the same transaction. Completing an already deleted pool is a no-op.
func (l *PoolLifecycle) Complete(ctx context.Context, tgt lifecycle.Target) error {
	if tgt.Kind != "pool" {
		return fmt.Errorf("postgres: pool completer handles pools, not %q", tgt.Kind)
	}
	err := l.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var state, teamID string
		if err := t.QueryRow(ctx, `SELECT state, team_id FROM afs.agent_pools WHERE id = $1 FOR UPDATE`, tgt.ID).Scan(&state, &teamID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NotFound("pool")
			}
			return err
		}
		switch state {
		case "deleted":
			return nil
		case "deleting":
		default:
			return domain.Conflict("pool_not_deleting", fmt.Sprintf("pool %s is %s, not deleting", tgt.ID, state), nil)
		}
		if _, err := t.Exec(ctx, `UPDATE afs.agent_pools SET state = 'deleted', deleted_at = now() WHERE id = $1`, tgt.ID); err != nil {
			return err
		}
		return l.audit.Write(ctx, tx, port.AuditRecord{
			Action: "pool.delete_finalized", Outcome: port.OutcomeAllowed,
			ResourceKind: "pool", ResourceID: tgt.ID, TeamID: teamID,
			Before: map[string]any{"state": "deleting"}, After: map[string]any{"state": "deleted"},
		})
	})
	return MapError(err, OpWrite, "pool")
}

// RecordLink implements pool.LinkRecorder: the status columns, one audit record and, for a transition into
// verified or failed, the contract event, in one transaction. The row only changes while it still holds the
// status the pass saw (From).
func (l *PoolLifecycle) RecordLink(ctx context.Context, c pool.LinkChange) error {
	err := l.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var lastErr *string
		if c.Error != "" {
			lastErr = &c.Error
		}
		tag, err := t.Exec(ctx, `UPDATE afs.pool_role_links SET status = $2, last_error = $3,
       last_verified_at = CASE WHEN $4::timestamptz IS NULL THEN last_verified_at ELSE $4 END
 WHERE id = $1 AND status = $5`, c.LinkID, string(c.To), lastErr, c.VerifiedAt, string(c.From))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		if err := l.audit.Write(ctx, tx, port.AuditRecord{Action: "ReconcilePoolRoleLink", Outcome: port.OutcomeAllowed,
			ResourceKind: "pool_role_links", ResourceID: c.LinkID, TeamID: c.TeamID,
			Before: map[string]any{"status": string(c.From)},
			After:  map[string]any{"status": string(c.To), "error": c.Error, "poolId": c.PoolID, "accountId": c.AccountID}}); err != nil {
			return err
		}
		if c.Event == "" {
			return nil
		}
		id, err := l.ids.NewID(eventIDPrefix)
		if err != nil {
			return err
		}
		data := map[string]any{"linkId": c.LinkID, "poolId": c.PoolID, "accountId": c.AccountID, "roleArn": c.RoleARN, "status": string(c.To)}
		if c.Error != "" {
			data["error"] = c.Error
		}
		ev, err := domain.NewEvent(c.Event, id.String(), time.Now().UTC(), tx.Session().Actor, c.TeamID, "pool_role_link", c.LinkID, data)
		if err != nil {
			return err
		}
		return l.outbox.Publish(ctx, tx, ev)
	})
	return MapError(err, OpWrite, "pool role link")
}

package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/teardown"
)

// TeardownStore implements teardown.Store for the reconciler's login: afs_reconciler reads
// afs.teams_due_for_teardown and may update teams.state, torn_down_at and teardown_reason (and deleted_at,
// which the delete finalizer sets).
type TeardownStore struct {
	runner port.TxRunner
	audit  port.AuditWriter
	outbox port.OutboxWriter
	ids    port.IDGen
}

// NewTeardownStore returns a TeardownStore.
func NewTeardownStore(runner port.TxRunner, audit port.AuditWriter, outbox port.OutboxWriter, ids port.IDGen) *TeardownStore {
	return &TeardownStore{runner: runner, audit: audit, outbox: outbox, ids: ids}
}

var _ teardown.Store = (*TeardownStore)(nil)

// Due implements teardown.Store.
func (s *TeardownStore) Due(ctx context.Context) ([]teardown.Due, error) {
	var out []teardown.Due
	err := s.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return collect(ctx, t, "teams due for teardown", `SELECT team_id, teardown_rule, coalesce(reason, '') FROM afs.teams_due_for_teardown
 WHERE reason IS NOT NULL ORDER BY team_id`, nil, func(rows pgx.Rows) error {
			var d teardown.Due
			if err := rows.Scan(&d.TeamID, &d.Rule, &d.Reason); err != nil {
				return err
			}
			out = append(out, d)
			return nil
		})
	})
	return out, err
}

// Begin implements teardown.Store. torn_down_at is the database's now(), so it is always earlier than the
// deleted_at the finalizer's transaction sets later (no clock skew between the reconciler and the database
// can reverse them); `at` stamps the event. The row is locked and re-checked, so a team that was deleted, suspended or
// already started meanwhile is left alone and nothing is written.
func (s *TeardownStore) Begin(ctx context.Context, d teardown.Due, at time.Time) (bool, error) {
	started := false
	err := s.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		started = false
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var state, kind string
		if err := t.QueryRow(ctx, `SELECT state, kind FROM afs.teams WHERE id = $1 FOR UPDATE`, d.TeamID).Scan(&state, &kind); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if state != "active" || kind != "ephemeral" {
			return nil
		}
		if _, err := t.Exec(ctx, `UPDATE afs.teams SET state = 'deleting', torn_down_at = now(), teardown_reason = $2 WHERE id = $1`, d.TeamID, d.Reason); err != nil {
			return err
		}
		if err := s.audit.Write(ctx, tx, port.AuditRecord{Action: "team.teardown_started", Outcome: port.OutcomeAllowed,
			ResourceKind: "team", ResourceID: d.TeamID, TeamID: d.TeamID,
			Before: map[string]any{"state": "active"}, After: map[string]any{"state": "deleting", "reason": d.Reason, "rule": d.Rule}}); err != nil {
			return err
		}
		if err := s.publish(ctx, tx, domain.EventTeamTeardownStarted, d.TeamID, d.Reason, d.Rule, at); err != nil {
			return err
		}
		started = true
		return nil
	})
	return started, MapError(err, OpWrite, "team teardown")
}

// Finished implements teardown.Store: a deleted team whose torn_down_at is earlier than its deleted_at has been
// started but not closed (Close sets the two equal), which holds without reading the outbox, whose rows are
// purged after their retention.
func (s *TeardownStore) Finished(ctx context.Context) ([]teardown.Finished, error) {
	var out []teardown.Finished
	err := s.runner.ReadTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return collect(ctx, t, "torn down teams", `SELECT id, teardown_reason FROM afs.teams
 WHERE state = 'deleted' AND torn_down_at IS NOT NULL AND deleted_at IS NOT NULL AND torn_down_at < deleted_at ORDER BY id`, nil, func(rows pgx.Rows) error {
			var f teardown.Finished
			if err := rows.Scan(&f.TeamID, &f.Reason); err != nil {
				return err
			}
			out = append(out, f)
			return nil
		})
	})
	return out, err
}

// Close implements teardown.Store.
func (s *TeardownStore) Close(ctx context.Context, f teardown.Finished) error {
	err := s.runner.InTx(ctx, reconcilerSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		var tornDown, deleted time.Time
		var reason string
		if err := t.QueryRow(ctx, `SELECT torn_down_at, deleted_at, teardown_reason FROM afs.teams
 WHERE id = $1 AND state = 'deleted' AND torn_down_at IS NOT NULL AND deleted_at IS NOT NULL FOR UPDATE`, f.TeamID).Scan(&tornDown, &deleted, &reason); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		if !tornDown.Before(deleted) {
			return nil // closed already
		}
		if _, err := t.Exec(ctx, `UPDATE afs.teams SET torn_down_at = deleted_at WHERE id = $1`, f.TeamID); err != nil {
			return err
		}
		if err := s.audit.Write(ctx, tx, port.AuditRecord{Action: "team.torn_down", Outcome: port.OutcomeAllowed,
			ResourceKind: "team", ResourceID: f.TeamID, TeamID: f.TeamID,
			Before: map[string]any{"state": "deleting"}, After: map[string]any{"state": "deleted", "reason": reason}}); err != nil {
			return err
		}
		return s.publish(ctx, tx, domain.EventTeamTornDown, f.TeamID, reason, "", deleted)
	})
	return MapError(err, OpWrite, "team teardown")
}

func (s *TeardownStore) publish(ctx context.Context, tx port.Tx, typ domain.EventType, teamID, reason, rule string, at time.Time) error {
	id, err := s.ids.NewID(eventIDPrefix)
	if err != nil {
		return err
	}
	data := map[string]any{"teamId": teamID, "reason": reason}
	if rule != "" {
		data["rule"] = rule
	}
	ev, err := domain.NewEvent(typ, id.String(), at.UTC(), tx.Session().Actor, teamID, "team", teamID, data)
	if err != nil {
		return err
	}
	return s.outbox.Publish(ctx, tx, ev)
}

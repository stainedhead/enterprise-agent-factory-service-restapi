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
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// ReconcilerSession is the session of every write the reconciler makes (database contract §3: service
// roles set afs.actor to system:<service>).
func ReconcilerSession() port.Session { return port.SystemSession("afs-reconciler") }

// eventIDPrefix prefixes event ids; the contract only requires a ULID.
const eventIDPrefix domain.IDPrefix = "evt"

// StatusWriter implements engine.StatusWriter on resource_status. In the transaction that upserts
// the row it writes the <kind>.status_changed outbox event when the phase changed (CT-API-7).
type StatusWriter struct {
	runner *TxRunner
	outbox port.OutboxWriter
	ids    port.IDGen
	clock  port.Clock
}

// NewStatusWriter returns a StatusWriter.
func NewStatusWriter(runner *TxRunner, outbox port.OutboxWriter, ids port.IDGen, clock port.Clock) *StatusWriter {
	return &StatusWriter{runner: runner, outbox: outbox, ids: ids, clock: clock}
}

const selectStatusSQL = `SELECT phase, observed_generation, conditions, drift_detected_at, last_reconciled_at
  FROM afs.resource_status WHERE resource_kind = $1 AND resource_id = $2 FOR UPDATE`

const upsertStatusSQL = `INSERT INTO afs.resource_status
  (resource_kind, resource_id, team_id, observed_generation, phase, conditions, drift_detected_at, last_reconciled_at)
VALUES ($1, $2, nullif($3, ''), $4, $5, $6, $7, $8)
ON CONFLICT (resource_kind, resource_id) DO UPDATE SET
  observed_generation = EXCLUDED.observed_generation, phase = EXCLUDED.phase, conditions = EXCLUDED.conditions,
  drift_detected_at = EXCLUDED.drift_detected_at, last_reconciled_at = EXCLUDED.last_reconciled_at`

// Apply implements engine.StatusWriter.
func (w *StatusWriter) Apply(ctx context.Context, r engine.Report) error {
	return w.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		prev, err := loadStatus(ctx, t, r)
		if err != nil {
			return err
		}
		next, previous, changed := engine.MergeStatus(prev, r)
		conds, err := json.Marshal(next.Conditions)
		if err != nil {
			return fmt.Errorf("postgres: status conditions: %w", err)
		}
		if _, err := t.Exec(ctx, upsertStatusSQL, string(r.Kind), r.ID, r.TeamID, next.ObservedGeneration,
			string(next.Phase), conds, next.DriftDetectedAt, next.LastReconciledAt); err != nil {
			return MapError(err, OpWrite, "resource status")
		}
		if !changed {
			return nil
		}
		return w.publishChange(ctx, tx, r, previous, next)
	})
}

func loadStatus(ctx context.Context, t *Tx, r engine.Report) (*engine.Stored, error) {
	var (
		s      engine.Stored
		phase  string
		conds  []byte
		drift  *time.Time
		lastAt *time.Time
	)
	err := t.QueryRow(ctx, selectStatusSQL, string(r.Kind), r.ID).Scan(&phase, &s.ObservedGeneration, &conds, &drift, &lastAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, MapError(err, OpRead, "resource status")
	}
	s.Phase, s.DriftDetectedAt, s.LastReconciledAt = domain.Phase(phase), drift, lastAt
	if err := json.Unmarshal(conds, &s.Conditions); err != nil {
		return nil, fmt.Errorf("postgres: status conditions of %s/%s: %w", r.Kind, r.ID, err)
	}
	return &s, nil
}

func (w *StatusWriter) publishChange(ctx context.Context, tx port.Tx, r engine.Report, previous domain.Phase, next engine.Stored) error {
	typ, ok := engine.StatusEvent(r.Kind)
	if !ok {
		return nil
	}
	id, err := w.ids.NewID(eventIDPrefix)
	if err != nil {
		return err
	}
	ev, err := domain.NewEvent(typ, id.String(), w.clock.Now(), tx.Session().Actor, r.TeamID, string(r.Kind), r.ID, map[string]any{
		"phase": string(next.Phase), "previousPhase": string(previous), "observedGeneration": next.ObservedGeneration,
	})
	if err != nil {
		return err
	}
	switch r.Kind {
	case engine.KindAgent:
		ev.Detail.AgentID = r.ID
	case engine.KindPool:
		ev.Detail.PoolID = r.ID
	}
	return w.outbox.Publish(ctx, tx, ev)
}

// DriftRecorder implements engine.DriftRecorder: one audit record and one drift.corrected event per
// corrected object, in one transaction.
type DriftRecorder struct {
	runner *TxRunner
	audit  port.AuditWriter
	outbox port.OutboxWriter
	ids    port.IDGen
}

// NewDriftRecorder returns a DriftRecorder.
func NewDriftRecorder(runner *TxRunner, audit port.AuditWriter, outbox port.OutboxWriter, ids port.IDGen) *DriftRecorder {
	return &DriftRecorder{runner: runner, audit: audit, outbox: outbox, ids: ids}
}

// Record implements engine.DriftRecorder.
func (d *DriftRecorder) Record(ctx context.Context, rec engine.DriftRecord) error {
	return d.runner.InTx(ctx, ReconcilerSession(), func(ctx context.Context, tx port.Tx) error {
		err := d.audit.Write(ctx, tx, port.AuditRecord{
			Action: string(domain.EventDriftCorrected), Outcome: port.OutcomeAllowed,
			ResourceKind: string(rec.Kind), ResourceID: rec.ID, TeamID: rec.TeamID,
			After: map[string]any{"object": rec.Object},
		})
		if err != nil {
			return err
		}
		id, err := d.ids.NewID(eventIDPrefix)
		if err != nil {
			return err
		}
		ev, err := domain.NewEvent(domain.EventDriftCorrected, id.String(), rec.At, tx.Session().Actor, rec.TeamID, string(rec.Kind), rec.ID,
			map[string]any{"object": rec.Object, "resourceKind": string(rec.Kind), "resourceId": rec.ID})
		if err != nil {
			return err
		}
		return d.outbox.Publish(ctx, tx, ev)
	})
}

var (
	_ engine.StatusWriter  = (*StatusWriter)(nil)
	_ engine.DriftRecorder = (*DriftRecorder)(nil)
)

package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/housekeeping"
)

// Housekeeping implements the database ports of the reconciler's housekeeping jobs (M1-J) for the
// afs_reconciler login: the outbox, partition management and the idempotency purge.
type Housekeeping struct {
	runner port.TxRunner
}

// NewHousekeeping returns a Housekeeping on the reconciler's runner.
func NewHousekeeping(runner port.TxRunner) *Housekeeping { return &Housekeeping{runner: runner} }

var hkSession = port.SystemSession("afs-reconciler")

// Pending implements housekeeping.OutboxStore.
func (h *Housekeeping) Pending(ctx context.Context, limit int) ([]housekeeping.OutboxEvent, error) {
	var out []housekeeping.OutboxEvent
	err := h.runner.ReadTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT id, event_type, aggregate_kind, aggregate_id, coalesce(team_id, ''), payload, created_at, attempts
FROM afs.outbox_events WHERE published_at IS NULL ORDER BY id LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e housekeeping.OutboxEvent
			if err := rows.Scan(&e.ID, &e.Type, &e.AggregateKind, &e.AggregateID, &e.TeamID, &e.Payload, &e.CreatedAt, &e.Attempts); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, MapError(err, OpRead, "outbox")
}

// MarkPublished implements housekeeping.OutboxStore.
func (h *Housekeeping) MarkPublished(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return MapError(h.runner.InTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		_, err = t.Exec(ctx, `UPDATE afs.outbox_events SET published_at = now() WHERE id = ANY($1) AND published_at IS NULL`, ids)
		return err
	}), OpWrite, "outbox")
}

// RecordFailure implements housekeeping.OutboxStore.
func (h *Housekeeping) RecordFailure(ctx context.Context, id int64) error {
	return MapError(h.runner.InTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		_, err = t.Exec(ctx, `UPDATE afs.outbox_events SET attempts = attempts + 1 WHERE id = $1 AND published_at IS NULL`, id)
		return err
	}), OpWrite, "outbox")
}

// Lag implements housekeeping.OutboxStore.
func (h *Housekeeping) Lag(ctx context.Context) (int, time.Duration, error) {
	var n int
	var secs float64
	err := h.runner.ReadTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return t.QueryRow(ctx, `SELECT count(*), coalesce(extract(epoch FROM now() - min(created_at)), 0)::float8
FROM afs.outbox_events WHERE published_at IS NULL`).Scan(&n, &secs)
	})
	return n, time.Duration(secs * float64(time.Second)), MapError(err, OpRead, "outbox")
}

var ensureFunc = map[housekeeping.PartitionTable]string{
	housekeeping.TableUsage:    "afs.ensure_usage_partition",
	housekeeping.TableActivity: "afs.ensure_activity_partition",
	housekeeping.TableTraffic:  "afs.ensure_traffic_partition",
}

// EnsurePartition implements housekeeping.PartitionOps.
func (h *Housekeeping) EnsurePartition(ctx context.Context, table housekeeping.PartitionTable, month time.Time) (string, error) {
	fn, ok := ensureFunc[table]
	if !ok {
		return "", fmt.Errorf("postgres: no partition function for table %q", table)
	}
	first := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	var name string
	err := h.runner.InTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return t.QueryRow(ctx, `SELECT `+fn+`($1::date)`, first).Scan(&name)
	})
	return name, MapError(err, OpWrite, "partition")
}

// DropOlderThan implements housekeeping.PartitionOps. afs.drop_partitions_older_than touches only
// usage_events, agent_activity and external_traffic; audit_log is not partitioned and is untouched.
func (h *Housekeeping) DropOlderThan(ctx context.Context, months int) ([]string, error) {
	var names []string
	err := h.runner.InTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		rows, err := t.Query(ctx, `SELECT * FROM afs.drop_partitions_older_than($1)`, months)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			names = append(names, n)
		}
		return rows.Err()
	})
	return names, MapError(err, OpWrite, "partition")
}

// PurgeIdempotencyKeys implements housekeeping.KeyPurger.
func (h *Housekeeping) PurgeIdempotencyKeys(ctx context.Context) (int64, error) {
	var n int64
	err := h.runner.InTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return t.QueryRow(ctx, `SELECT afs.purge_idempotency_keys()`).Scan(&n)
	})
	return n, MapError(err, OpWrite, "idempotency keys")
}

var (
	_ housekeeping.OutboxStore  = (*Housekeeping)(nil)
	_ housekeeping.PartitionOps = (*Housekeeping)(nil)
	_ housekeeping.KeyPurger    = (*Housekeeping)(nil)
)

// Recent implements rollup.Store: afs.rollup_reports_recent(days) (-db 000018) recomputes the daily
// activity and traffic rollups for the last days UTC days and returns the rows written.
func (h *Housekeeping) Recent(ctx context.Context, days int) (int64, error) {
	var n int64
	err := h.runner.InTx(ctx, hkSession, func(ctx context.Context, tx port.Tx) error {
		t, err := Unwrap(tx)
		if err != nil {
			return err
		}
		return t.QueryRow(ctx, `SELECT afs.rollup_reports_recent($1::int)`, days).Scan(&n)
	})
	return n, MapError(err, OpWrite, "report rollup")
}

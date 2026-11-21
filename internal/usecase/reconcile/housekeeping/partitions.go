package housekeeping

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// PartitionTable names one monthly partitioned fact table.
type PartitionTable string

// The three partitioned tables. audit_log is not among them and is never dropped from (D36).
const (
	TableUsage    PartitionTable = "usage_events"
	TableActivity PartitionTable = "agent_activity"
	TableTraffic  PartitionTable = "external_traffic"
)

// Tables lists the partitioned tables in the order they are handled.
func Tables() []PartitionTable { return []PartitionTable{TableUsage, TableActivity, TableTraffic} }

// RetentionMonths is how many whole months of fact rows are kept (D36).
const RetentionMonths = 13

// MonthsAhead is how many months after the current one get a partition. Two means a run on the
// last day of a month finds next month present with a month of margin should a run be missed.
const MonthsAhead = 2

// PartitionOps is the database side (afs.ensure_*_partition, afs.drop_partitions_older_than).
type PartitionOps interface {
	// EnsurePartition creates the partition of table for the month containing month (no-op when it
	// exists) and returns its name.
	EnsurePartition(ctx context.Context, table PartitionTable, month time.Time) (string, error)
	// DropOlderThan drops monthly partitions older than months whole months and returns their names.
	DropOlderThan(ctx context.Context, months int) ([]string, error)
}

// PartitionManager runs the partition jobs (M1-J2).
type PartitionManager struct {
	Ops   PartitionOps
	Clock func() time.Time
	Log   *slog.Logger

	mu       sync.Mutex
	lastDrop string // YYYY-MM of the last successful drop
	dropRuns int
}

// Ensure creates the partitions of the current month and the next MonthsAhead months for every
// table. Every table and month is attempted; the errors are joined.
func (m *PartitionManager) Ensure(ctx context.Context) error {
	now := m.Clock().UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var errs []error
	for _, t := range Tables() {
		for i := 0; i <= MonthsAhead; i++ {
			month := first.AddDate(0, i, 0)
			if _, err := m.Ops.EnsurePartition(ctx, t, month); err != nil {
				errs = append(errs, fmt.Errorf("housekeeping: ensure %s partition %s: %w", t, month.Format("2006-01"), err))
			}
		}
	}
	return errors.Join(errs...)
}

// DropMonthly drops partitions older than RetentionMonths, at most once per calendar month: the job
// is scheduled daily and does nothing after the first success of a month (a run missed on the 1st
// is made up on the next day). A restart repeats the drop, which is idempotent.
func (m *PartitionManager) DropMonthly(ctx context.Context) error {
	month := m.Clock().UTC().Format("2006-01")
	m.mu.Lock()
	done := m.lastDrop == month
	m.mu.Unlock()
	if done {
		return nil
	}
	names, err := m.Ops.DropOlderThan(ctx, RetentionMonths)
	if err != nil {
		return fmt.Errorf("housekeeping: drop old partitions: %w", err)
	}
	m.mu.Lock()
	m.lastDrop = month
	m.dropRuns++
	m.mu.Unlock()
	if len(names) > 0 && m.Log != nil {
		m.Log.InfoContext(ctx, "dropped partitions", "partitions", names)
	}
	return nil
}

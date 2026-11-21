package housekeeping

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Scheduler is the engine's leader-only periodic job scheduler (M1-F4 `Every`). A job runs only on
// the leader; an error is logged and the job runs again at the next interval.
type Scheduler interface {
	Every(name string, interval time.Duration, fn func(ctx context.Context) error)
}

// Job is one periodic housekeeping job.
type Job struct {
	Name     string
	Interval time.Duration
	Run      func(ctx context.Context) error
}

// Intervals of the jobs.
const (
	OutboxInterval    = time.Second
	PartitionInterval = 24 * time.Hour
	PurgeInterval     = 5 * time.Minute
)

// Deps are the ports of the jobs.
type Deps struct {
	Outbox     OutboxStore
	Sink       EventSink
	Partitions PartitionOps
	Purger     KeyPurger
	Recorder   Recorder
	Clock      func() time.Time
	Log        *slog.Logger
}

// Jobs returns every housekeeping job of the reconciler.
func Jobs(d Deps) []Job {
	clock := d.Clock
	if clock == nil {
		clock = time.Now
	}
	pub := NewPublisher(d.Outbox, d.Sink, d.Recorder, clock)
	pm := &PartitionManager{Ops: d.Partitions, Clock: clock, Log: d.Log}
	jobs := []Job{
		{Name: "outbox-publisher", Interval: OutboxInterval, Run: pub.Drain},
		{Name: "partitions-ensure", Interval: PartitionInterval, Run: pm.Ensure},
		{Name: "partitions-drop", Interval: PartitionInterval, Run: pm.DropMonthly},
		{Name: "idempotency-purge", Interval: PurgeInterval, Run: (&Purge{Purger: d.Purger, Log: d.Log}).Run},
	}
	return jobs
}

// Register adds the jobs to the scheduler.
func Register(s Scheduler, jobs []Job) {
	for _, j := range jobs {
		s.Every(j.Name, j.Interval, j.Run)
	}
}

// KeyPurger deletes expired idempotency keys (afs.purge_idempotency_keys).
type KeyPurger interface {
	PurgeIdempotencyKeys(ctx context.Context) (int64, error)
}

// Purge is the idempotency purge job (M1-J3).
type Purge struct {
	Purger KeyPurger
	Log    *slog.Logger
}

// Run purges once.
func (p *Purge) Run(ctx context.Context) error {
	n, err := p.Purger.PurgeIdempotencyKeys(ctx)
	if err != nil {
		return fmt.Errorf("housekeeping: purge idempotency keys: %w", err)
	}
	if n > 0 && p.Log != nil {
		p.Log.InfoContext(ctx, "purged idempotency keys", "count", n)
	}
	return nil
}

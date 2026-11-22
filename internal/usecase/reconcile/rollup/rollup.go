// Package rollup runs the reconciler's report rollup jobs: they call afs.rollup_reports_recent so the
// service activity and traffic reports over more than two days read one row per group and day (-db 000018).
package rollup

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Cadence of the jobs. The recent job recomputes today and yesterday; the nightly job the last week, which
// picks up late samples and a missed run. Both are idempotent.
const (
	RecentInterval  = 10 * time.Minute
	RecentDays      = 2
	NightlyInterval = 24 * time.Hour
	NightlyDays     = 7
)

// Store recomputes the rollups for the last days UTC days and returns the rows written.
type Store interface {
	Recent(ctx context.Context, days int) (int64, error)
}

// Recorder receives the outcome of a run (metrics). It may be nil.
type Recorder interface {
	RollupRun(job string, rows int64, err error)
}

// Scheduler is the engine's leader-only periodic job scheduler.
type Scheduler interface {
	Every(name string, interval time.Duration, fn func(ctx context.Context) error)
}

// Job is one rollup job.
type Job struct {
	Name     string
	Interval time.Duration
	Days     int
}

// Jobs lists the two rollup jobs.
func Jobs() []Job {
	return []Job{
		{Name: "report-rollup-recent", Interval: RecentInterval, Days: RecentDays},
		{Name: "report-rollup-nightly", Interval: NightlyInterval, Days: NightlyDays},
	}
}

// Runner runs rollup jobs.
type Runner struct {
	Store    Store
	Recorder Recorder
	Log      *slog.Logger
}

// Run executes one job once, recording the outcome.
func (r *Runner) Run(ctx context.Context, j Job) error {
	rows, err := r.Store.Recent(ctx, j.Days)
	if r.Recorder != nil {
		r.Recorder.RollupRun(j.Name, rows, err)
	}
	if err != nil {
		return fmt.Errorf("rollup: %s over %d days: %w", j.Name, j.Days, err)
	}
	if r.Log != nil {
		r.Log.DebugContext(ctx, "report rollup", "job", j.Name, "days", j.Days, "rows", rows)
	}
	return nil
}

// Register adds both jobs to the scheduler.
func (r *Runner) Register(s Scheduler) {
	for _, j := range Jobs() {
		s.Every(j.Name, j.Interval, func(ctx context.Context) error { return r.Run(ctx, j) })
	}
}

// Package teardown is the ephemeral team teardown job (M5-D1, RC-10): it finds ephemeral teams that are due
// (expired, at their budget cap, or with their work complete), starts the delete flow of RC-17 for them, and
// records the end of the teardown. The delete itself is the team finalizer's: the job only moves the team to
// `deleting`, which the team controller acts on, and later closes the record once the team is `deleted`.
// Configuration, revisions, usage and audit stay, as for any deleted team.
package teardown

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DefaultInterval is how often the job runs; a due team starts its teardown within one interval, well inside
// the five minutes RC-10 allows.
const DefaultInterval = 30 * time.Second

// Reasons a team is torn down (the contract's data.reason).
const (
	ReasonExpired      = "expired"
	ReasonCapReached   = "cap_reached"
	ReasonWorkComplete = "work_complete"
)

// Due is a team the database says is due for teardown.
type Due struct {
	TeamID string
	// Rule is the team's teardown rule (at_expiry, at_cap_or_expiry, at_work_complete).
	Rule string
	// Reason is why it is due.
	Reason string
}

// Finished is a team whose delete flow ended (state deleted) and whose teardown is not yet recorded.
type Finished struct {
	TeamID string
	Reason string
}

// Store is the database side of the job. Every method is idempotent and safe under a retry after a lost
// response.
type Store interface {
	// Due lists the active ephemeral teams that are due.
	Due(ctx context.Context) ([]Due, error)
	// Begin moves the team to `deleting`, records torn_down_at (at) and the reason, and writes one audit
	// record and the team.teardown_started event, in one transaction. It reports false, writing nothing, when
	// the team is no longer active (someone else deleted or suspended it meanwhile).
	Begin(ctx context.Context, d Due, at time.Time) (bool, error)
	// Finished lists the deleted teams whose teardown has no closing record yet.
	Finished(ctx context.Context) ([]Finished, error)
	// Close stamps torn_down_at with the time the team was deleted and writes one audit record and the
	// team.torn_down event, in one transaction. A team already closed is left alone.
	Close(ctx context.Context, f Finished) error
}

// Clock is the time source.
type Clock func() time.Time

// Job runs one pass of the teardown flow.
type Job struct {
	Store Store
	Now   Clock
	Log   *slog.Logger
}

// Run starts the teardown of every due team and closes the record of every finished one. A failure for one
// team does not stop the others; the errors are joined, and the next pass retries what failed.
func (j *Job) Run(ctx context.Context) error {
	var errs []error
	due, err := j.Store.Due(ctx)
	if err != nil {
		return fmt.Errorf("teardown: list due teams: %w", err)
	}
	for _, d := range due {
		if d.Reason == "" {
			continue // listed without a reason: not due after all
		}
		started, err := j.Store.Begin(ctx, d, j.Now())
		if err != nil {
			errs = append(errs, fmt.Errorf("teardown: start %s: %w", d.TeamID, err))
			continue
		}
		if started {
			j.log().Info("ephemeral team teardown started", "team", d.TeamID, "reason", d.Reason, "rule", d.Rule)
		}
	}
	done, err := j.Store.Finished(ctx)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("teardown: list finished teams: %w", err))...)
	}
	for _, f := range done {
		if err := j.Store.Close(ctx, f); err != nil {
			errs = append(errs, fmt.Errorf("teardown: close %s: %w", f.TeamID, err))
			continue
		}
		j.log().Info("ephemeral team torn down", "team", f.TeamID, "reason", f.Reason)
	}
	return errors.Join(errs...)
}

func (j *Job) log() *slog.Logger {
	if j.Log != nil {
		return j.Log
	}
	return slog.Default()
}

// Scheduler runs a function on an interval while the process leads (engine.Engine.Every).
type Scheduler interface {
	Every(name string, interval time.Duration, fn func(ctx context.Context) error)
}

// Register adds the job to the scheduler.
func Register(s Scheduler, j *Job) { s.Every("ephemeral-teardown", DefaultInterval, j.Run) }

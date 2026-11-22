package workorder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// AssignStore is the database side of team creation.
type AssignStore interface {
	// Unassigned lists the open work orders that have a template and no team, oldest first.
	Unassigned(ctx context.Context) ([]string, error)
	// Instantiate creates the work order's ephemeral remediation team from its swarm template and assigns the
	// work order (afs.instantiate_work_order_team, X-DB-4). The function is idempotent: a work order that has a
	// team returns it. The team, its budget, models, stages, pools, handoffs and configuration, the audit record
	// and the team.created and work_order.assigned events are written by the function in one transaction.
	Instantiate(ctx context.Context, workOrderID string) (teamID string, err error)
}

// Backoff bounds of a failing work order (a disabled template, a template without a cost center).
const (
	failureBase = 30 * time.Second
	failureMax  = 10 * time.Minute
)

// Assigner creates remediation teams (RC-US-10.2) and provisions the secrets their templates declare.
type Assigner struct {
	Store   AssignStore
	Secrets *Secrets // optional
	Now     Clock
	Log     *slog.Logger

	failures map[string]failure
}

type failure struct {
	n     int
	until time.Time
}

// Run assigns every open work order that has a template, then provisions pending template secrets. A failing
// work order does not stop the others; it is retried with a growing delay (30 s up to 10 min), so a disabled
// template is not retried in a hot loop.
func (a *Assigner) Run(ctx context.Context) error {
	if a.failures == nil {
		a.failures = map[string]failure{}
	}
	ids, err := a.Store.Unassigned(ctx)
	if err != nil {
		return fmt.Errorf("workorder assign: list: %w", err)
	}
	var errs []error
	live := map[string]bool{}
	now := a.Now()
	for _, id := range ids {
		live[id] = true
		if f, ok := a.failures[id]; ok && now.Before(f.until) {
			continue
		}
		team, err := a.Store.Instantiate(ctx, id)
		if err != nil {
			f := a.failures[id]
			f.n++
			f.until = now.Add(min(failureBase<<min(f.n-1, 10), failureMax))
			a.failures[id] = f
			errs = append(errs, fmt.Errorf("workorder assign: %s: %w", id, err))
			continue
		}
		delete(a.failures, id)
		a.log().Info("remediation team created", "workOrder", id, "team", team)
	}
	for id := range a.failures {
		if !live[id] {
			delete(a.failures, id)
		}
	}
	if a.Secrets != nil {
		if err := a.Secrets.Run(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (a *Assigner) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

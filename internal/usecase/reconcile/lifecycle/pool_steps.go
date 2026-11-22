package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
	teamrec "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Step names of the pool finalizer, in the order they run.
const (
	StepStopWorkers    = "StopWorkers"
	StepDeletePoolVols = "DeleteVolumes"
	StepPoolPodIdent   = "PodIdentity"
	StepPoolRole       = "PoolRole"
	StepPoolInbox      = "InboxQueue"
)

// PoolWorkloads stops a pool's workers.
type PoolWorkloads interface {
	// Stop removes the Deployment and the objects it referenced; volume claims are not touched.
	Stop(ctx context.Context, d pool.Desired) error
	// Stopped reports that the Deployment and every worker pod of the pool are gone.
	Stopped(ctx context.Context, d pool.Desired) (bool, error)
}

// PoolVolumes acts on a pool's volumes: the shared EFS claim and any scratch claim left behind.
type PoolVolumes interface {
	// DeleteVolumes deletes every claim of the pool; a missing claim is success.
	DeleteVolumes(ctx context.Context, d pool.Desired) error
	// VolumesGone reports that no claim of the pool is left.
	VolumesGone(ctx context.Context, d pool.Desired) (bool, error)
}

// QueueDeleter deletes a pool's inbox queue.
type QueueDeleter interface {
	// Delete removes the queue named name; a missing queue is success.
	Delete(ctx context.Context, name string) error
}

// PoolDeps are the ports of the pool delete steps.
type PoolDeps struct {
	Source    pool.Source
	Workloads PoolWorkloads
	Volumes   PoolVolumes
	Pods      agent.PodIdentities
	IAM       RoleDeleter
	Queues    QueueDeleter
}

// PoolTarget is the finalizer's target for a pool.
func PoolTarget(d pool.Desired) Target {
	return Target{Kind: string(engine.KindPool), ID: d.ID, TeamID: d.Team.ID, Namespace: d.Team.Namespace, Slug: d.Team.Slug}
}

// PoolSteps returns the ordered delete steps of a pool (RC-17): stop the workers and wait until their pods are
// gone, delete the volumes (the shared claim and any scratch claim left behind), the Pod Identity association,
// the pool's role and its inbox queue. A pool has no snapshot and no retained volume: scratch space is
// disposable and a shared volume holds working files of a team that is going away. Every step is idempotent.
func PoolSteps(d PoolDeps) []Step {
	run := func(name string, fn func(ctx context.Context, p pool.Desired) error) Step {
		return StepFunc{StepName: name, Fn: func(ctx context.Context, t Target) error {
			p, err := d.Source.LoadPool(ctx, t.ID)
			if err != nil {
				return err
			}
			return fn(ctx, p)
		}}
	}
	return []Step{
		run(StepStopWorkers, func(ctx context.Context, p pool.Desired) error {
			if err := d.Workloads.Stop(ctx, p); err != nil {
				return err
			}
			gone, err := d.Workloads.Stopped(ctx, p)
			if err != nil {
				return err
			}
			if !gone {
				return fmt.Errorf("the workers of %s are terminating: %w", p.Name, ErrPending)
			}
			return nil
		}),
		run(StepDeletePoolVols, func(ctx context.Context, p pool.Desired) error {
			if err := d.Volumes.DeleteVolumes(ctx, p); err != nil {
				return err
			}
			gone, err := d.Volumes.VolumesGone(ctx, p)
			if err != nil {
				return err
			}
			if !gone {
				return fmt.Errorf("the volumes of %s are being deleted: %w", p.Name, ErrPending)
			}
			return nil
		}),
		run(StepPoolPodIdent, func(ctx context.Context, p pool.Desired) error {
			return d.Pods.Delete(ctx, p.Team.Namespace, pool.ServiceAccountName(p.Name))
		}),
		run(StepPoolRole, func(ctx context.Context, p pool.Desired) error {
			name, err := pool.RoleName(p.ID)
			if err != nil {
				return err
			}
			return d.IAM.DeleteRole(ctx, teamrec.RolePath(p.Team.Namespace), name)
		}),
		run(StepPoolInbox, func(ctx context.Context, p pool.Desired) error {
			name, err := pool.QueueName(p.Team.ID, p.ID)
			if err != nil {
				return err
			}
			return d.Queues.Delete(ctx, name)
		}),
	}
}

// NewPoolFinalizer returns the finalizer of a pool.
func NewPoolFinalizer(d PoolDeps, store ConditionStore, c Completer) *Finalizer {
	return &Finalizer{Steps: PoolSteps(d), Store: store, Completer: c}
}

// PoolDeleting returns the pool controller's Hooks.Deleting: one finalizer pass per reconcile, with the same
// outcome rules as TeamDeleting and AgentDeleting.
func PoolDeleting(f *Finalizer) func(ctx context.Context, d pool.Desired) (engine.Outcome, error) {
	return func(ctx context.Context, d pool.Desired) (engine.Outcome, error) {
		res, err := f.Reconcile(ctx, PoolTarget(d))
		out := engine.Outcome{Generation: d.Generation, Phase: domain.PhaseDeleting, NoStatus: true, Requeue: res.RequeueAfter}
		if err != nil {
			return out, err
		}
		if res.Done {
			out.Phase = ""
		}
		return out, nil
	}
}

// TeamPoolsStep is the team finalizer's step for pools (Hooks.Pools): every pool of the team goes through its
// own finalizer, so each stops its workers and frees its volumes and queue before the team's role, secrets
// and namespace go. It is pending while any pool still has steps to run.
func TeamPoolsStep(src pool.Source, f *Finalizer) Step {
	return StepFunc{StepName: "Pools", Fn: func(ctx context.Context, t Target) error {
		list, err := src.ListTeamPools(ctx, t.TeamID)
		if err != nil {
			return fmt.Errorf("list pools: %w", err)
		}
		pending := 0
		var errs []error
		for _, l := range list {
			res, err := f.Reconcile(ctx, Target{Kind: string(engine.KindPool), ID: l.ID, TeamID: t.TeamID, Namespace: l.Namespace, Slug: t.Slug})
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("pool %s: %w", l.Name, err))
			case !res.Done:
				pending++
			}
		}
		if err := errors.Join(errs...); err != nil {
			return err
		}
		if pending > 0 {
			return fmt.Errorf("%d pool(s) of the team are being deleted: %w", pending, ErrPending)
		}
		return nil
	}}
}

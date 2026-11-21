package lifecycle

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	teamctl "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// TeamTarget is the finalizer's target for a team.
func TeamTarget(t teamctl.Desired) Target {
	return Target{Kind: string(engine.KindTeam), ID: t.ID, TeamID: t.ID, Slug: t.Slug, Namespace: t.Namespace}
}

// TeamDeleting returns the team controller's Hooks.Deleting: one finalizer pass per reconcile. The
// finalizer writes the step conditions to resource_status itself, so every outcome is NoStatus. A
// pending step requeues without an error; a failed step returns its error with phase Deleting, which
// the engine retries with its backoff without leaving Deleting.
func TeamDeleting(f *Finalizer) func(ctx context.Context, t teamctl.Desired) (engine.Outcome, error) {
	return func(ctx context.Context, t teamctl.Desired) (engine.Outcome, error) {
		res, err := f.Reconcile(ctx, TeamTarget(t))
		out := engine.Outcome{Generation: t.Generation, Phase: domain.PhaseDeleting, NoStatus: true, Requeue: res.RequeueAfter}
		if err != nil {
			return out, err
		}
		if res.Done {
			out.Phase = ""
		}
		return out, nil
	}
}

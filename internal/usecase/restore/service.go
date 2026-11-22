// Package restore implements RestoreRevision (RA-13): it puts a team's desired state back to a revision
// in one transaction, or reports the diff of doing so (dryRun) and writes nothing.
package restore

import (
	"context"
	"errors"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// TeamLock is the locked team row.
type TeamLock struct {
	State      domain.TeamState
	Version    domain.Version
	Generation int64
}

// Repository is the database side of a restore. Every method runs in the transaction it is given.
type Repository interface {
	// LockTeam locks the team row (concurrent mutations of the team wait) and returns it; ok is false when
	// the team does not exist for the caller.
	LockTeam(ctx context.Context, tx port.Tx, teamID string) (lock TeamLock, ok bool, err error)
	// HasRevision reports whether revision rev belongs to the team.
	HasRevision(ctx context.Context, tx port.Tx, teamID string, rev int64) (bool, error)
	// Target returns the restorable resources of the team as they were at revision rev: the newest
	// revision of each resource with an id up to rev, absent when that revision is a delete.
	Target(ctx context.Context, tx port.Tx, teamID string, rev int64) (domain.RestoreState, error)
	// Current returns the restorable resources of the team now.
	Current(ctx context.Context, tx port.Tx, teamID string) (domain.RestoreState, error)
	// Apply writes one step. A step the database refuses through a guard (a linked value cannot be
	// deleted, a model is no longer enabled, ...) is rolled back alone and returned as a skip reason;
	// any other failure is an error that aborts the restore.
	Apply(ctx context.Context, tx port.Tx, teamID string, step domain.RestoreStep) (skip string, err error)
	// BumpGeneration raises the team's generation by one and returns the new value.
	BumpGeneration(ctx context.Context, tx port.Tx, teamID string) (int64, error)
}

// Service is the restore use case.
type Service struct {
	acting.Deps
	Repo Repository
	// Guards is the catalog the agent guardrails read (agents.Guard). Required: a restore without it
	// fails closed instead of skipping the checks.
	Guards agents.GuardSource
}

// Result is the outcome of a restore or its dry run.
type Result struct {
	Revision int64
	DryRun   bool
	Changes  []domain.RestoreChange
	// Skipped are resources the restore left alone, each with a reason.
	Skipped []domain.RestoreChange
	Replay  *port.StoredResponse
}

var errRollback = errors.New("restore: dry run rolls back")

// Run implements RestoreRevision. expect, when not nil, is compared with the team's version (412). The
// team row stays locked until the transaction ends, so concurrent mutations of the team serialize with
// the restore. A dry run runs the same steps and rolls back, so it reports guard skips exactly.
func (s *Service) Run(ctx context.Context, c acting.Caller, teamID string, rev int64, dryRun bool, expect *domain.Version, idem *acting.Idem) (Result, error) {
	if !authz.Authorize(c.Principal, authz.RevisionsRestore, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}).Allowed() {
		return Result{}, s.Denied(ctx, c, "RestoreRevision", teamID, authz.RevisionsRestore)
	}
	if dryRun {
		idem = nil
	}
	res := Result{Revision: rev, DryRun: dryRun}
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{Revision: rev, DryRun: dryRun}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		lock, ok, err := s.Repo.LockTeam(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if !ok {
			return domain.NotFound("team")
		}
		if lock.State == domain.TeamStateDeleting || lock.State == domain.TeamStateDeleted {
			return domain.Conflict("invalid_state", "the team is being deleted", nil)
		}
		if expect != nil {
			if err := domain.CheckVersion(lock.Version, *expect); err != nil {
				return err
			}
		}
		has, err := s.Repo.HasRevision(ctx, tx, teamID, rev)
		if err != nil {
			return err
		}
		if !has {
			return domain.NotFound("revision")
		}
		target, err := s.Repo.Target(ctx, tx, teamID, rev)
		if err != nil {
			return err
		}
		current, err := s.Repo.Current(ctx, tx, teamID)
		if err != nil {
			return err
		}
		res.Changes, res.Skipped = []domain.RestoreChange{}, []domain.RestoreChange{}
		plan, err := guardSteps(ctx, tx, s.Guards, teamID, domain.PlanRestore(target, current))
		if err != nil {
			return err
		}
		for _, step := range plan {
			if step.Change.Action == domain.RestoreSkip {
				res.Skipped = append(res.Skipped, step.Change)
				continue
			}
			skip, err := s.Repo.Apply(ctx, tx, teamID, step)
			if err != nil {
				return err
			}
			if skip != "" {
				step.Change.Action, step.Change.Reason = domain.RestoreSkip, skip
				res.Skipped = append(res.Skipped, step.Change)
				continue
			}
			res.Changes = append(res.Changes, step.Change)
		}
		gen, err := s.Repo.BumpGeneration(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("RestoreRevision", "teams", teamID, teamID,
			map[string]any{"generation": lock.Generation},
			map[string]any{"revision": rev, "generation": gen, "changes": len(res.Changes), "skipped": len(res.Skipped), "dryRun": dryRun})); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventTeamUpdated, teamID, "team", teamID,
			map[string]any{"fields": []string{"restore"}, "revision": rev}); err != nil {
			return err
		}
		if dryRun {
			return errRollback
		}
		return idem.Complete(ctx, s.Idem, tx, res)
	})
	if err != nil && !errors.Is(err, errRollback) {
		return Result{}, err
	}
	return res, nil
}

package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/snapshot"
	teamrec "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Step names of the agent finalizer, in the order they run.
const (
	StepStopWorkload    = "StopWorkload"
	StepFinalSnapshot   = "FinalSnapshot"
	StepDeleteVolume    = "DeleteVolume"
	StepRetainVolume    = "RetainVolume"
	StepRevokeAgentPod  = "PodIdentity"
	StepRevokeAgentRole = "AgentRole"
	StepSnapshotClass   = "SnapshotClass"
)

// On-delete rules of an agent's volume (agents.storage_on_delete).
const (
	OnDeleteSnapshotThenDelete = "snapshot_then_delete"
	OnDeleteRetain             = "retain"
	OnDeleteDelete             = "delete"
)

// AgentWorkloads stops an agent's workload.
type AgentWorkloads interface {
	// Stop removes the Teammate and the objects it referenced; volumes are not touched.
	Stop(ctx context.Context, d agent.Desired) error
	// Stopped reports that the Teammate and every pod of the agent are gone.
	Stopped(ctx context.Context, d agent.Desired) (bool, error)
}

// RetainedVolume names what the retain rule kept.
type RetainedVolume struct {
	// Claim is the PersistentVolumeClaim and Volume the PersistentVolume behind it.
	Claim, Volume string
}

// AgentVolumes acts on an agent's volume.
type AgentVolumes interface {
	// DeleteVolume deletes the claim; a missing claim is success.
	DeleteVolume(ctx context.Context, d agent.Desired) error
	// RetainVolume makes the volume outlive its claim and the namespace (reclaim policy Retain) and says
	// which volume it is; a missing claim returns the zero value.
	RetainVolume(ctx context.Context, d agent.Desired) (RetainedVolume, error)
	// DeleteSnapshotClass removes the agent's VolumeSnapshotClass.
	DeleteSnapshotClass(ctx context.Context, d agent.Desired) error
}

// VolumeRecorder records that a volume was kept, for service admins to recover it.
type VolumeRecorder interface {
	RecordRetained(ctx context.Context, d agent.Desired, v RetainedVolume) error
}

// FinalSnapshots creates and finds the final snapshot of a delete (snapshot.Store).
type FinalSnapshots interface {
	ForAgent(ctx context.Context, agentID, reason string) ([]snapshot.Row, error)
	Create(ctx context.Context, teamID, agentID, reason string) (string, error)
}

// RoleDeleter deletes an agent's dedicated role (team.IAM).
type RoleDeleter interface {
	DeleteRole(ctx context.Context, path, name string) error
}

// AgentDeps are the ports of the agent delete steps.
type AgentDeps struct {
	Source    agent.Source
	Workloads AgentWorkloads
	Volumes   AgentVolumes
	Snapshots FinalSnapshots
	Recorder  VolumeRecorder
	Pods      agent.PodIdentities
	IAM       RoleDeleter
}

// AgentTarget is the finalizer's target for an agent.
func AgentTarget(d agent.Desired) Target {
	return Target{Kind: string(engine.KindAgent), ID: d.ID, TeamID: d.Team.ID, Namespace: d.Team.Namespace, Slug: d.Team.Slug}
}

// AgentSteps returns the ordered delete steps of an agent (RC-17): scale to zero, the on-delete rule of the
// volume (a final snapshot then delete, retain, or delete), the Pod Identity association and the dedicated
// role. The on-delete rule is read when the delete starts; every step is idempotent.
func AgentSteps(d AgentDeps) []Step {
	run := func(name string, fn func(ctx context.Context, a agent.Desired) error) Step {
		return StepFunc{StepName: name, Fn: func(ctx context.Context, t Target) error {
			a, err := d.Source.LoadAgent(ctx, t.ID)
			if err != nil {
				return err
			}
			return fn(ctx, a)
		}}
	}
	// The volume steps depend on the rule, which is fixed while the agent is deleting (agents.storage_on_delete
	// can still be edited by nobody: the row is in state deleting), so the step list is decided per pass by a
	// step that does the right thing for every rule.
	return []Step{
		run(StepStopWorkload, func(ctx context.Context, a agent.Desired) error { return stopWorkload(ctx, d, a) }),
		run(StepFinalSnapshot, func(ctx context.Context, a agent.Desired) error { return finalSnapshot(ctx, d, a) }),
		run(StepDeleteVolume, func(ctx context.Context, a agent.Desired) error { return deleteOrRetainVolume(ctx, d, a) }),
		run(StepRevokeAgentPod, func(ctx context.Context, a agent.Desired) error {
			return d.Pods.Delete(ctx, a.Team.Namespace, agent.ServiceAccountName(a.Name))
		}),
		run(StepRevokeAgentRole, func(ctx context.Context, a agent.Desired) error {
			if a.IAMIdentity != agent.IdentityDedicated {
				return nil
			}
			name, err := agent.RoleName(a.ID)
			if err != nil {
				return err
			}
			return d.IAM.DeleteRole(ctx, teamrec.RolePath(a.Team.Namespace), name)
		}),
		run(StepSnapshotClass, func(ctx context.Context, a agent.Desired) error { return d.Volumes.DeleteSnapshotClass(ctx, a) }),
	}
}

// stopWorkload scales the agent to zero by removing its workload, then waits until its pods are gone, so
// the volume is detached before it is snapshotted or deleted.
func stopWorkload(ctx context.Context, d AgentDeps, a agent.Desired) error {
	if err := d.Workloads.Stop(ctx, a); err != nil {
		return err
	}
	gone, err := d.Workloads.Stopped(ctx, a)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("the pods of %s are terminating: %w", a.Name, ErrPending)
	}
	return nil
}

// finalSnapshot waits for the final snapshot of snapshot_then_delete: it requests one when there is none,
// is pending while it completes and returns an error, after requesting another, when the last one failed.
// The other rules have no snapshot.
func finalSnapshot(ctx context.Context, d AgentDeps, a agent.Desired) error {
	if a.Volume.OnDelete != OnDeleteSnapshotThenDelete {
		return nil
	}
	rows, err := d.Snapshots.ForAgent(ctx, a.ID, snapshot.ReasonPreDelete)
	if err != nil {
		return fmt.Errorf("find final snapshot: %w", err)
	}
	failed := 0
	for _, r := range rows {
		switch r.State {
		case snapshot.StateCompleted:
			return nil
		case snapshot.StateRequested, snapshot.StatePending:
			return fmt.Errorf("the final snapshot of %s is %s: %w", a.Name, r.State, ErrPending)
		case snapshot.StateFailed:
			failed++
		}
	}
	if _, err := d.Snapshots.Create(ctx, a.Team.ID, a.ID, snapshot.ReasonPreDelete); err != nil {
		return fmt.Errorf("request final snapshot: %w", err)
	}
	if failed > 0 {
		return fmt.Errorf("the final snapshot of %s failed %d time(s); another was requested", a.Name, failed)
	}
	return fmt.Errorf("the final snapshot of %s was requested: %w", a.Name, ErrPending)
}

// deleteOrRetainVolume applies the on-delete rule to the volume: retain keeps it and records which one,
// delete and snapshot_then_delete (whose snapshot has completed by now) delete the claim.
func deleteOrRetainVolume(ctx context.Context, d AgentDeps, a agent.Desired) error {
	switch a.Volume.OnDelete {
	case OnDeleteRetain:
		v, err := d.Volumes.RetainVolume(ctx, a)
		if err != nil {
			return fmt.Errorf("retain volume: %w", err)
		}
		if v == (RetainedVolume{}) {
			return nil
		}
		return d.Recorder.RecordRetained(ctx, a, v)
	case OnDeleteDelete, OnDeleteSnapshotThenDelete:
		return d.Volumes.DeleteVolume(ctx, a)
	}
	return fmt.Errorf("unknown on-delete rule %q", a.Volume.OnDelete)
}

// NewAgentFinalizer returns the finalizer of an agent.
func NewAgentFinalizer(d AgentDeps, store ConditionStore, c Completer) *Finalizer {
	return &Finalizer{Steps: AgentSteps(d), Store: store, Completer: c}
}

// AgentDeleting returns the agent controller's Hooks.Deleting: one finalizer pass per reconcile, with the
// same outcome rules as TeamDeleting.
func AgentDeleting(f *Finalizer) func(ctx context.Context, d agent.Desired) (engine.Outcome, error) {
	return func(ctx context.Context, d agent.Desired) (engine.Outcome, error) {
		res, err := f.Reconcile(ctx, AgentTarget(d))
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

// TeamAgentsStep is the team finalizer's step for agents (Hooks.Agents): every agent of the team goes through
// its own finalizer, so each applies its on-delete rule before the team's role, secrets and namespace go. It
// is pending while any agent still has steps to run.
func TeamAgentsStep(src agent.Source, f *Finalizer) Step {
	return StepFunc{StepName: "Agents", Fn: func(ctx context.Context, t Target) error {
		list, err := src.ListTeamAgents(ctx, t.TeamID)
		if err != nil {
			return fmt.Errorf("list agents: %w", err)
		}
		pending := 0
		var errs []error
		for _, l := range list {
			res, err := f.Reconcile(ctx, Target{Kind: string(engine.KindAgent), ID: l.ID, TeamID: t.TeamID, Namespace: l.Namespace, Slug: t.Slug})
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("agent %s: %w", l.Name, err))
			case !res.Done:
				pending++
			}
		}
		if err := errors.Join(errs...); err != nil {
			return err
		}
		if pending > 0 {
			return fmt.Errorf("%d agent(s) of the team are being deleted: %w", pending, ErrPending)
		}
		return nil
	}}
}

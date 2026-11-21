package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// SecretRecoveryDays is the Secrets Manager recovery window of every secret a deleted team owned
// (RC-US-15.2): a deleted team's secrets can be restored for 30 days.
const SecretRecoveryDays = 30

// TeamSecrets finds and schedules the deletion of a team's secrets (adapter: aws/secretsmanager
// for the tag lookup, postgres for the rows).
type TeamSecrets interface {
	// ListTeamSecrets returns the ARNs of every secret the team owns.
	ListTeamSecrets(ctx context.Context, teamID string) ([]string, error)
	// ScheduleDeletion schedules arn for deletion with the recovery window. A secret already
	// scheduled or gone is success.
	ScheduleDeletion(ctx context.Context, arn string, recoveryDays int) error
}

// IAMRevoker removes the team's IAM roles (team role, dedicated agent roles, the Bedrock role).
type IAMRevoker interface {
	// RevokeTeam deletes every role tagged with the team, detaching policies first. Missing roles
	// are success.
	RevokeTeam(ctx context.Context, teamID string) error
}

// InferenceProfiles deletes the team's application inference profiles.
type InferenceProfiles interface {
	DeleteTeamProfiles(ctx context.Context, teamID string) error
}

// AccessEntries deletes the team's EKS access entries.
type AccessEntries interface {
	DeleteTeamAccessEntries(ctx context.Context, teamID string) error
}

// Namespaces deletes the team's Kubernetes namespace.
type Namespaces interface {
	// DeleteNamespace requests deletion and reports whether the namespace is already gone.
	DeleteNamespace(ctx context.Context, name string) (gone bool, err error)
}

// Hooks are the delete steps of workloads, contributed by later milestones. They run, in order,
// before any team step: agents (M2) first, then pools (M5), so a workload's own resources go
// before the team's role, profiles and namespace.
type Hooks struct {
	Agents []Step
	Pools  []Step
}

// TeamDeps are the ports of the team delete steps.
type TeamDeps struct {
	Secrets  TeamSecrets
	IAM      IAMRevoker
	Profiles InferenceProfiles
	Access   AccessEntries
	NS       Namespaces
}

// Step names of the team finalizer, in the order they run after the hooks.
const (
	StepScheduleSecretDeletion = "ScheduleSecretDeletion"
	StepRevokeIAM              = "RevokeIAM"
	StepDeleteInferenceProfile = "InferenceProfiles"
	StepDeleteAccessEntry      = "AccessEntry"
	StepDeleteNamespace        = "Namespace"
)

// TeamSteps returns the full ordered step list of a team delete: agent hooks, pool hooks, secrets
// scheduled for deletion (before the namespace goes), IAM, inference profiles, access entries and
// the namespace last.
func TeamSteps(d TeamDeps, h Hooks) []Step {
	steps := make([]Step, 0, len(h.Agents)+len(h.Pools)+5)
	steps = append(steps, h.Agents...)
	steps = append(steps, h.Pools...)
	steps = append(steps,
		StepFunc{StepScheduleSecretDeletion, func(ctx context.Context, t Target) error { return scheduleSecrets(ctx, d.Secrets, t) }},
		StepFunc{StepRevokeIAM, func(ctx context.Context, t Target) error { return d.IAM.RevokeTeam(ctx, t.TeamID) }},
		StepFunc{StepDeleteInferenceProfile, func(ctx context.Context, t Target) error { return d.Profiles.DeleteTeamProfiles(ctx, t.TeamID) }},
		StepFunc{StepDeleteAccessEntry, func(ctx context.Context, t Target) error { return d.Access.DeleteTeamAccessEntries(ctx, t.TeamID) }},
		StepFunc{StepDeleteNamespace, func(ctx context.Context, t Target) error { return deleteNamespace(ctx, d.NS, t) }},
	)
	return steps
}

// NewTeamFinalizer returns the finalizer of a team.
func NewTeamFinalizer(d TeamDeps, h Hooks, store ConditionStore, c Completer) *Finalizer {
	return &Finalizer{Steps: TeamSteps(d, h), Store: store, Completer: c}
}

func scheduleSecrets(ctx context.Context, s TeamSecrets, t Target) error {
	arns, err := s.ListTeamSecrets(ctx, t.TeamID)
	if err != nil {
		return fmt.Errorf("list team secrets: %w", err)
	}
	sort.Strings(arns)
	var errs []error
	for _, arn := range arns {
		// Every secret is attempted even when one fails, so one bad secret does not hold the
		// others; the step fails (and is retried) until all are scheduled.
		if err := s.ScheduleDeletion(ctx, arn, SecretRecoveryDays); err != nil {
			errs = append(errs, fmt.Errorf("schedule %s: %w", arn, err))
		}
	}
	return errors.Join(errs...)
}

func deleteNamespace(ctx context.Context, n Namespaces, t Target) error {
	if t.Namespace == "" {
		return fmt.Errorf("namespace unknown for team %s", t.TeamID)
	}
	gone, err := n.DeleteNamespace(ctx, t.Namespace)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("namespace %s is terminating: %w", t.Namespace, ErrPending)
	}
	return nil
}

// UnionSecrets lists the secrets of several sources (the database rows and the tag lookup) without
// duplicates, and schedules through the first source.
type UnionSecrets struct {
	Sources []interface {
		ListTeamSecrets(ctx context.Context, teamID string) ([]string, error)
	}
	Scheduler interface {
		ScheduleDeletion(ctx context.Context, arn string, recoveryDays int) error
	}
}

// ListTeamSecrets implements TeamSecrets.
func (u UnionSecrets) ListTeamSecrets(ctx context.Context, teamID string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, s := range u.Sources {
		arns, err := s.ListTeamSecrets(ctx, teamID)
		if err != nil {
			return nil, err
		}
		for _, a := range arns {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	return out, nil
}

// ScheduleDeletion implements TeamSecrets.
func (u UnionSecrets) ScheduleDeletion(ctx context.Context, arn string, days int) error {
	return u.Scheduler.ScheduleDeletion(ctx, arn, days)
}

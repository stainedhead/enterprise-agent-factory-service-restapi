package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Target identifies the resource being finalized.
type Target struct {
	// Kind is the resource kind of resource_status ("team", "agent", "pool").
	Kind string
	// ID is the resource id (team_…, agt_…, pool_…).
	ID string
	// TeamID is the owning team; for a team it equals ID.
	TeamID string
	// Slug is the team slug, when known.
	Slug string
	// Namespace is the Kubernetes namespace of the team (afs-t-<slug>).
	Namespace string
}

// Step is one named, idempotent delete step. Run returns nil when the step's effect is in place,
// ErrPending while it is still converging (for example a terminating namespace) and any other error
// when it failed; the finalizer retries both on the next pass.
type Step interface {
	// Name is a PascalCase word; the finalizer reports the step as condition "Delete"+Name.
	Name() string
	Run(ctx context.Context, t Target) error
}

// StepFunc adapts a function to Step.
type StepFunc struct {
	StepName string
	Fn       func(ctx context.Context, t Target) error
}

// Name implements Step.
func (s StepFunc) Name() string { return s.StepName }

// Run implements Step.
func (s StepFunc) Run(ctx context.Context, t Target) error { return s.Fn(ctx, t) }

// ErrPending is returned by a step that has started its work and is waiting for it to complete.
var ErrPending = errors.New("lifecycle: step pending")

// CodedError is implemented by errors that carry an AWS or Kubernetes error code. The finalizer
// copies the code into the condition's reason.
type CodedError interface {
	error
	Code() string
}

// ConditionStore is the part of the engine's status writer the finalizer needs (M1-F5 owns the
// implementation: resource_status upsert).
type ConditionStore interface {
	// Conditions returns the stored conditions of a resource; none yet is not an error.
	Conditions(ctx context.Context, kind, id string) ([]domain.Condition, error)
	// Write stores phase and conditions. Phase is always Deleting while the finalizer runs.
	Write(ctx context.Context, t Target, phase domain.Phase, conds []domain.Condition) error
}

// Completer performs the last step: the row becomes `deleted` with audit and the outbox event, in
// one transaction (usage and audit stay readable).
type Completer interface {
	Complete(ctx context.Context, t Target) error
}

// Result tells the engine what to do next.
type Result struct {
	// Done is true when the resource reached `deleted`.
	Done bool
	// RequeueAfter, when nonzero, asks for another pass after the delay (a pending step).
	RequeueAfter time.Duration
}

// PendingRequeue is how soon a pending step is checked again.
const PendingRequeue = 5 * time.Second

// Finalizer runs Steps in order, shows each as a condition and completes the delete.
type Finalizer struct {
	Steps     []Step
	Store     ConditionStore
	Completer Completer
	Now       func() time.Time
}

// CondType is the condition type that reports step name.
func CondType(name string) string { return "Delete" + name }

// Reconcile runs one pass. A step that fails or is pending stops the pass (later steps depend on
// earlier ones); its condition is False with the error code as reason and the engine retries with
// its own backoff (the returned error), except for ErrPending which requeues without an error.
// Steps whose condition is already True are skipped.
func (f *Finalizer) Reconcile(ctx context.Context, t Target) (Result, error) {
	now := f.now()
	prev, err := f.Store.Conditions(ctx, t.Kind, t.ID)
	if err != nil {
		return Result{}, fmt.Errorf("lifecycle: read conditions: %w", err)
	}
	conds := declared(f.Steps, prev, now)
	for i, s := range f.Steps {
		if conds[i].Status == domain.ConditionTrue {
			continue
		}
		stepErr := s.Run(ctx, t)
		conds[i] = next(conds[i], stepErr, f.now())
		if err := f.Store.Write(ctx, t, domain.PhaseDeleting, conds); err != nil {
			return Result{}, fmt.Errorf("lifecycle: write conditions: %w", err)
		}
		switch {
		case stepErr == nil:
		case errors.Is(stepErr, ErrPending):
			return Result{RequeueAfter: PendingRequeue}, nil
		default:
			return Result{}, fmt.Errorf("lifecycle: step %s: %w", s.Name(), stepErr)
		}
	}
	if err := f.Store.Write(ctx, t, domain.PhaseDeleting, conds); err != nil {
		return Result{}, fmt.Errorf("lifecycle: write conditions: %w", err)
	}
	if err := f.Completer.Complete(ctx, t); err != nil {
		return Result{}, fmt.Errorf("lifecycle: complete delete: %w", err)
	}
	return Result{Done: true}, nil
}

func (f *Finalizer) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// declared returns one condition per step, in step order, keeping what a previous pass stored and
// Unknown for steps not yet attempted, so a client sees the whole plan while Deleting.
func declared(steps []Step, prev []domain.Condition, now time.Time) []domain.Condition {
	out := make([]domain.Condition, len(steps))
	for i, s := range steps {
		out[i] = domain.Condition{Type: CondType(s.Name()), Status: domain.ConditionUnknown, Reason: "Pending", LastTransitionTime: now.UTC()}
		for _, c := range prev {
			if c.Type == out[i].Type {
				out[i] = c
			}
		}
	}
	return out
}

func next(c domain.Condition, err error, now time.Time) domain.Condition {
	n := domain.Condition{Type: c.Type, LastTransitionTime: c.LastTransitionTime}
	switch {
	case err == nil:
		n.Status, n.Reason = domain.ConditionTrue, "Done"
	case errors.Is(err, ErrPending):
		n.Status, n.Reason, n.Message = domain.ConditionFalse, "InProgress", err.Error()
	default:
		n.Status, n.Reason, n.Message = domain.ConditionFalse, reason(err), err.Error()
	}
	if c.Status != n.Status || c.Reason != n.Reason {
		n.LastTransitionTime = now.UTC()
	}
	return n
}

func reason(err error) string {
	var ce CodedError
	if errors.As(err, &ce) && ce.Code() != "" {
		return ce.Code()
	}
	return "Error"
}

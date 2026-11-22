package engine

import (
	"context"
	"slices"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// DriftConditionTTL is how long DriftCorrected stays on a resource after a correction, across later
// reconciles that find nothing to correct.
const DriftConditionTTL = 10 * time.Minute

// Report is what the engine writes to resource_status after a reconcile.
type Report struct {
	Ref
	// Generation, when set, becomes observed_generation. Nil keeps the previous value (a failed
	// reconcile applied nothing).
	Generation *int64
	Phase      domain.Phase
	Conditions []domain.Condition
	// Replace makes Conditions the complete list; otherwise they are merged into the stored ones by
	// type (a failure only touches the failing condition).
	Replace bool
	// DriftDetectedAt, when set, becomes drift_detected_at.
	DriftDetectedAt *time.Time
	ReconciledAt    time.Time
}

// Stored is a resource_status row.
type Stored struct {
	Phase              domain.Phase
	ObservedGeneration int64
	Conditions         []domain.Condition
	DriftDetectedAt    *time.Time
	LastReconciledAt   *time.Time
}

// StatusWriter writes reports. The adapter runs MergeStatus in one transaction with the upsert and,
// when the phase changed, the <kind>.status_changed outbox event.
type StatusWriter interface {
	Apply(ctx context.Context, r Report) error
}

// MergeStatus computes the row that replaces prev (nil for a resource with no row yet). changed
// reports whether the phase differs from prev's (an absent row counts as Pending), which is when the
// status_changed event is due.
func MergeStatus(prev *Stored, r Report) (next Stored, previous domain.Phase, changed bool) {
	previous = domain.PhasePending
	var old []domain.Condition
	if prev != nil {
		next = *prev
		previous = prev.Phase
		old = prev.Conditions
	}
	if !r.Phase.Valid() {
		r.Phase = domain.PhaseReady
	}
	next.Phase = r.Phase
	if r.Generation != nil {
		next.ObservedGeneration = *r.Generation
	}
	if r.DriftDetectedAt != nil {
		t := *r.DriftDetectedAt
		next.DriftDetectedAt = &t
	}
	rt := r.ReconciledAt
	next.LastReconciledAt = &rt

	var merged []domain.Condition
	if r.Replace {
		merged = slices.Clone(r.Conditions)
		// DriftCorrected outlives reconciles that correct nothing, for a while.
		if _, has := findCondition(merged, domain.CondDriftCorrected); !has {
			if c, ok := findCondition(old, domain.CondDriftCorrected); ok && c.Status == domain.ConditionTrue &&
				r.ReconciledAt.Sub(c.LastTransitionTime) < DriftConditionTTL {
				merged = append(merged, c)
			}
		}
		// BudgetExhausted belongs to the budget reconciler (M3-F5), which sets and clears it itself; a
		// controller replacing its own conditions must not wipe it.
		if _, has := findCondition(merged, domain.CondBudgetExhausted); !has {
			if c, ok := findCondition(old, domain.CondBudgetExhausted); ok {
				merged = append(merged, c)
			}
		}
	} else {
		merged = slices.Clone(old)
		for _, c := range r.Conditions {
			if i := slices.IndexFunc(merged, func(x domain.Condition) bool { return x.Type == c.Type }); i >= 0 {
				merged[i] = c
			} else {
				merged = append(merged, c)
			}
		}
	}
	for i, c := range merged {
		if o, ok := findCondition(old, c.Type); ok && o.Status == c.Status {
			merged[i].LastTransitionTime = o.LastTransitionTime
		} else if c.LastTransitionTime.IsZero() {
			merged[i].LastTransitionTime = r.ReconciledAt
		}
	}
	slices.SortStableFunc(merged, func(a, b domain.Condition) int {
		switch {
		case a.Type < b.Type:
			return -1
		case a.Type > b.Type:
			return 1
		}
		return 0
	})
	if merged == nil {
		merged = []domain.Condition{}
	}
	next.Conditions = merged
	return next, previous, previous != next.Phase
}

func findCondition(cs []domain.Condition, typ string) (domain.Condition, bool) {
	for _, c := range cs {
		if c.Type == typ {
			return c, true
		}
	}
	return domain.Condition{}, false
}

// StatusEvent returns the event type announcing a phase change of a resource of kind k, if the
// contract has one (API contract §7).
func StatusEvent(k Kind) (domain.EventType, bool) {
	switch k {
	case KindTeam:
		return domain.EventTeamStatusChanged, true
	case KindAgent:
		return domain.EventAgentStatusChanged, true
	case KindPool:
		return domain.EventPoolStatusChanged, true
	}
	return "", false
}

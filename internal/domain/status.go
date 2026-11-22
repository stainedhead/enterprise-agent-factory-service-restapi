package domain

import "time"

// Phase is the reconcile phase of a desired-state resource (API contract §1.1). These are the only
// status words (root PRD D24).
type Phase string

// Phases.
const (
	PhasePending     Phase = "Pending"
	PhaseReconciling Phase = "Reconciling"
	PhaseReady       Phase = "Ready"
	PhaseDegraded    Phase = "Degraded"
	PhaseFailed      Phase = "Failed"
	PhaseDeleting    Phase = "Deleting"
)

// Phases lists every phase.
func Phases() []Phase {
	return []Phase{PhasePending, PhaseReconciling, PhaseReady, PhaseDegraded, PhaseFailed, PhaseDeleting}
}

// Valid reports whether p is a known phase.
func (p Phase) Valid() bool {
	for _, q := range Phases() {
		if p == q {
			return true
		}
	}
	return false
}

// Terminal reports whether a client polling for completion can stop at p.
func (p Phase) Terminal() bool { return p == PhaseReady || p == PhaseDegraded || p == PhaseFailed }

// ConditionStatus is True, False or Unknown.
type ConditionStatus string

// Condition statuses.
const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// Condition types of API contract §1.1, plus DeliveryHealthy on notification channels.
const (
	CondNamespaceReady    = "NamespaceReady"
	CondIamReady          = "IamReady"
	CondSecretsSynced     = "SecretsSynced"
	CondVolumeReady       = "VolumeReady"
	CondEgressPolicyReady = "EgressPolicyReady"
	CondTemplateRetired   = "TemplateRetired"
	// CondCollaborationKeyMissing is True on a connected worker that has no Paperclip agent key yet (RC-22).
	CondCollaborationKeyMissing = "CollaborationKeyMissing"
	CondDriftCorrected          = "DriftCorrected"
	CondBudgetExhausted         = "BudgetExhausted"
	CondDeliveryHealthy         = "DeliveryHealthy"
)

// Condition is one observed condition of a resource.
type Condition struct {
	Type               string          `json:"type"`
	Status             ConditionStatus `json:"status"`
	Reason             string          `json:"reason,omitempty"`
	Message            string          `json:"message,omitempty"`
	LastTransitionTime time.Time       `json:"lastTransitionTime"`
}

// Status is the status object every desired-state resource carries.
type Status struct {
	Phase              Phase       `json:"phase"`
	ObservedGeneration int64       `json:"observedGeneration"`
	Conditions         []Condition `json:"conditions"`
	UpdatedAt          time.Time   `json:"updatedAt"`
}

// PendingStatus is the status of a resource the reconciler has not observed yet.
func PendingStatus(now time.Time) Status {
	return Status{Phase: PhasePending, Conditions: []Condition{}, UpdatedAt: now.UTC()}
}

// Condition returns the condition of type t, if present.
func (s Status) Condition(t string) (Condition, bool) {
	for _, c := range s.Conditions {
		if c.Type == t {
			return c, true
		}
	}
	return Condition{}, false
}

// BudgetBlocked reports whether the resource is Ready with BudgetExhausted=True (shown as Budget blocked).
func (s Status) BudgetBlocked() bool {
	c, ok := s.Condition(CondBudgetExhausted)
	return s.Phase == PhaseReady && ok && c.Status == ConditionTrue
}

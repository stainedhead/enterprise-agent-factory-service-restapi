package engine

import (
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func cond(typ string, st domain.ConditionStatus, at time.Time) domain.Condition {
	return domain.Condition{Type: typ, Status: st, LastTransitionTime: at}
}

func i64(v int64) *int64 { return &v }

func TestMergeStatus(t *testing.T) {
	t.Run("first report is a phase change from Pending", func(t *testing.T) {
		next, prev, changed := MergeStatus(nil, Report{Phase: domain.PhaseReady, Generation: i64(4), ReconciledAt: t0,
			Conditions: []domain.Condition{{Type: domain.CondNamespaceReady, Status: domain.ConditionTrue}}, Replace: true})
		if !changed || prev != domain.PhasePending || next.Phase != domain.PhaseReady || next.ObservedGeneration != 4 {
			t.Errorf("next = %+v prev = %v changed = %v", next, prev, changed)
		}
		if got := next.Conditions[0].LastTransitionTime; !got.Equal(t0) {
			t.Errorf("transition time = %v", got)
		}
	})
	t.Run("an unchanged condition keeps its transition time", func(t *testing.T) {
		old := &Stored{Phase: domain.PhaseReady, ObservedGeneration: 1, Conditions: []domain.Condition{cond(domain.CondNamespaceReady, domain.ConditionTrue, t0.Add(-time.Hour))}}
		next, _, changed := MergeStatus(old, Report{Phase: domain.PhaseReady, Generation: i64(2), ReconciledAt: t0, Replace: true,
			Conditions: []domain.Condition{{Type: domain.CondNamespaceReady, Status: domain.ConditionTrue}}})
		if changed {
			t.Error("same phase reported as changed")
		}
		if !next.Conditions[0].LastTransitionTime.Equal(t0.Add(-time.Hour)) || next.ObservedGeneration != 2 {
			t.Errorf("next = %+v", next)
		}
	})
	t.Run("a flipped condition gets a new transition time", func(t *testing.T) {
		old := &Stored{Phase: domain.PhaseReady, Conditions: []domain.Condition{cond(domain.CondIamReady, domain.ConditionTrue, t0.Add(-time.Hour))}}
		next, prev, changed := MergeStatus(old, Report{Phase: domain.PhaseDegraded, ReconciledAt: t0,
			Conditions: []domain.Condition{{Type: domain.CondIamReady, Status: domain.ConditionFalse, Reason: "AccessDenied"}}})
		if !changed || prev != domain.PhaseReady {
			t.Errorf("changed=%v prev=%v", changed, prev)
		}
		if c := next.Conditions[0]; !c.LastTransitionTime.Equal(t0) || c.Reason != "AccessDenied" {
			t.Errorf("condition = %+v", c)
		}
	})
	t.Run("a failure merges and keeps the observed generation", func(t *testing.T) {
		old := &Stored{Phase: domain.PhaseReady, ObservedGeneration: 7, Conditions: []domain.Condition{
			cond(domain.CondNamespaceReady, domain.ConditionTrue, t0.Add(-time.Hour)), cond(domain.CondIamReady, domain.ConditionTrue, t0.Add(-time.Hour))}}
		next, _, _ := MergeStatus(old, Report{Phase: domain.PhaseDegraded, ReconciledAt: t0,
			Conditions: []domain.Condition{{Type: domain.CondIamReady, Status: domain.ConditionFalse}, {Type: "Ready", Status: domain.ConditionFalse}}})
		if next.ObservedGeneration != 7 || len(next.Conditions) != 3 {
			t.Fatalf("next = %+v", next)
		}
		if next.Conditions[0].Type != domain.CondIamReady || next.Conditions[1].Type != domain.CondNamespaceReady || next.Conditions[2].Type != "Ready" {
			t.Errorf("conditions not sorted by type: %+v", next.Conditions)
		}
		if next.Conditions[1].Status != domain.ConditionTrue {
			t.Error("untouched condition changed")
		}
	})
	t.Run("an invalid phase becomes Ready", func(t *testing.T) {
		next, _, _ := MergeStatus(nil, Report{ReconciledAt: t0})
		if next.Phase != domain.PhaseReady || next.Conditions == nil {
			t.Errorf("next = %+v", next)
		}
	})
	t.Run("drift detected time", func(t *testing.T) {
		at := t0.Add(time.Minute)
		next, _, _ := MergeStatus(nil, Report{Phase: domain.PhaseReady, ReconciledAt: t0, DriftDetectedAt: &at, Replace: true})
		if next.DriftDetectedAt == nil || !next.DriftDetectedAt.Equal(at) || next.LastReconciledAt == nil || !next.LastReconciledAt.Equal(t0) {
			t.Errorf("next = %+v", next)
		}
	})
}

func TestDriftConditionOutlivesQuietReconciles(t *testing.T) {
	old := &Stored{Phase: domain.PhaseReady, Conditions: []domain.Condition{cond(domain.CondDriftCorrected, domain.ConditionTrue, t0)}}
	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"just after", t0.Add(time.Minute), true},
		{"after the TTL", t0.Add(DriftConditionTTL), false},
	}
	for _, tt := range tests {
		next, _, _ := MergeStatus(old, Report{Phase: domain.PhaseReady, ReconciledAt: tt.now, Replace: true})
		_, has := findCondition(next.Conditions, domain.CondDriftCorrected)
		if has != tt.want {
			t.Errorf("%s: DriftCorrected present = %v, want %v", tt.name, has, tt.want)
		}
	}
	// a fresh correction replaces it
	next, _, _ := MergeStatus(old, Report{Phase: domain.PhaseReady, ReconciledAt: t0.Add(time.Minute), Replace: true,
		Conditions: []domain.Condition{driftCondition([]string{"X/y"}, t0.Add(time.Minute))}})
	if c, _ := findCondition(next.Conditions, domain.CondDriftCorrected); c.Message != "reverted: X/y" {
		t.Errorf("condition = %+v", c)
	}
}

func TestStatusEvent(t *testing.T) {
	for kind, want := range map[Kind]domain.EventType{KindTeam: domain.EventTeamStatusChanged, KindAgent: domain.EventAgentStatusChanged, KindPool: domain.EventPoolStatusChanged} {
		if got, ok := StatusEvent(kind); !ok || got != want {
			t.Errorf("StatusEvent(%s) = %v, %v", kind, got, ok)
		}
	}
	if _, ok := StatusEvent(KindAdmission); ok {
		t.Error("admission has no status event")
	}
}

func TestObjectNameAndDrift(t *testing.T) {
	if got := ObjectName("Namespace", "", "afs-t-a"); got != "Namespace/afs-t-a" {
		t.Error(got)
	}
	if got := ObjectName("NetworkPolicy", "ns", "p"); got != "NetworkPolicy/ns/p" {
		t.Error(got)
	}
	noted := map[string]time.Time{"A/x": t0, "B/y": t0, "C/z": t0}
	got := correctedDrift(noted, []string{"C/z", "A/x", "D/w"})
	if len(got) != 2 || got[0] != "A/x" || got[1] != "C/z" {
		t.Errorf("correctedDrift = %v", got)
	}
	k := Key{KindAgent, "agt_1"}
	if k.String() != "agent/agt_1" {
		t.Error(k.String())
	}
}

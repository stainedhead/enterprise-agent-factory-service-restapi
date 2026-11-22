package engine

import (
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// M3-F5: a controller replacing its conditions keeps BudgetExhausted, which the budget reconciler owns.
func TestReplaceKeepsBudgetExhausted(t *testing.T) {
	t0 := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	prev := &Stored{Phase: domain.PhaseReady, Conditions: []domain.Condition{
		{Type: domain.CondBudgetExhausted, Status: domain.ConditionTrue, Reason: "BudgetExhausted", LastTransitionTime: t0},
		{Type: "NamespaceReady", Status: domain.ConditionTrue, LastTransitionTime: t0},
	}}
	next, _, _ := MergeStatus(prev, Report{Phase: domain.PhaseReady, Replace: true, ReconciledAt: t0.Add(time.Hour),
		Conditions: []domain.Condition{{Type: "IamReady", Status: domain.ConditionTrue}}})
	got := map[string]domain.Condition{}
	for _, c := range next.Conditions {
		got[c.Type] = c
	}
	if len(got) != 2 || got["IamReady"].Type == "" {
		t.Fatalf("conditions = %+v", next.Conditions)
	}
	b, ok := got[domain.CondBudgetExhausted]
	if !ok || b.Status != domain.ConditionTrue || !b.LastTransitionTime.Equal(t0) {
		t.Fatalf("BudgetExhausted = %+v %v", b, ok)
	}
	if _, kept := got["NamespaceReady"]; kept {
		t.Error("a condition the controller no longer reports was kept")
	}

	// A controller that reports it (it does not today) wins; a resource without it gains none.
	next, _, _ = MergeStatus(prev, Report{Phase: domain.PhaseReady, Replace: true, ReconciledAt: t0,
		Conditions: []domain.Condition{{Type: domain.CondBudgetExhausted, Status: domain.ConditionFalse}}})
	if len(next.Conditions) != 1 || next.Conditions[0].Status != domain.ConditionFalse {
		t.Fatalf("conditions = %+v", next.Conditions)
	}
	next, _, _ = MergeStatus(nil, Report{Phase: domain.PhaseReady, Replace: true, ReconciledAt: t0})
	if len(next.Conditions) != 0 {
		t.Fatalf("conditions = %+v", next.Conditions)
	}
}

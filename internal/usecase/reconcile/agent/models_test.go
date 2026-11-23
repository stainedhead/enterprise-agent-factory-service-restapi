package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

type evRec struct {
	got []map[string]any
	err error
}

func (e *evRec) Publish(_ context.Context, typ domain.EventType, team, kind, id, agentID string, data map[string]any) error {
	if typ != domain.EventAgentModelUnavailable || team != "team_1" {
		return errors.New("unexpected event")
	}
	data["_kind"], data["_id"], data["_agent"] = kind, id, agentID
	e.got = append(e.got, data)
	return e.err
}

func yes() *bool { v := true; return &v }
func no() *bool  { v := false; return &v }

// MOD-6: ModelUnavailable is True with the reason while the default is outside the effective set, and the
// event is published once per transition (a first sighting counts), never while it stays True.
func TestModelCondition(t *testing.T) {
	ok := ModelState{TeamID: "team_1", Kind: "agents", ID: "agt_1", Default: "m1", Effective: []string{"m1", "m2"}}
	if c := ModelCondition(ok); c.Status != domain.ConditionFalse || c.Type != CondModelUnavailable {
		t.Errorf("available: %+v", c)
	}
	gone := ModelState{TeamID: "team_1", Kind: "agents", ID: "agt_1", Default: "m1", Effective: []string{"m2"}, Reason: domain.ReasonModelNotEnabledForTeam}
	c := ModelCondition(gone)
	if c.Status != domain.ConditionTrue || c.Reason != domain.ReasonModelNotEnabledForTeam || c.Message == "" {
		t.Errorf("unavailable: %+v", c)
	}
	empty := ModelState{Default: "m1"}
	if c := ModelCondition(empty); c.Status != domain.ConditionTrue || c.Reason != domain.ReasonModelNotEnabledForAgent {
		t.Errorf("empty set: %+v", c)
	}
}

func TestReportModelOncePerTransition(t *testing.T) {
	base := ModelState{TeamID: "team_1", Kind: "agents", ID: "agt_1", Default: "m1", Effective: []string{"m2"}, Reason: domain.ReasonModelNotPriced}
	for name, tc := range map[string]struct {
		prev *bool
		want int
	}{"first sighting": {nil, 1}, "was available": {no(), 1}, "still unavailable": {yes(), 0}} {
		t.Run(name, func(t *testing.T) {
			ev := &evRec{}
			s := base
			s.Prev = tc.prev
			if c := ReportModel(t.Context(), ev, nil, s); c.Status != domain.ConditionTrue || len(ev.got) != tc.want {
				t.Fatalf("cond %+v events %d", c, len(ev.got))
			}
			if tc.want == 1 {
				d := ev.got[0]
				if d["agentId"] != "agt_1" || d["defaultModelId"] != "m1" || d["reason"] != domain.ReasonModelNotPriced || d["_kind"] != "agents" || d["_agent"] != "agt_1" {
					t.Errorf("data %v", d)
				}
			}
		})
	}
	t.Run("available publishes nothing", func(t *testing.T) {
		ev := &evRec{}
		ReportModel(t.Context(), ev, nil, ModelState{TeamID: "team_1", Default: "m1", Effective: []string{"m1"}})
		if len(ev.got) != 0 {
			t.Error("published")
		}
	})
	t.Run("a pool", func(t *testing.T) {
		ev := &evRec{}
		s := base
		s.Kind, s.ID = "pools", "pool_1"
		ReportModel(t.Context(), ev, nil, s)
		if len(ev.got) != 1 || ev.got[0]["poolId"] != "pool_1" || ev.got[0]["_agent"] != "" {
			t.Errorf("data %v", ev.got)
		}
	})
	t.Run("a failed publish still returns the condition", func(t *testing.T) {
		ev := &evRec{err: errors.New("down")}
		if c := ReportModel(t.Context(), ev, nil, base); c.Status != domain.ConditionTrue {
			t.Error("condition")
		}
	})
	t.Run("no sink", func(t *testing.T) {
		if c := ReportModel(t.Context(), nil, nil, base); c.Status != domain.ConditionTrue {
			t.Error("condition")
		}
	})
}

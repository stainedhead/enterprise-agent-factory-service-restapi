package main

import (
	"context"
	"slices"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/links"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
)

func TestRoutesRouteAgentTables(t *testing.T) {
	reg, err := routes()
	if err != nil {
		t.Fatal(err)
	}
	a1, a2, other := engine.NewRef(engine.KindAgent, "agt_1", "team_A"), engine.NewRef(engine.KindAgent, "agt_2", "team_A"), engine.NewRef(engine.KindAgent, "agt_3", "team_B")
	fan := func(_ context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
		if !slices.Contains(kinds, engine.KindAgent) {
			return nil, nil
		}
		return []engine.Ref{a1, a2, other}, nil
	}
	tests := []struct {
		name   string
		change engine.Change
		want   []engine.Ref
	}{
		{"an agent row is the agent", engine.Change{Table: "agents", TeamID: "team_A", ID: "agt_1"}, []engine.Ref{a1}},
		{"bindings reconcile every agent of the team only", engine.Change{Table: "agent_secret_bindings", TeamID: "team_A"}, []engine.Ref{a1, a2}},
		{"a team's tags reconcile its agents", engine.Change{Table: "team_tags", TeamID: "team_B"}, []engine.Ref{other}},
		{"a service tag reconciles every agent", engine.Change{Table: "service_tags"}, []engine.Ref{a1, a2, other}},
		{"a template change reconciles every agent", engine.Change{Table: "agent_templates", ID: "tpl_1"}, []engine.Ref{a1, a2, other}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reg.Resolve(t.Context(), tc.change, fan)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for _, w := range tc.want {
				if !slices.Contains(got, w) {
					t.Errorf("missing %v in %v", w, got)
				}
			}
		})
	}
	// The team routes are kept: a team row is still the team.
	got, _ := reg.Resolve(t.Context(), engine.Change{Table: "teams", TeamID: "team_A", ID: "team_A"}, fan)
	if !slices.Contains(got, engine.NewRef(engine.KindTeam, "team_A", "team_A")) {
		t.Errorf("teams row lost its team target: %v", got)
	}
}

func TestRoutesMergeEveryRouteList(t *testing.T) {
	// The agent controller, egress (extra) and the base all route "agents" and "teams"; the registry accepts
	// them only once merged.
	reg, err := routes(engine.Route{Table: "agents", Targets: []engine.Target{{Kind: "egress"}}})
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	got, _ := reg.Resolve(t.Context(), engine.Change{Table: "agents", TeamID: "team_A", ID: "agt_1"}, func(context.Context, []engine.Kind) ([]engine.Ref, error) { return nil, nil })
	if !slices.Contains(got, engine.NewRef(engine.KindAgent, "agt_1", "team_A")) {
		t.Errorf("agent target lost: %v", got)
	}
}

// M4 integration: the tags routes (team_tags to the team, service_tags fanned out to teams) and the agent
// routes (the same tables to agents) must both survive the single mergeRoutes.
func TestRoutesCombineTagsAndAgentRoutes(t *testing.T) {
	reg, err := routes(append(links.Routes(), tags.Routes()...)...)
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	team := engine.NewRef(engine.KindTeam, "team_A", "team_A")
	a1 := engine.NewRef(engine.KindAgent, "agt_1", "team_A")
	fan := func(_ context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
		var out []engine.Ref
		if slices.Contains(kinds, engine.KindTeam) {
			out = append(out, team)
		}
		if slices.Contains(kinds, engine.KindAgent) {
			out = append(out, a1)
		}
		return out, nil
	}
	for _, ch := range []engine.Change{
		{Table: "team_tags", TeamID: "team_A"},
		{Table: "service_tags"},
	} {
		got, err := reg.Resolve(t.Context(), ch, fan)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(got, team) || !slices.Contains(got, a1) {
			t.Errorf("%s: want team and agent targets, got %v", ch.Table, got)
		}
	}
}

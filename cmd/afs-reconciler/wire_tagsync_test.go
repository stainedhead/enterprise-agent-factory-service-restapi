package main

import (
	"context"
	"slices"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tagsync"
)

// M6-X2: the tag sync routes share the tables the team, agent and pool controllers route; all targets must
// survive the single mergeRoutes.
func TestRoutesCombineTagSyncWithOthers(t *testing.T) {
	reg, err := routes(append(append(tagsync.Routes(), tags.Routes()...), pool.Routes()...)...)
	if err != nil {
		t.Fatalf("routes: %v", err)
	}
	sync := engine.NewRef(tagsync.Kind, "team_A", "team_A")
	team := engine.NewRef(engine.KindTeam, "team_A", "team_A")
	fan := func(_ context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
		var out []engine.Ref
		if slices.Contains(kinds, tagsync.Kind) {
			out = append(out, sync)
		}
		if slices.Contains(kinds, engine.KindTeam) {
			out = append(out, team)
		}
		return out, nil
	}
	for _, ch := range []engine.Change{
		{Table: "team_tags", TeamID: "team_A"}, {Table: "secrets", TeamID: "team_A", ID: "sec_1"},
		{Table: "agent_pools", TeamID: "team_A", ID: "pool_1"}, {Table: "agents", TeamID: "team_A", ID: "agt_1"}, {Table: "service_tags"},
	} {
		got, err := reg.Resolve(t.Context(), ch, fan)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(got, sync) {
			t.Errorf("%s: tag sync not reached: %v", ch.Table, got)
		}
	}
	// The team's own routes survive the merge.
	if got, _ := reg.Resolve(t.Context(), engine.Change{Table: "team_tags", TeamID: "team_A"}, fan); !slices.Contains(got, team) {
		t.Errorf("team target lost: %v", got)
	}
}

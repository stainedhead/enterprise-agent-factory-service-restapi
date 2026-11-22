package main

import (
	"context"
	"slices"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

// M5-C: the pool routes merge with the team and agent routes (one registry entry per table) and route a pool's
// tables to the pool, never to an agent of the same team only.
func TestRoutesRoutePoolTables(t *testing.T) {
	reg, err := routes(pool.Routes()...)
	if err != nil {
		t.Fatal(err)
	}
	p1, p2, other := engine.NewRef(engine.KindPool, "pool_1", "team_A"), engine.NewRef(engine.KindPool, "pool_2", "team_A"), engine.NewRef(engine.KindPool, "pool_3", "team_B")
	fan := func(_ context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
		if !slices.Contains(kinds, engine.KindPool) {
			return nil, nil
		}
		return []engine.Ref{p1, p2, other}, nil
	}
	tests := []struct {
		name   string
		change engine.Change
		want   []engine.Ref
	}{
		{"a pool row is the pool", engine.Change{Table: "agent_pools", TeamID: "team_A", ID: "pool_1"}, []engine.Ref{p1}},
		{"a handoff reconciles the team's pools only", engine.Change{Table: "pool_handoffs", TeamID: "team_A"}, []engine.Ref{p1, p2}},
		{"a pool role link reconciles the team's pools", engine.Change{Table: "pool_role_links", TeamID: "team_B"}, []engine.Ref{other}},
		{"a template change reconciles every pool", engine.Change{Table: "agent_templates", ID: "tpl_1"}, []engine.Ref{p1, p2, other}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reg.Resolve(t.Context(), tc.change, fan)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !slices.Contains(got, w) {
					t.Errorf("missing %v in %v", w, got)
				}
			}
			for _, g := range got {
				if g.Kind == engine.KindPool && !slices.Contains(tc.want, g) {
					t.Errorf("unexpected pool %v", g)
				}
			}
		})
	}
	// The agent routes survive the merge.
	got, _ := reg.Resolve(t.Context(), engine.Change{Table: "agents", TeamID: "team_A", ID: "agt_1"}, fan)
	if !slices.Contains(got, engine.NewRef(engine.KindAgent, "agt_1", "team_A")) {
		t.Errorf("agents row lost its agent target: %v", got)
	}
}

func TestPoolDriftKindsWatched(t *testing.T) {
	if len(k8s.PoolOwnedKinds()) == 0 {
		t.Fatal("no pool kinds watched")
	}
}

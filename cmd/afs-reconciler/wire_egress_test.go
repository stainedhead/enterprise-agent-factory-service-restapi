package main

import (
	"reflect"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

func TestMergeRoutes(t *testing.T) {
	in := []engine.Route{
		{Table: "agents", Targets: []engine.Target{{Kind: engine.KindAgent, Self: true}}},
		{Table: "teams", Targets: []engine.Target{{Kind: engine.KindTeam, Self: true}}},
		{Table: "agents", Targets: []engine.Target{{Kind: "egress"}, {Kind: engine.KindAgent, Self: true}}, FanOut: []engine.Kind{"egress"}},
		{Table: "agents", FanOut: []engine.Kind{"egress"}},
	}
	got := mergeRoutes(in)
	want := []engine.Route{
		{Table: "agents", Targets: []engine.Target{{Kind: engine.KindAgent, Self: true}, {Kind: "egress"}}, FanOut: []engine.Kind{"egress"}},
		{Table: "teams", Targets: []engine.Target{{Kind: engine.KindTeam, Self: true}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged = %+v\nwant %+v", got, want)
	}
	if _, err := engine.NewRegistry(got...); err != nil {
		t.Errorf("a merged table is routed once: %v", err)
	}
}

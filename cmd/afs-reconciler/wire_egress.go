package main

import (
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// egressWiring is the wiring of the per-agent egress (M2-H1): the controller, its routes and a lister
// wrapper that adds one egress resource per live team.
type egressWiring struct {
	Controller *egress.Controller
	Routes     []engine.Route
	WrapLister func(engine.Lister) engine.Lister
}

// wireEgress builds the egress controller.
func wireEgress(kc *k8s.Client, runner *postgres.TxRunner, pool postgres.Querier2, now func() time.Time) (egressWiring, error) {
	ctl, err := egress.NewController(egress.Config{
		Source: postgres.NewEgressSource(runner), Cluster: k8s.NewEgressCluster(kc), Agents: postgres.NewAgentConditions(runner, now),
	})
	if err != nil {
		return egressWiring{}, err
	}
	// The agents table is routed here too: a new, paused or deleted agent changes its team's egress.
	// Another controller routes it as well (the agent controller), so routes() merges targets per table.
	routes := append(egress.Routes(), engine.Route{Table: "agents", Targets: []engine.Target{egress.AgentsTarget()}})
	return egressWiring{
		Controller: ctl, Routes: routes,
		WrapLister: func(l engine.Lister) engine.Lister { return postgres.NewTeamLister(l, pool, egress.Kind) },
	}, nil
}

// mergeRoutes merges routes that name the same table into one (their targets and fan-outs are
// concatenated, without duplicates), so several controllers can route a table.
func mergeRoutes(in []engine.Route) []engine.Route {
	idx := map[string]int{}
	var out []engine.Route
	for _, r := range in {
		i, ok := idx[r.Table]
		if !ok {
			idx[r.Table] = len(out)
			out = append(out, engine.Route{Table: r.Table, Targets: append([]engine.Target(nil), r.Targets...), FanOut: append([]engine.Kind(nil), r.FanOut...)})
			continue
		}
		for _, t := range r.Targets {
			if !containsTarget(out[i].Targets, t) {
				out[i].Targets = append(out[i].Targets, t)
			}
		}
		for _, k := range r.FanOut {
			if !containsKind(out[i].FanOut, k) {
				out[i].FanOut = append(out[i].FanOut, k)
			}
		}
	}
	return out
}

func containsTarget(ts []engine.Target, t engine.Target) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

func containsKind(ks []engine.Kind, k engine.Kind) bool {
	for _, x := range ks {
		if x == k {
			return true
		}
	}
	return false
}

package postgres_test

import (
	"net/netip"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// staticDir resolves every address to one agent pod of the team under test.
type staticDir struct{ team, agent string }

func (d staticDir) Lookup(netip.Addr) (gateway.PodInfo, bool) {
	agent := "agt_01JD0000000000000000000A00"
	if d.agent != "" {
		agent = d.agent
	}
	return gateway.PodInfo{Namespace: "afs-t-x", Name: "agent-x-0", UID: "u", ServiceAccount: "agent-x", TeamID: d.team, AgentID: agent}, true
}
func (staticDir) Ready() bool { return true }

func (staticDir) request(model string) gateway.Request {
	return gateway.Request{SourceIP: netip.MustParseAddr("10.0.0.1"), Op: gateway.OpConverse, ModelID: model, RequestID: "req"}
}

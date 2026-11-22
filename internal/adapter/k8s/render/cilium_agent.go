package render

import (
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
)

// Names and labels of the per-agent egress policies (M2-H1).
const (
	// LabelAgentID is the label an agent's pod carries (set by the agent renderer) and the egress policy
	// selects on. The same label on a policy names the agent it belongs to.
	LabelAgentID = "afs.stainedhead.io/agent-id"
	// EgressKind is the engine kind (and resource-kind label) of the team's egress.
	EgressKind = "egress"
	// egressPolicyPrefix prefixes the policy names.
	egressPolicyPrefix = "afs-egress-"
)

// EgressPolicyName is the name of an agent's egress policy: DNS-1123, derived from the agent id.
func EgressPolicyName(agentID string) string {
	return egressPolicyPrefix + strings.ToLower(strings.ReplaceAll(agentID, "_", "-"))
}

// OwnerOfEgress is the owner of a team's egress objects: the team's egress resource, so drift on a
// policy reconciles the team's egress.
func OwnerOfEgress(teamID string) Owner { return Owner{Kind: EgressKind, ID: teamID, TeamID: teamID} }

// CiliumAgentEgress renders one agent's egress allowance: the endpoints of the systems that are on for
// it as FQDN rules, one rule per port. The team's default deny and its DNS and gateway allowances
// (CiliumTeamPolicy) stay in force; this policy only adds. An agent with no system on gets a policy
// with an empty egress rule, which selects the agent for egress enforcement and allows nothing more.
//
// Patterns: "host" is matchName, "*.domain" is matchPattern, which Cilium evaluates against names the
// agent resolved through the DNS proxy, so an address reached without a lookup is not allowed.
func CiliumAgentEgress(namespace string, p egress.AgentPolicy) *unstructured.Unstructured {
	u := New(ciliumAPIVersion, "CiliumNetworkPolicy", namespace, EgressPolicyName(p.AgentID))
	byPort := map[int][]any{}
	for _, r := range p.Rules {
		for _, e := range r.Endpoints {
			key := "matchName"
			if e.Wildcard() {
				key = "matchPattern"
			}
			byPort[e.Port] = append(byPort[e.Port], map[string]any{key: e.Host})
		}
	}
	ports := make([]int, 0, len(byPort))
	for port := range byPort {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	egressRules := []any{}
	for _, port := range ports {
		egressRules = append(egressRules, map[string]any{
			"toFQDNs": dedupe(byPort[port]),
			"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": strconv.Itoa(port), "protocol": "TCP"}}}},
		})
	}
	if len(egressRules) == 0 {
		egressRules = append(egressRules, map[string]any{})
	}
	u.Object["spec"] = map[string]any{
		"endpointSelector": map[string]any{"matchLabels": map[string]any{LabelAgentID: p.AgentID}},
		"egress":           egressRules,
	}
	Managed(u, OwnerOfEgress(p.TeamID))
	l := u.GetLabels()
	l[LabelAgentID] = p.AgentID
	u.SetLabels(l)
	return u
}

// dedupe drops repeated FQDN matchers (two systems may list one host), keeping the first order.
func dedupe(in []any) []any {
	seen := map[string]bool{}
	out := make([]any, 0, len(in))
	for _, v := range in {
		m := v.(map[string]any)
		var key string
		for k, s := range m {
			key = k + "=" + s.(string)
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, v)
		}
	}
	return out
}

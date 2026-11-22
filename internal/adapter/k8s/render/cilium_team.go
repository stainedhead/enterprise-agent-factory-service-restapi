package render

import (
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Names of the Cilium policies.
const (
	TeamEgressPolicyName     = "afs-allow-dns-and-gateway"
	GatewayIngressPolicyName = "afs-gateway-ingress-from-teams"
	ciliumAPIVersion         = "cilium.io/v2"
	podNamespaceLabel        = "k8s:io.kubernetes.pod.namespace"
	gatewayNameLabel         = "app.kubernetes.io/name"
	// namespaceLabelPrefix is how Cilium exposes namespace labels in an endpoint's identity.
	namespaceLabelPrefix = "k8s:io.cilium.k8s.namespace.labels."
)

func port(p int) string { return strconv.Itoa(p) }

// CiliumTeamPolicy renders the team's egress allowances on top of the default deny: DNS to the cluster
// resolver (with DNS visibility, which FQDN rules need) and the connection to afs-gateway, the only
// path to Bedrock (D57).
func CiliumTeamPolicy(env team.Environment) *unstructured.Unstructured {
	u := New(ciliumAPIVersion, "CiliumNetworkPolicy", env.Team.Namespace, TeamEgressPolicyName)
	u.Object["spec"] = map[string]any{
		"endpointSelector": map[string]any{},
		"egress": []any{
			map[string]any{
				"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{
					podNamespaceLabel: "kube-system", "k8s-app": "kube-dns",
				}}},
				"toPorts": []any{map[string]any{
					"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}},
					"rules": map[string]any{"dns": []any{map[string]any{"matchPattern": "*"}}},
				}},
			},
			map[string]any{
				"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{
					podNamespaceLabel: env.GatewayNamespace, gatewayNameLabel: env.GatewayName,
				}}},
				"toPorts": []any{map[string]any{
					"ports": []any{map[string]any{"port": port(env.GatewayPort), "protocol": "TCP"}},
				}},
			},
		},
	}
	return Managed(u, OwnerOfTeam(env.Team))
}

// OwnerPlatform is the owner of the cluster-wide objects.
func OwnerPlatform() Owner { return Owner{Kind: string(engine.KindPlatform), ID: "afs-platform"} }

// CiliumGatewayIngress renders the policy on afs-gateway's pods: they accept connections only from pods
// of namespaces labelled as team namespaces. Cilium evaluates it against the pod identity (its
// namespace's labels), not the source address, so it holds with WireGuard on and cannot be spoofed by
// an address (D57, RC-US-1a.4).
func CiliumGatewayIngress(p team.PlatformSpec) *unstructured.Unstructured {
	u := New(ciliumAPIVersion, "CiliumNetworkPolicy", p.GatewayNamespace, GatewayIngressPolicyName)
	u.Object["spec"] = map[string]any{
		"endpointSelector": map[string]any{"matchLabels": map[string]any{gatewayNameLabel: p.GatewayName}},
		"ingress": []any{map[string]any{
			"fromEndpoints": []any{map[string]any{"matchLabels": map[string]any{
				namespaceLabelPrefix + LabelTeamNamespace: "true",
			}}},
			"toPorts": []any{map[string]any{
				"ports": []any{map[string]any{"port": port(p.GatewayPort), "protocol": "TCP"}},
			}},
		}},
	}
	return Managed(u, OwnerPlatform())
}

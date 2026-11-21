package render

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Names of the default-deny policies.
const (
	DenyIngressName = "afs-default-deny-ingress"
	DenyEgressName  = "afs-default-deny-egress"
)

// DefaultDeny renders the two default-deny NetworkPolicies of a team namespace (TEAM-3): no pod accepts
// or opens any connection until a policy allows it. The Cilium policies of CiliumTeamPolicy and the
// per-agent egress rules open what is needed.
func DefaultDeny(t team.Desired) []*unstructured.Unstructured {
	mk := func(name, policyType string) *unstructured.Unstructured {
		u := New("networking.k8s.io/v1", "NetworkPolicy", t.Namespace, name)
		u.Object["spec"] = map[string]any{
			"podSelector": map[string]any{},
			"policyTypes": []any{policyType},
		}
		return Managed(u, OwnerOfTeam(t))
	}
	return []*unstructured.Unstructured{mk(DenyIngressName, "Ingress"), mk(DenyEgressName, "Egress")}
}

package render

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Names of the quota objects in a team namespace.
const (
	QuotaName      = "afs-team-quota"
	LimitRangeName = "afs-team-limits"
)

// Container defaults of the LimitRange: pods that set nothing still get bounded.
const (
	defaultRequestCPU    = "100m"
	defaultRequestMemory = "128Mi"
	defaultLimitCPU      = "1"
	defaultLimitMemory   = "1Gi"
)

// ResourceQuota renders the namespace's quota from the team's quotas (TEAM-2). A quota field that is
// empty sets no limit.
func ResourceQuota(t team.Desired) *unstructured.Unstructured {
	u := New("v1", "ResourceQuota", t.Namespace, QuotaName)
	hard := map[string]any{}
	if t.Quotas.CPU != "" {
		hard["cpu"] = t.Quotas.CPU
	}
	if t.Quotas.Memory != "" {
		hard["memory"] = t.Quotas.Memory
	}
	if t.Quotas.Pods > 0 {
		hard["pods"] = t.Quotas.Pods
	}
	if t.Quotas.Storage != "" {
		hard["requests.storage"] = t.Quotas.Storage
	}
	u.Object["spec"] = map[string]any{"hard": hard}
	return Managed(u, OwnerOfTeam(t))
}

// LimitRange renders the per-container defaults.
func LimitRange(t team.Desired) *unstructured.Unstructured {
	u := New("v1", "LimitRange", t.Namespace, LimitRangeName)
	u.Object["spec"] = map[string]any{"limits": []any{map[string]any{
		"type":           "Container",
		"defaultRequest": map[string]any{"cpu": defaultRequestCPU, "memory": defaultRequestMemory},
		"default":        map[string]any{"cpu": defaultLimitCPU, "memory": defaultLimitMemory},
	}}}
	return Managed(u, OwnerOfTeam(t))
}

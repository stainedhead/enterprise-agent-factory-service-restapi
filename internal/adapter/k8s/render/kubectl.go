package render

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Names and kind of the viewer kubectl objects (RC-13, M5-E2).
const (
	// ViewerKubectlKind is the resource kind label of a team's viewer kubectl objects.
	ViewerKubectlKind = "kubectl"
	// ViewerRoleName names both the read-only Role and the RoleBinding of a team namespace.
	ViewerRoleName = "afs-viewer"
)

// OwnerOfViewerKubectl is the owner of a team's viewer kubectl objects.
func OwnerOfViewerKubectl(t team.Desired) Owner {
	return Owner{Kind: ViewerKubectlKind, ID: t.ID, TeamID: t.ID, TeamSlug: t.Slug}
}

// viewerRules is everything a viewer may do: get, list and watch workloads, their pods' logs, events and
// the agents' custom resources, in one namespace. There is no secrets rule, no pods/exec, pods/attach,
// pods/portforward or pods/proxy, no configmaps and no verb but the three reads; TestViewerRoleIsReadOnly
// pins that, and the envtest case proves the cluster refuses the rest.
func viewerRules() []any {
	read := []any{"get", "list", "watch"}
	rule := func(groups []any, resources ...string) map[string]any {
		rs := make([]any, len(resources))
		for i, r := range resources {
			rs[i] = r
		}
		return map[string]any{"apiGroups": groups, "resources": rs, "verbs": read}
	}
	return []any{
		rule([]any{""}, "pods", "pods/log", "services", "endpoints", "events", "persistentvolumeclaims", "resourcequotas"),
		rule([]any{"apps"}, "deployments", "replicasets", "statefulsets", "daemonsets"),
		rule([]any{"batch"}, "jobs", "cronjobs"),
		rule([]any{"events.k8s.io"}, "events"),
		rule([]any{"agenticteammates.stainedhead.io"}, "*"),
	}
}

// ViewerRole renders the namespace Role of viewer kubectl.
func ViewerRole(namespace string, t team.Desired) *unstructured.Unstructured {
	u := New("rbac.authorization.k8s.io/v1", "Role", namespace, ViewerRoleName)
	u.Object["rules"] = viewerRules()
	return Managed(u, OwnerOfViewerKubectl(t))
}

// ViewerRoleBinding renders the RoleBinding of the viewer Role to the Kubernetes groups the federated
// roles' access entries map to.
func ViewerRoleBinding(namespace string, t team.Desired, groups []string) *unstructured.Unstructured {
	u := New("rbac.authorization.k8s.io/v1", "RoleBinding", namespace, ViewerRoleName)
	subjects := make([]any, len(groups))
	for i, g := range groups {
		subjects[i] = map[string]any{"kind": "Group", "apiGroup": "rbac.authorization.k8s.io", "name": g}
	}
	u.Object["roleRef"] = map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": ViewerRoleName}
	u.Object["subjects"] = subjects
	return Managed(u, OwnerOfViewerKubectl(t))
}

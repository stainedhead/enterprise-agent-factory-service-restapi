package render

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// LabelTeamNamespace marks a namespace as a team namespace. Cilium copies namespace labels into pod
// identities, which is how the policy on afs-gateway admits only team pods (D57).
const LabelTeamNamespace = "afs.stainedhead.io/team-namespace"

// OwnerOfTeam is the owner of every object of a team's environment.
func OwnerOfTeam(t team.Desired) Owner {
	return Owner{Kind: string(engine.KindTeam), ID: t.ID, TeamID: t.ID, TeamSlug: t.Slug}
}

// Namespace renders the team namespace "afs-t-<slug>" with Pod Security "restricted" enforced,
// audited and warned (RC-1, TEAM-2).
func Namespace(t team.Desired) *unstructured.Unstructured {
	u := New("v1", "Namespace", "", t.Namespace)
	l := map[string]string{
		"pod-security.kubernetes.io/enforce":         "restricted",
		"pod-security.kubernetes.io/enforce-version": "latest",
		"pod-security.kubernetes.io/audit":           "restricted",
		"pod-security.kubernetes.io/audit-version":   "latest",
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/warn-version":    "latest",
		LabelTeamNamespace:                           "true",
	}
	u.SetLabels(l)
	return Managed(u, OwnerOfTeam(t))
}

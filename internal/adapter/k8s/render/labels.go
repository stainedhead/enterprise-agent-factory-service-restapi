// Package render builds the Kubernetes objects the reconciler owns from plain Go descriptions of the
// desired state. Objects are unstructured, so third-party custom resources (Cilium, External Secrets,
// Karpenter) need no vendored Go types (ADR-006). Nothing here talks to a cluster.
package render

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Labels every object the reconciler owns carries. The drift watch selects on ManagedBy, and the
// reconciler never applies an object without it.
const (
	LabelManagedBy    = "app.kubernetes.io/managed-by"
	ManagedByValue    = "afs-reconciler"
	LabelTeamID       = "afs.stainedhead.io/team-id"
	LabelTeamSlug     = "afs.stainedhead.io/team-slug"
	LabelResourceKind = "afs.stainedhead.io/resource-kind"
	LabelResourceID   = "afs.stainedhead.io/resource-id"
)

// Owner names the resource an object belongs to; it fills the managed labels.
type Owner struct {
	// Kind is the engine kind (team, agent, pool, admission).
	Kind string
	// ID is the resource id (or the singleton's name).
	ID string
	// TeamID is empty for cluster-wide singletons.
	TeamID string
	// TeamSlug is optional and only informational.
	TeamSlug string
}

// Labels returns the managed labels of o.
func (o Owner) Labels() map[string]string {
	l := map[string]string{LabelManagedBy: ManagedByValue, LabelResourceKind: o.Kind, LabelResourceID: o.ID}
	if o.TeamID != "" {
		l[LabelTeamID] = o.TeamID
	}
	if o.TeamSlug != "" {
		l[LabelTeamSlug] = o.TeamSlug
	}
	return l
}

// Managed adds the owner's labels to obj, keeping labels it already has, and returns obj.
func Managed(obj *unstructured.Unstructured, o Owner) *unstructured.Unstructured {
	l := obj.GetLabels()
	if l == nil {
		l = map[string]string{}
	}
	for k, v := range o.Labels() {
		l[k] = v
	}
	obj.SetLabels(l)
	return obj
}

// IsManaged reports whether obj carries the managed-by label.
func IsManaged(obj *unstructured.Unstructured) bool {
	return obj.GetLabels()[LabelManagedBy] == ManagedByValue
}

// OwnerOf reads the owner labels back from obj.
func OwnerOf(obj *unstructured.Unstructured) Owner {
	l := obj.GetLabels()
	return Owner{Kind: l[LabelResourceKind], ID: l[LabelResourceID], TeamID: l[LabelTeamID], TeamSlug: l[LabelTeamSlug]}
}

// New returns an empty object of the given group, version and kind.
func New(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	u.SetName(name)
	if namespace != "" {
		u.SetNamespace(namespace)
	}
	return u
}

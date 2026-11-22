package k8s

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/kubectl"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

var (
	gvkRole        = schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"}
	gvkRoleBinding = schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}
)

// KubectlCluster implements kubectl.Cluster: the read-only Role and its RoleBinding in a team namespace,
// applied with server-side apply (M5-E2, RC-13).
type KubectlCluster struct{ client *Client }

// NewKubectlCluster returns a KubectlCluster.
func NewKubectlCluster(c *Client) *KubectlCluster { return &KubectlCluster{client: c} }

var _ kubectl.Cluster = (*KubectlCluster)(nil)

func desired(t kubectl.Team) team.Desired {
	return team.Desired{ID: t.ID, Slug: t.Slug, Namespace: t.Namespace}
}

// ApplyViewerAccess implements kubectl.Cluster. The Role goes first so the binding never names a Role that
// does not exist; both are attempted and the first error is returned.
func (c *KubectlCluster) ApplyViewerAccess(ctx context.Context, namespace string, t kubectl.Team, groups []string) (kubectl.ApplyResult, error) {
	var res kubectl.ApplyResult
	var first error
	for _, obj := range []struct {
		kind string
		do   func() (ApplyResult, error)
	}{
		{"Role", func() (ApplyResult, error) { return c.client.Apply(ctx, render.ViewerRole(namespace, desired(t))) }},
		{"RoleBinding", func() (ApplyResult, error) {
			return c.client.Apply(ctx, render.ViewerRoleBinding(namespace, desired(t), groups))
		}},
	} {
		r, err := obj.do()
		if r.Modified() {
			res.Changed = append(res.Changed, r.Name)
		}
		if err != nil && first == nil {
			first = fmt.Errorf("%s %s: %w", obj.kind, render.ViewerRoleName, err)
		}
	}
	return res, first
}

// RemoveViewerAccess implements kubectl.Cluster: the RoleBinding goes first, which ends the access, then
// the Role. Only objects the reconciler manages are deleted: a Role named afs-viewer that someone else made
// is left alone.
func (c *KubectlCluster) RemoveViewerAccess(ctx context.Context, namespace string) (kubectl.ApplyResult, error) {
	var res kubectl.ApplyResult
	var first error
	for _, gvk := range []schema.GroupVersionKind{gvkRoleBinding, gvkRole} {
		live, err := c.client.Get(ctx, gvk, namespace, render.ViewerRoleName)
		if err != nil {
			if first == nil {
				first = fmt.Errorf("get %s: %w", gvk.Kind, err)
			}
			continue
		}
		if live == nil || !render.IsManaged(live) {
			continue
		}
		if err := c.client.Delete(ctx, gvk, namespace, render.ViewerRoleName); err != nil {
			if first == nil {
				first = fmt.Errorf("delete %s: %w", gvk.Kind, err)
			}
			continue
		}
		res.Changed = append(res.Changed, engine.ObjectName(gvk.Kind, namespace, render.ViewerRoleName))
	}
	return res, first
}

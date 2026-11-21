package k8s

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

var (
	getOptions    = metav1.GetOptions{}
	deleteOptions = metav1.DeleteOptions{}
)

func listOptions(selector string) metav1.ListOptions {
	return metav1.ListOptions{LabelSelector: selector}
}

// ApplyResult says what an Apply did.
type ApplyResult struct {
	// Name is engine.ObjectName of the object.
	Name string
	// Created is true when the object did not exist.
	Created bool
	// Changed is true when the object existed and the apply modified it: a spec change or someone
	// else's edit being reverted.
	Changed         bool
	ResourceVersion string
}

// Modified reports whether the apply created or changed the object.
func (r ApplyResult) Modified() bool { return r.Created || r.Changed }

// Apply server-side applies obj as field manager afs-reconciler, forcing ownership of the fields it
// sets, so edits by others to those fields are reverted. A no-op apply writes nothing.
func (c *Client) Apply(ctx context.Context, obj *unstructured.Unstructured) (ApplyResult, error) {
	if !render.IsManaged(obj) {
		return ApplyResult{}, ErrNotManaged
	}
	gvk := obj.GroupVersionKind()
	ri, err := c.iface(gvk, obj.GetNamespace())
	if err != nil {
		return ApplyResult{}, err
	}
	res := ApplyResult{Name: engine.ObjectName(gvk.Kind, obj.GetNamespace(), obj.GetName())}
	before, err := c.Get(ctx, gvk, obj.GetNamespace(), obj.GetName())
	if err != nil {
		return res, err
	}
	data, err := obj.MarshalJSON()
	if err != nil {
		return res, err
	}
	force := true
	after, err := ri.Patch(ctx, obj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	if err != nil {
		return res, wrap(err)
	}
	res.ResourceVersion = after.GetResourceVersion()
	res.Created = before == nil
	res.Changed = before != nil && before.GetResourceVersion() != after.GetResourceVersion()
	if res.Modified() {
		c.rememberWrite(objectKey(gvk, obj.GetNamespace(), obj.GetName()), res.ResourceVersion)
	}
	return res, nil
}

// rememberWrite records the resourceVersion of our own write so the drift watch ignores its echo.
func (c *Client) rememberWrite(key, rv string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.own) > 10000 {
		c.own = map[string]string{}
	}
	c.own[key] = rv
}

// IsOwnWrite reports whether the object at resourceVersion rv is the result of our last write.
func (c *Client) IsOwnWrite(key, rv string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.own[key] == rv
}

// forgetWrite drops a remembered write.
func (c *Client) forgetWrite(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.own, key)
}

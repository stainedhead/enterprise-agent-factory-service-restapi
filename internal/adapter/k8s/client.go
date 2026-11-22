package k8s

import (
	"context"
	"errors"
	"fmt"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// FieldManager is the server-side apply field manager of every write the reconciler makes.
const FieldManager = "afs-reconciler"

// ErrNotManaged is returned when asked to apply an object without the managed-by label: the
// reconciler never touches objects it does not own.
var ErrNotManaged = errors.New("k8s: refusing to apply an object without the afs managed-by label")

// Client applies and deletes unstructured objects with server-side apply.
type Client struct {
	dyn    dynamic.Interface
	mapper meta.ResettableRESTMapper

	mu      sync.Mutex
	changed *sync.Cond          // signalled when a write in flight finishes
	own     map[string][]string // objectKey -> resourceVersions of our most recent writes (or deletedMarker)
	pending map[string]int      // objectKey -> writes in flight
}

// NewClient builds a Client for the cluster cfg points at.
func NewClient(cfg *rest.Config) (*Client, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: discovery client: %w", err)
	}
	return NewClientFor(dyn, restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc))), nil
}

// NewClientFor builds a Client from parts (tests use the fake dynamic client).
func NewClientFor(dyn dynamic.Interface, mapper meta.ResettableRESTMapper) *Client {
	c := &Client{dyn: dyn, mapper: mapper, own: map[string][]string{}, pending: map[string]int{}}
	c.changed = sync.NewCond(&c.mu)
	return c
}

// Dynamic returns the underlying dynamic client (the drift watch and tests share it).
func (c *Client) Dynamic() dynamic.Interface { return c.dyn }

// resource maps a kind to its REST resource, refreshing discovery once when the kind is not known
// yet (a CRD that was installed after the cache was filled).
func (c *Client) resource(gvk schema.GroupVersionKind) (schema.GroupVersionResource, bool, error) {
	m, err := c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		c.mapper.Reset()
		m, err = c.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		return schema.GroupVersionResource{}, false, fmt.Errorf("k8s: resolve %s: %w", gvk, wrap(err))
	}
	return m.Resource, m.Scope.Name() == meta.RESTScopeNameNamespace, nil
}

func (c *Client) iface(gvk schema.GroupVersionKind, namespace string) (dynamic.ResourceInterface, error) {
	gvr, namespaced, err := c.resource(gvk)
	if err != nil {
		return nil, err
	}
	if namespaced {
		if namespace == "" {
			return nil, fmt.Errorf("k8s: %s is namespaced and needs a namespace", gvk.Kind)
		}
		return c.dyn.Resource(gvr).Namespace(namespace), nil
	}
	return c.dyn.Resource(gvr), nil
}

// Get returns the live object, or (nil, nil) when it does not exist.
func (c *Client) Get(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, error) {
	ri, err := c.iface(gvk, namespace)
	if err != nil {
		return nil, err
	}
	obj, err := ri.Get(ctx, name, getOptions)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrap(err)
	}
	return obj, nil
}

// Delete removes an object; a missing object is not an error.
func (c *Client) Delete(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) error {
	ri, err := c.iface(gvk, namespace)
	if err != nil {
		return err
	}
	key := objectKey(gvk, namespace, name)
	c.beginWrite(key)
	defer c.endWrite(key)
	c.rememberWrite(key, deletedMarker)
	if err := ri.Delete(ctx, name, deleteOptions); err != nil && !apierrors.IsNotFound(err) {
		c.forgetWrite(key)
		return wrap(err)
	}
	return nil
}

// List returns the objects of a kind with a label selector, in one namespace or (empty) all.
func (c *Client) List(ctx context.Context, gvk schema.GroupVersionKind, namespace, selector string) ([]unstructured.Unstructured, error) {
	gvr, namespaced, err := c.resource(gvk)
	if err != nil {
		return nil, err
	}
	var ri dynamic.ResourceInterface = c.dyn.Resource(gvr)
	if namespaced && namespace != "" {
		ri = c.dyn.Resource(gvr).Namespace(namespace)
	}
	list, err := ri.List(ctx, listOptions(selector))
	if err != nil {
		return nil, wrap(err)
	}
	return list.Items, nil
}

// wrap gives a Kubernetes API error the error code the engine records as a condition reason.
func wrap(err error) error {
	if err == nil {
		return nil
	}
	if reason := apierrors.ReasonForError(err); reason != "" {
		return &engine.CodedError{Code: string(reason), Err: err}
	}
	return err
}

// ObjectKey names an object for the own-write cache and for drift correlation.
func objectKey(gvk schema.GroupVersionKind, namespace, name string) string {
	return engine.ObjectName(gvk.Kind, namespace, name)
}

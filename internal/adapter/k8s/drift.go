package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// deletedMarker stands in for a resourceVersion in the own-write cache when we deleted the object.
const deletedMarker = "<deleted>"

// DriftHandler is called when an owned object was changed or deleted by someone other than the
// reconciler. ref is the resource that owns the object and object its engine.ObjectName.
type DriftHandler func(ref engine.Ref, object string)

// Watcher watches the objects the reconciler owns (those with the managed-by label) and reports edits
// and deletes made by others (RC-US-3.1). It only detects: the reconcile that follows reverts.
type Watcher struct {
	client  *Client
	gvrs    []schema.GroupVersionResource
	handler DriftHandler
	log     *slog.Logger
	resync  time.Duration
}

// NewWatcher watches the given kinds. Kinds are resolved to resources through the client's mapper.
func NewWatcher(c *Client, handler DriftHandler, log *slog.Logger, kinds ...schema.GroupVersionKind) (*Watcher, error) {
	w := &Watcher{client: c, handler: handler, log: log, resync: 0}
	if log == nil {
		w.log = slog.Default()
	}
	for _, k := range kinds {
		gvr, _, err := c.resource(k)
		if err != nil {
			return nil, err
		}
		w.gvrs = append(w.gvrs, gvr)
	}
	return w, nil
}

// Run starts the informers, waits for their first sync and blocks until ctx ends.
func (w *Watcher) Run(ctx context.Context) error {
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(w.client.dyn, w.resync, "", func(o *metav1.ListOptions) {
		o.LabelSelector = render.LabelManagedBy + "=" + render.ManagedByValue
	})
	for _, gvr := range w.gvrs {
		inf := factory.ForResource(gvr).Informer()
		_, err := inf.AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(oldObj, newObj any) { w.onUpdate(oldObj, newObj) },
			DeleteFunc: func(obj any) { w.onDelete(obj) },
		})
		if err != nil {
			return fmt.Errorf("k8s: watch %s: %w", gvr, err)
		}
	}
	factory.Start(ctx.Done())
	for gvr, ok := range factory.WaitForCacheSync(ctx.Done()) {
		if !ok && ctx.Err() == nil {
			return fmt.Errorf("k8s: cache for %s did not sync", gvr)
		}
	}
	<-ctx.Done()
	factory.Shutdown()
	return nil
}

func (w *Watcher) onUpdate(oldObj, newObj any) {
	o, ok1 := oldObj.(*unstructured.Unstructured)
	n, ok2 := newObj.(*unstructured.Unstructured)
	if !ok1 || !ok2 || !significantChange(o, n) {
		return
	}
	key := objectKey(n.GroupVersionKind(), n.GetNamespace(), n.GetName())
	if w.client.IsOwnWrite(key, n.GetResourceVersion()) {
		return
	}
	w.report(n, key)
}

func (w *Watcher) onDelete(obj any) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return
	}
	key := objectKey(u.GroupVersionKind(), u.GetNamespace(), u.GetName())
	if w.client.IsOwnWrite(key, deletedMarker) {
		w.client.forgetWrite(key)
		return
	}
	w.report(u, key)
}

func (w *Watcher) report(u *unstructured.Unstructured, key string) {
	o := render.OwnerOf(u)
	if o.Kind == "" || o.ID == "" {
		return
	}
	w.log.Info("drift detected", slog.String("object", key), slog.String("owner", o.Kind+"/"+o.ID))
	w.handler(engine.NewRef(engine.Kind(o.Kind), o.ID, o.TeamID), key)
}

// significantChange reports whether newObj differs from oldObj in anything but what the cluster
// itself maintains: status, resourceVersion, generation, managedFields. A change in those is not
// somebody editing what the reconciler owns.
func significantChange(oldObj, newObj *unstructured.Unstructured) bool {
	return !reflect.DeepEqual(stripVolatile(oldObj), stripVolatile(newObj))
}

func stripVolatile(u *unstructured.Unstructured) map[string]any {
	c := u.DeepCopy()
	delete(c.Object, "status")
	md, _ := c.Object["metadata"].(map[string]any)
	for _, k := range []string{"resourceVersion", "generation", "managedFields", "creationTimestamp", "uid", "selfLink"} {
		delete(md, k)
	}
	return c.Object
}

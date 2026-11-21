package k8s

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/restmapper"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

func obj(data string, mut func(*unstructured.Unstructured)) *unstructured.Unstructured {
	u := render.New("v1", "ConfigMap", "ns", "cm")
	u.Object["data"] = map[string]any{"k": data}
	u.SetResourceVersion("1")
	if mut != nil {
		mut(u)
	}
	return u
}

func TestSignificantChange(t *testing.T) {
	base := obj("a", nil)
	tests := []struct {
		name string
		mut  func(*unstructured.Unstructured)
		data string
		want bool
	}{
		{"identical", nil, "a", false},
		{"resourceVersion only", func(u *unstructured.Unstructured) { u.SetResourceVersion("99") }, "a", false},
		{"generation only", func(u *unstructured.Unstructured) { u.SetGeneration(5) }, "a", false},
		{"managedFields only", func(u *unstructured.Unstructured) {
			u.SetManagedFields(nil)
			_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"manager": "x"}}, "metadata", "managedFields")
		}, "a", false},
		{"status only", func(u *unstructured.Unstructured) { u.Object["status"] = map[string]any{"used": "1"} }, "a", false},
		{"data changed", nil, "b", true},
		{"label changed", func(u *unstructured.Unstructured) { u.SetLabels(map[string]string{"x": "y"}) }, "a", true},
		{"annotation added", func(u *unstructured.Unstructured) { u.SetAnnotations(map[string]string{"x": "y"}) }, "a", true},
	}
	for _, tt := range tests {
		if got := significantChange(base, obj(tt.data, tt.mut)); got != tt.want {
			t.Errorf("%s: significantChange = %v, want %v", tt.name, got, tt.want)
		}
	}
	if base.GetResourceVersion() != "1" {
		t.Error("significantChange modified its input")
	}
}

func fakeClient() *Client {
	scheme := runtime.NewScheme()
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(nil)
	_ = mapper
	dyn := dynfake.NewSimpleDynamicClient(scheme)
	return NewClientFor(dyn, resettable{meta.NewDefaultRESTMapper(nil)})
}

type resettable struct{ *meta.DefaultRESTMapper }

func (resettable) Reset() {}

func TestApplyRefusesUnmanaged(t *testing.T) {
	_, err := fakeClient().Apply(context.Background(), render.New("v1", "ConfigMap", "ns", "x"))
	if !errors.Is(err, ErrNotManaged) {
		t.Errorf("err = %v", err)
	}
}

func TestUnknownKindIsAnError(t *testing.T) {
	c := fakeClient()
	o := render.Managed(render.New("v1", "ConfigMap", "ns", "x"), render.Owner{Kind: "team", ID: "team_X"})
	if _, err := c.Apply(context.Background(), o); err == nil {
		t.Error("apply of an unmapped kind succeeded")
	}
	if err := c.Delete(context.Background(), schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "ns", "x"); err == nil {
		t.Error("delete of an unmapped kind succeeded")
	}
	if _, err := c.Get(context.Background(), schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "ns", "x"); err == nil {
		t.Error("get of an unmapped kind succeeded")
	}
	if _, err := c.List(context.Background(), schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, "ns", ""); err == nil {
		t.Error("list of an unmapped kind succeeded")
	}
	if _, err := NewWatcher(c, nil, nil, schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}); err == nil {
		t.Error("watcher for an unmapped kind created")
	}
}

func TestOwnWriteCache(t *testing.T) {
	c := fakeClient()
	c.rememberWrite("ConfigMap/ns/a", "5")
	if !c.IsOwnWrite("ConfigMap/ns/a", "5") || c.IsOwnWrite("ConfigMap/ns/a", "6") || c.IsOwnWrite("ConfigMap/ns/b", "5") {
		t.Error("own write cache")
	}
	c.forgetWrite("ConfigMap/ns/a")
	if c.IsOwnWrite("ConfigMap/ns/a", "5") {
		t.Error("forgotten write still remembered")
	}
	for i := range 10001 { // bounded
		c.rememberWrite(string(rune(i)), "1")
	}
	if len(c.own) > 10001 {
		t.Errorf("cache grew to %d", len(c.own))
	}
}

func TestWrapGivesReasonAsCode(t *testing.T) {
	if wrap(nil) != nil {
		t.Error("nil")
	}
	plain := errors.New("x")
	if !errors.Is(wrap(plain), plain) {
		t.Error("non-API errors pass through")
	}
}

func TestReportSkipsUnownedObjects(t *testing.T) {
	var got []string
	w := &Watcher{client: fakeClient(), handler: func(ref engine.Ref, o string) { got = append(got, ref.String()+"|"+o) }, log: nopLogger()}
	w.report(render.New("v1", "ConfigMap", "ns", "x"), "ConfigMap/ns/x") // no owner labels
	if len(got) != 0 {
		t.Errorf("reported without an owner: %v", got)
	}
	w.report(render.Managed(render.New("v1", "ConfigMap", "ns", "x"), render.Owner{Kind: "team", ID: "team_X", TeamID: "team_X"}), "ConfigMap/ns/x")
	if len(got) != 1 || got[0] != "team/team_X|ConfigMap/ns/x" {
		t.Errorf("got %v", got)
	}
}

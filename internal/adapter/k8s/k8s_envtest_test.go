//go:build envtest

package k8s_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/envtesting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

var env *envtesting.Env

func TestMain(m *testing.M) { os.Exit(envtesting.Run(m, &env)) }

var (
	gvkConfigMap = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
	gvkNamespace = schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	gvkCNP       = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}
)

func newClient(t *testing.T) *k8s.Client {
	t.Helper()
	e := envtesting.Require(t, env)
	c, err := k8s.NewClient(e.Config)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var owner = render.Owner{Kind: "team", ID: "team_01JD0000000000000000000000", TeamID: "team_01JD0000000000000000000000"}

func configMap(ns, name, value string) *unstructured.Unstructured {
	u := render.New("v1", "ConfigMap", ns, name)
	u.Object["data"] = map[string]any{"key": value}
	return render.Managed(u, owner)
}

// M1-F1: server-side apply creates, is a no-op when nothing differs, and reverts edits and deletes.
func TestApplyCreatesIsIdempotentAndReverts(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	ns := "f1-apply"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)

	res, err := c.Apply(ctx, configMap(ns, "cfg", "one"))
	if err != nil || !res.Created || res.Changed || res.Name != "ConfigMap/f1-apply/cfg" {
		t.Fatalf("create: %+v, %v", res, err)
	}
	again, err := c.Apply(ctx, configMap(ns, "cfg", "one"))
	if err != nil || again.Modified() || again.ResourceVersion != res.ResourceVersion {
		t.Fatalf("idempotent apply wrote: %+v, %v", again, err)
	}

	// Someone edits a field we own.
	cs := envtesting.Clientset(t, env.Config)
	cm, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "cfg", metav1.GetOptions{})
	cm.Data["key"] = "tampered"
	if _, err := cs.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{FieldManager: "kubectl-edit"}); err != nil {
		t.Fatal(err)
	}
	fixed, err := c.Apply(ctx, configMap(ns, "cfg", "one"))
	if err != nil || !fixed.Changed {
		t.Fatalf("revert: %+v, %v", fixed, err)
	}
	if got, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "cfg", metav1.GetOptions{}); got.Data["key"] != "one" {
		t.Errorf("value = %q, want reverted", got.Data["key"])
	}
	// A field someone else added (not ours) survives: only owned fields are enforced.
	cm, _ = cs.CoreV1().ConfigMaps(ns).Get(ctx, "cfg", metav1.GetOptions{})
	cm.Data["extra"] = "kept"
	_, _ = cs.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{FieldManager: "kubectl-edit"})
	if _, err := c.Apply(ctx, configMap(ns, "cfg", "one")); err != nil {
		t.Fatal(err)
	}
	if got, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "cfg", metav1.GetOptions{}); got.Data["extra"] != "kept" {
		t.Error("a field the reconciler does not own was removed")
	}

	// Field manager is recorded.
	live, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "cfg", metav1.GetOptions{})
	found := false
	for _, mf := range live.ManagedFields {
		if mf.Manager == k8s.FieldManager && mf.Operation == metav1.ManagedFieldsOperationApply {
			found = true
		}
	}
	if !found {
		t.Errorf("no apply entry by %q in managedFields", k8s.FieldManager)
	}
	if live.Labels[render.LabelManagedBy] != render.ManagedByValue {
		t.Errorf("managed label missing: %v", live.Labels)
	}

	// Deleted: recreated.
	if err := c.Delete(ctx, gvkConfigMap, ns, "cfg"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, gvkConfigMap, ns, "cfg"); err != nil {
		t.Errorf("deleting a missing object: %v", err)
	}
	if got, err := c.Get(ctx, gvkConfigMap, ns, "cfg"); err != nil || got != nil {
		t.Errorf("Get after delete = %v, %v", got, err)
	}
	re, err := c.Apply(ctx, configMap(ns, "cfg", "one"))
	if err != nil || !re.Created {
		t.Errorf("recreate: %+v, %v", re, err)
	}
	items, err := c.List(ctx, gvkConfigMap, ns, render.LabelManagedBy+"="+render.ManagedByValue)
	if err != nil || len(items) != 1 {
		t.Errorf("List = %d, %v", len(items), err)
	}
}

func TestApplyRefusesUnmanagedAndReportsCodes(t *testing.T) {
	c := newClient(t)
	u := render.New("v1", "ConfigMap", "default", "x")
	if _, err := c.Apply(t.Context(), u); !errors.Is(err, k8s.ErrNotManaged) {
		t.Errorf("unmanaged apply: %v", err)
	}
	// A namespaced kind without a namespace is refused before any request.
	nons := render.Managed(render.New("v1", "ConfigMap", "", "x"), owner)
	if _, err := c.Apply(t.Context(), nons); err == nil {
		t.Error("namespaced object without namespace accepted")
	}
	// Unknown kind.
	unk := render.Managed(render.New("nothing.example.com/v1", "Widget", "default", "x"), owner)
	if _, err := c.Apply(t.Context(), unk); err == nil {
		t.Error("unknown kind accepted")
	}
	// The API server's reason becomes the error code the engine records: applying into a namespace
	// that does not exist is NotFound.
	_, err := c.Apply(t.Context(), configMap("no-such-namespace", "x", "v"))
	if engine.CodeOf(err) != "NotFound" {
		t.Errorf("code = %q, err = %v", engine.CodeOf(err), err)
	}
}

// M1-F1: pinned third-party CRDs are applied as unstructured objects (Cilium shown).
func TestApplyCustomResource(t *testing.T) {
	c := newClient(t)
	ns := "f1-crd"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	cnp := render.Managed(render.New("cilium.io/v2", "CiliumNetworkPolicy", ns, "allow-dns"), owner)
	cnp.Object["spec"] = map[string]any{"endpointSelector": map[string]any{}, "egress": []any{map[string]any{"toPorts": []any{}}}}
	res, err := c.Apply(t.Context(), cnp)
	if err != nil || !res.Created {
		t.Fatalf("apply CiliumNetworkPolicy: %+v, %v", res, err)
	}
	if got, err := c.Get(t.Context(), gvkCNP, ns, "allow-dns"); err != nil || got == nil {
		t.Errorf("get: %v, %v", got, err)
	}
	for _, kind := range [][2]string{{"external-secrets.io/v1", "SecretStore"}, {"external-secrets.io/v1", "ExternalSecret"}, {"karpenter.sh/v1", "NodePool"},
		{"agenticteammates.stainedhead.io/v1alpha1", "Teammate"}, {"snapshot.storage.k8s.io/v1", "VolumeSnapshot"}} {
		ns2 := ns
		if kind[1] == "NodePool" {
			ns2 = ""
		}
		o := render.Managed(render.New(kind[0], kind[1], ns2, "probe"), owner)
		o.Object["spec"] = map[string]any{"any": "field"}
		if _, err := c.Apply(t.Context(), o); err != nil {
			t.Errorf("%s: %v", kind[1], err)
		}
	}
}

// M1-F2: a second replica takes over within 30 s of the leader dying without releasing its lease.
func TestLeaderFailover(t *testing.T) {
	e := envtesting.Require(t, env)
	envtesting.EnsureNamespace(t, e.Config, "afs-system", nil)
	cs := envtesting.Clientset(t, e.Config)
	mk := func(id string) *k8s.Elector {
		el, err := k8s.NewElector(k8s.ElectorConfig{Client: cs, Namespace: "afs-system", Name: "afs-reconciler-leader", Identity: id})
		if err != nil {
			t.Fatal(err)
		}
		return el
	}
	a, b := mk("replica-a"), mk("replica-b")
	led := make(chan string, 4)
	ctxA, cancelA := context.WithCancel(t.Context())
	ctxB, cancelB := context.WithCancel(t.Context())
	defer cancelB()
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		_ = a.Run(ctxA, func(ctx context.Context) { led <- "a"; <-ctx.Done() })
	}()
	select {
	case who := <-led:
		if who != "a" {
			t.Fatalf("leader = %s", who)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("no leader elected")
	}
	if !a.IsLeader() {
		t.Error("IsLeader false for the leader")
	}
	go func() { _ = b.Run(ctxB, func(ctx context.Context) { led <- "b"; <-ctx.Done() }) }()
	time.Sleep(3 * time.Second)
	if b.IsLeader() {
		t.Fatal("two leaders")
	}

	// The leader dies (no release): b leads once the lease expires.
	killed := time.Now()
	cancelA()
	<-doneA
	select {
	case who := <-led:
		if who != "b" {
			t.Fatalf("new leader = %s", who)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no failover within 30 s")
	}
	if took := time.Since(killed); took > 30*time.Second {
		t.Errorf("failover took %v", took)
	}
	if !b.IsLeader() || a.IsLeader() {
		t.Errorf("leader flags: a=%v b=%v", a.IsLeader(), b.IsLeader())
	}
}

type drifts struct {
	mu   sync.Mutex
	seen []string
}

func (d *drifts) add(ref engine.Ref, object string) {
	d.mu.Lock()
	d.seen = append(d.seen, fmt.Sprintf("%s|%s", ref, object))
	d.mu.Unlock()
}

func (d *drifts) list() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// M1-F6: edits and deletes by others are reported with their owner; the reconciler's own writes are not.
func TestDriftWatcher(t *testing.T) {
	c := newClient(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ns := "f6-drift"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := &drifts{}
	w, err := k8s.NewWatcher(c, d.add, nil, gvkConfigMap, gvkNamespace)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.Run(ctx) }()
	time.Sleep(2 * time.Second) // informers synced

	if _, err := c.Apply(ctx, configMap(ns, "watched", "one")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(ctx, configMap(ns, "watched", "two")); err != nil { // our own update
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if got := d.list(); len(got) != 0 {
		t.Fatalf("own writes reported as drift: %v", got)
	}

	cs := envtesting.Clientset(t, env.Config)
	cm, _ := cs.CoreV1().ConfigMaps(ns).Get(ctx, "watched", metav1.GetOptions{})
	cm.Data["key"] = "tampered"
	if _, err := cs.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{FieldManager: "kubectl-edit"}); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("team/%s|ConfigMap/%s/watched", owner.ID, ns)
	envtesting.Eventually(t, 10*time.Second, "edit reported", func() bool { return contains(d.list(), want) })

	if err := cs.CoreV1().ConfigMaps(ns).Delete(ctx, "watched", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	envtesting.Eventually(t, 10*time.Second, "delete reported", func() bool { return len(d.list()) >= 2 })

	// Our own delete is not drift.
	if _, err := c.Apply(ctx, configMap(ns, "ours", "x")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	n := len(d.list())
	if err := c.Delete(ctx, gvkConfigMap, ns, "ours"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if len(d.list()) != n {
		t.Errorf("own delete reported: %v", d.list())
	}

	// Objects without the managed label are never reported.
	_, _ = cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "stranger"}, Data: map[string]string{"a": "b"}}, metav1.CreateOptions{})
	_ = cs.CoreV1().ConfigMaps(ns).Delete(ctx, "stranger", metav1.DeleteOptions{})
	time.Sleep(time.Second)
	if len(d.list()) != n {
		t.Errorf("unmanaged object reported: %v", d.list())
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

package podcache_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/podcache"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
)

func ns(name, team string, teamNS bool) *corev1.Namespace {
	l := map[string]string{}
	if team != "" {
		l[render.LabelTeamID] = team
	}
	if teamNS {
		l[render.LabelTeamNamespace] = "true"
	}
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
}

func pod(namespace, name, ip string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID("uid-" + name), Labels: labels},
		Spec:       corev1.PodSpec{ServiceAccountName: "sa-" + name},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip, PodIPs: []corev1.PodIP{{IP: ip}}},
	}
}

func start(t *testing.T, cfg podcache.Config) *podcache.Cache {
	t.Helper()
	c, err := podcache.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, "ready", c.Ready)
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

func TestLookupResolvesTeamAgentAndPool(t *testing.T) {
	cs := fake.NewClientset(
		ns("afs-t-alpha", "team_A", true), ns("kube-system", "", false), ns("afs-t-beta", "team_B", true),
		pod("afs-t-alpha", "agent-bot-0", "10.0.0.1", map[string]string{render.LabelTeamID: "team_A", render.LabelAgentID: "agt_1"}),
		pod("afs-t-alpha", "pool-w-xyz", "10.0.0.2", map[string]string{render.LabelPoolID: "pool_1"}),
		pod("kube-system", "coredns", "10.0.9.9", nil),
		pod("afs-t-beta", "agent-x-0", "10.0.1.1", map[string]string{render.LabelAgentID: "agt_2"}),
	)
	c := start(t, podcache.Config{Client: cs})
	waitFor(t, "pods", func() bool { return c.Len() >= 4 })

	got, ok := c.Lookup(ip("10.0.0.1"))
	if !ok || got.TeamID != "team_A" || got.AgentID != "agt_1" || got.PoolID != "" || got.Namespace != "afs-t-alpha" || got.Name != "agent-bot-0" || got.UID != "uid-agent-bot-0" || got.ServiceAccount != "sa-agent-bot-0" {
		t.Fatalf("agent pod = %+v ok %v", got, ok)
	}
	got, ok = c.Lookup(ip("10.0.0.2"))
	if !ok || got.PoolID != "pool_1" || got.AgentID != "" || got.Name != "pool-w-xyz" || got.TeamID != "team_A" {
		t.Fatalf("worker pod = %+v ok %v", got, ok)
	}
	if got, ok = c.Lookup(ip("10.0.1.1")); !ok || got.TeamID != "team_B" {
		t.Fatalf("team B pod = %+v ok %v", got, ok)
	}
	for name, a := range map[string]string{"pod outside team namespaces": "10.0.9.9", "unknown ip": "10.5.5.5"} {
		if _, ok := c.Lookup(ip(a)); ok {
			t.Errorf("%s resolved", name)
		}
	}
}

func TestPodWithAnotherTeamsLabelIsIgnored(t *testing.T) {
	cs := fake.NewClientset(ns("afs-t-alpha", "team_A", true),
		pod("afs-t-alpha", "liar", "10.0.0.3", map[string]string{render.LabelTeamID: "team_B", render.LabelAgentID: "agt_9"}))
	c := start(t, podcache.Config{Client: cs})
	waitFor(t, "pod", func() bool { return c.Len() >= 1 })
	if got, ok := c.Lookup(ip("10.0.0.3")); ok {
		t.Fatalf("a pod labelled for team B in team A's namespace resolved: %+v", got)
	}
}

func TestPodsThatCannotBeIdentifiedByIPAreSkipped(t *testing.T) {
	host := pod("afs-t-alpha", "host", "10.9.0.1", map[string]string{render.LabelAgentID: "a"})
	host.Spec.HostNetwork = true
	done := pod("afs-t-alpha", "done", "10.9.0.2", map[string]string{render.LabelAgentID: "a"})
	done.Status.Phase = corev1.PodSucceeded
	cs := fake.NewClientset(ns("afs-t-alpha", "team_A", true), host, done,
		pod("afs-t-alpha", "ok", "10.9.0.3", map[string]string{render.LabelAgentID: "a"}))
	c := start(t, podcache.Config{Client: cs})
	waitFor(t, "ok pod", func() bool { _, ok := c.Lookup(ip("10.9.0.3")); return ok })
	for _, a := range []string{"10.9.0.1", "10.9.0.2"} {
		if _, ok := c.Lookup(ip(a)); ok {
			t.Errorf("%s resolved", a)
		}
	}
}

func TestDeletingAPodOrTheNamespaceMakesItsIPUnknown(t *testing.T) {
	cs := fake.NewClientset(ns("afs-t-alpha", "team_A", true), ns("afs-t-gone", "team_G", true),
		pod("afs-t-alpha", "a", "10.0.0.1", map[string]string{render.LabelAgentID: "agt_1"}),
		pod("afs-t-gone", "g", "10.0.7.1", map[string]string{render.LabelAgentID: "agt_7"}))
	c := start(t, podcache.Config{Client: cs})
	waitFor(t, "pods", func() bool { _, a := c.Lookup(ip("10.0.0.1")); _, b := c.Lookup(ip("10.0.7.1")); return a && b })

	if err := cs.CoreV1().Pods("afs-t-alpha").Delete(t.Context(), "a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pod removal", func() bool { _, ok := c.Lookup(ip("10.0.0.1")); return !ok })
	// The team was deleted: its namespace is gone before its pods' delete events may arrive.
	if err := cs.CoreV1().Namespaces().Delete(t.Context(), "afs-t-gone", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "namespace removal", func() bool { _, ok := c.Lookup(ip("10.0.7.1")); return !ok })
}

func TestIPReuseKeepsTheNewPod(t *testing.T) {
	cs := fake.NewClientset(ns("afs-t-alpha", "team_A", true),
		pod("afs-t-alpha", "old", "10.0.0.1", map[string]string{render.LabelAgentID: "agt_old"}))
	c := start(t, podcache.Config{Client: cs})
	waitFor(t, "old", func() bool { _, ok := c.Lookup(ip("10.0.0.1")); return ok })
	if _, err := cs.CoreV1().Pods("afs-t-alpha").Create(t.Context(), pod("afs-t-alpha", "new", "10.0.0.1", map[string]string{render.LabelAgentID: "agt_new"}), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "new owns the ip", func() bool { p, ok := c.Lookup(ip("10.0.0.1")); return ok && p.AgentID == "agt_new" })
	if err := cs.CoreV1().Pods("afs-t-alpha").Delete(t.Context(), "old", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if p, ok := c.Lookup(ip("10.0.0.1")); !ok || p.AgentID != "agt_new" {
		t.Fatalf("the old pod's delete removed the new pod's IP: %+v %v", p, ok)
	}
}

func TestNotReadyBeforeTheFirstList(t *testing.T) {
	c, err := podcache.New(podcache.Config{Client: fake.NewClientset()})
	if err != nil {
		t.Fatal(err)
	}
	if c.Ready() {
		t.Fatal("ready before Run")
	}
	if _, err := podcache.New(podcache.Config{}); err == nil {
		t.Fatal("a cache without a client")
	}
}

func endpoint(namespace, name, addr string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2", "kind": "CiliumEndpoint",
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"status":   map[string]any{"networking": map[string]any{"addressing": []any{map[string]any{"ipv4": addr}}}},
	}}
	return u
}

func TestCiliumEndpointMustAgreeWithThePod(t *testing.T) {
	cs := fake.NewClientset(ns("afs-t-alpha", "team_A", true),
		pod("afs-t-alpha", "a", "10.0.0.1", map[string]string{render.LabelAgentID: "agt_1"}),
		pod("afs-t-alpha", "b", "10.0.0.2", map[string]string{render.LabelAgentID: "agt_2"}),
		pod("afs-t-alpha", "c", "10.0.0.3", map[string]string{render.LabelAgentID: "agt_3"}))
	scheme := runtime.NewScheme()
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{podcache.CiliumEndpointGVR: "CiliumEndpointList"},
		endpoint("afs-t-alpha", "a", "10.0.0.1"),
		endpoint("afs-t-alpha", "other-name", "10.0.0.2"), // Cilium says this IP is another endpoint
	)
	c := start(t, podcache.Config{Client: cs, Dynamic: dyn})
	waitFor(t, "a", func() bool { _, ok := c.Lookup(ip("10.0.0.1")); return ok })
	if _, ok := c.Lookup(ip("10.0.0.2")); ok {
		t.Fatal("an IP whose Cilium endpoint names another pod resolved")
	}
	if _, ok := c.Lookup(ip("10.0.0.3")); ok {
		t.Fatal("an IP with no Cilium endpoint resolved")
	}
}

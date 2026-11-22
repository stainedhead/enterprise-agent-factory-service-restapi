package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/activity"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
)

func podObj(ns, name string, labels map[string]string, phase corev1.PodPhase, ready bool, ip string) *unstructured.Unstructured {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	p := &corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Status: corev1.PodStatus{Phase: phase, PodIP: ip, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}}}
	m, _ := runtime.DefaultUnstructuredConverter.ToUnstructured(p)
	return &unstructured.Unstructured{Object: m}
}

func metricsObj(ns, name string, usage ...map[string]any) *unstructured.Unstructured {
	cs := make([]any, len(usage))
	for i, u := range usage {
		cs[i] = map[string]any{"name": "c" + strconv.Itoa(i), "usage": u}
	}
	return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "metrics.k8s.io/v1beta1", "kind": "PodMetrics",
		"metadata": map[string]any{"namespace": ns, "name": name}, "containers": cs}}
}

func newMetrics(objs ...runtime.Object) (*PodMetrics, *dynfake.FakeDynamicClient) {
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		podGVR: "PodList", podMetrics: "PodMetricsList"}, pods(objs)...)
	// The fake tracker files objects by a guessed plural; metrics are created through the client under
	// the resource the adapter reads.
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetKind() == "PodMetrics" {
			if _, err := dyn.Resource(podMetrics).Namespace(u.GetNamespace()).Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
				panic(err)
			}
		}
	}
	return NewPodMetrics(dyn, nil), dyn
}

// M3-I1: the pods of agents and pool workers with CPU summed over containers and memory in MiB.
func TestPodMetricsPods(t *testing.T) {
	agent, pool := map[string]string{traffic.LabelAgentID: "agt_1"}, map[string]string{traffic.LabelPoolID: "pool_1"}
	m, _ := newMetrics(
		podObj("ns", "agent-0", agent, corev1.PodRunning, true, "10.0.0.1"),
		podObj("ns", "w-1", pool, corev1.PodRunning, false, "10.0.0.2"),
		podObj("ns", "failed", pool, corev1.PodFailed, false, ""),
		podObj("ns", "other", map[string]string{"app": "x"}, corev1.PodRunning, true, ""),
		podObj("other-ns", "ignored", agent, corev1.PodRunning, true, ""),
		metricsObj("ns", "agent-0", map[string]any{"cpu": "120m", "memory": "100Mi"}, map[string]any{"cpu": "30m", "memory": "28Mi"}),
		metricsObj("ns", "w-1", map[string]any{"cpu": "2", "memory": "1Gi"}),
	)
	got, err := m.Pods(t.Context(), []string{"ns"})
	if err != nil || len(got) != 3 {
		t.Fatalf("%v %+v", err, got)
	}
	by := map[string]activity.Pod{}
	for _, p := range got {
		by[p.Name] = p
	}
	a := by["agent-0"]
	if a.AgentID != "agt_1" || a.PoolID != "" || !a.Ready || a.Failed || a.IP != "10.0.0.1" || *a.Millicores != 150 || *a.MemMiB != 128 {
		t.Errorf("agent = %+v", a)
	}
	if w := by["w-1"]; w.PoolID != "pool_1" || w.Ready || *w.Millicores != 2000 || *w.MemMiB != 1024 {
		t.Errorf("worker = %+v", w)
	}
	if f := by["failed"]; !f.Failed || f.Ready || f.Millicores != nil {
		t.Errorf("failed = %+v", f)
	}
}

func TestPodMetricsWithoutMetricsAPI(t *testing.T) {
	m, dyn := newMetrics(podObj("ns", "agent-0", map[string]string{traffic.LabelAgentID: "agt_1"}, corev1.PodRunning, true, ""))
	dyn.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Group == "metrics.k8s.io" {
			return true, nil, apierrors.NewNotFound(podMetrics.GroupResource(), "")
		}
		return false, nil, nil
	})
	for range 2 {
		got, err := m.Pods(t.Context(), []string{"ns"})
		if err != nil || len(got) != 1 || got[0].Millicores != nil {
			t.Fatalf("%v %+v", err, got)
		}
	}
	// Any other metrics error, and a pod list error, are errors.
	m2, dyn2 := newMetrics()
	dyn2.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom")
	})
	if _, err := m2.Pods(t.Context(), []string{"ns"}); err == nil {
		t.Error("want the metrics error")
	}
	m3, dyn3 := newMetrics()
	dyn3.PrependReactor("list", "pods", func(a clienttesting.Action) (bool, runtime.Object, error) {
		if a.GetResource().Group == "" {
			return true, nil, errors.New("boom")
		}
		return false, nil, nil
	})
	if _, err := m3.Pods(t.Context(), []string{"ns"}); err == nil {
		t.Error("want the pod error")
	}
}

func statusServer(t *testing.T, h http.HandlerFunc) (ip string, port int) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	host, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return host, n
}

// OQ-7: the status endpoint, where there is one, says busy or idle; anything else leaves the decision
// to CPU and model calls.
func TestAgentStatus(t *testing.T) {
	reply := func(code int, body any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != StatusPath {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(code)
			if s, ok := body.(string); ok {
				_, _ = w.Write([]byte(s))
				return
			}
			_ = json.NewEncoder(w).Encode(body)
		}
	}
	tests := []struct {
		name    string
		h       http.HandlerFunc
		want    activity.StatusReport
		wantErr error
	}{
		{"busy", reply(200, map[string]string{"state": "busy", "activity": "reviewing PR 482"}), activity.StatusReport{Busy: true, Note: "reviewing PR 482"}, nil},
		{"idle", reply(200, map[string]string{"state": "idle"}), activity.StatusReport{}, nil},
		{"unknown state", reply(200, map[string]string{"state": "dreaming"}), activity.StatusReport{}, activity.ErrNoStatus},
		{"not json", reply(200, "<html>"), activity.StatusReport{}, activity.ErrNoStatus},
		{"500", reply(500, "x"), activity.StatusReport{}, activity.ErrNoStatus},
		{"redirect is not followed", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
		}, activity.StatusReport{}, activity.ErrNoStatus},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ip, port := statusServer(t, tc.h)
			got, err := NewAgentStatus(nil, port).Report(t.Context(), activity.Pod{Name: "p", IP: ip})
			if !errors.Is(err, tc.wantErr) || got != tc.want {
				t.Errorf("%+v %v", got, err)
			}
		})
	}
	// No pod address, nothing listening, and a slow pod.
	if _, err := NewAgentStatus(nil, 0).Report(t.Context(), activity.Pod{Name: "p"}); !errors.Is(err, activity.ErrNoStatus) {
		t.Errorf("no ip: %v", err)
	}
	l, _ := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	if _, err := NewAgentStatus(nil, port).Report(t.Context(), activity.Pod{Name: "p", IP: "127.0.0.1"}); !errors.Is(err, activity.ErrNoStatus) {
		t.Errorf("closed port: %v", err)
	}
	ip, port2 := statusServer(t, func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) })
	c := &http.Client{Timeout: 50 * time.Millisecond}
	if _, err := NewAgentStatus(c, port2).Report(context.Background(), activity.Pod{Name: "p", IP: ip}); !errors.Is(err, activity.ErrNoStatus) {
		t.Errorf("slow pod: %v", err)
	}
	if NewAgentStatus(nil, 0).port != DefaultStatusPort {
		t.Error("default port")
	}
}

func pods(objs []runtime.Object) []runtime.Object {
	var out []runtime.Object
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetKind() == "PodMetrics" {
			continue
		}
		out = append(out, o)
	}
	return out
}

package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/activity"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
)

var (
	podGVR     = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	podMetrics = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}
)

// PodMetrics implements activity.PodSource (RC-7 activity): the agent and pool worker pods of the team
// namespaces, with CPU and memory from the metrics API (metrics-server). A cluster without a metrics
// API gives pods without values, and the collector falls back to model calls and the status endpoint.
type PodMetrics struct {
	dyn dynamic.Interface
	log *slog.Logger

	warn sync.Once
}

// NewPodMetrics returns a PodMetrics reading through dyn.
func NewPodMetrics(dyn dynamic.Interface, log *slog.Logger) *PodMetrics {
	if log == nil {
		log = slog.Default()
	}
	return &PodMetrics{dyn: dyn, log: log}
}

var _ activity.PodSource = (*PodMetrics)(nil)

// agentPodSelectors select the pods of agents and of pool workers.
var agentPodSelectors = []string{traffic.LabelAgentID, traffic.LabelPoolID}

// Pods implements activity.PodSource.
func (m *PodMetrics) Pods(ctx context.Context, namespaces []string) ([]activity.Pod, error) {
	var out []activity.Pod
	for _, ns := range namespaces {
		usage, err := m.usage(ctx, ns)
		if err != nil {
			return nil, err
		}
		for _, sel := range agentPodSelectors {
			list, err := m.dyn.Resource(podGVR).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
			if err != nil {
				return nil, fmt.Errorf("k8s: list pods in %s: %w", ns, wrap(err))
			}
			for i := range list.Items {
				p, err := podOf(&list.Items[i])
				if err != nil {
					return nil, err
				}
				if u, ok := usage[p.Name]; ok {
					p.Millicores, p.MemMiB = u.cpu, u.mem
				}
				out = append(out, p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name })
	return out, nil
}

func podOf(u *unstructured.Unstructured) (activity.Pod, error) {
	var pod corev1.Pod
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &pod); err != nil {
		return activity.Pod{}, fmt.Errorf("k8s: read pod %s: %w", u.GetName(), err)
	}
	p := activity.Pod{Namespace: pod.Namespace, Name: pod.Name, AgentID: pod.Labels[traffic.LabelAgentID], PoolID: pod.Labels[traffic.LabelPoolID],
		IP: pod.Status.PodIP, Failed: pod.Status.Phase == corev1.PodFailed}
	if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp == nil {
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				p.Ready = true
			}
		}
	}
	return p, nil
}

type podUsage struct{ cpu, mem *int }

// usage returns the CPU (millicores) and memory (MiB) of each pod of the namespace, summed over its
// containers. Without a metrics API it returns nothing, once with a warning.
func (m *PodMetrics) usage(ctx context.Context, ns string) (map[string]podUsage, error) {
	list, err := m.dyn.Resource(podMetrics).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsServiceUnavailable(err) {
			m.warn.Do(func() { m.log.Warn("the metrics API is not available: CPU is not sampled", slog.Any("err", err)) })
			return nil, nil
		}
		return nil, fmt.Errorf("k8s: list pod metrics in %s: %w", ns, wrap(err))
	}
	out := map[string]podUsage{}
	for i := range list.Items {
		item := &list.Items[i]
		containers, _, _ := unstructured.NestedSlice(item.Object, "containers")
		var cpu, mem int64
		ok := false
		for _, c := range containers {
			cm, isMap := c.(map[string]any)
			if !isMap {
				continue
			}
			usage, _ := cm["usage"].(map[string]any)
			if q, err := resource.ParseQuantity(str(usage["cpu"])); err == nil {
				cpu += q.MilliValue()
				ok = true
			}
			if q, err := resource.ParseQuantity(str(usage["memory"])); err == nil {
				mem += q.Value() / (1 << 20)
			}
		}
		if ok {
			c, mm := int(cpu), int(mem)
			out[item.GetName()] = podUsage{cpu: &c, mem: &mm}
		}
	}
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

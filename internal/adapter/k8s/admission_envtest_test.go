//go:build envtest

package k8s_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/envtesting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/admission"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

const (
	admNS      = "afs-t-adm"
	admTeam    = "team_01JD0000000000000000000013"
	otherTeam  = "team_01JD0000000000000000000014"
	reconciler = "afs-reconciler-test"
)

var (
	gvkVAP  = schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicy"}
	gvkVAPB = schema.GroupVersionKind{Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicyBinding"}
)

func asUser(t *testing.T, user string) kubernetes.Interface {
	t.Helper()
	// system:masters passes RBAC, so the answers below come from admission alone.
	return envtesting.Clientset(t, envtesting.As(env.Config, user, "system:masters"))
}

func installAdmission(t *testing.T, c *k8s.Client) {
	t.Helper()
	t.Cleanup(func() { removeAdmission(c) })
	envtesting.EnsureNamespace(t, env.Config, admNS, map[string]string{render.LabelTeamNamespace: "true", render.LabelTeamID: admTeam})
	envtesting.EnsureNamespace(t, env.Config, "adm-free", nil)
	if _, err := k8s.NewAdmissionCluster(c).ApplyAdmission(t.Context(), admission.Spec{ReconcilerUsers: []string{reconciler}}); err != nil {
		t.Fatal(err)
	}
}

// waitEnforced polls until every policy refuses a probe: the API server compiles a new policy
// asynchronously.
func waitEnforced(t *testing.T, agent kubernetes.Interface) {
	t.Helper()
	yes := true
	n := 0
	probes := map[string]func(i int) error{
		"networkpolicy": func(i int) error {
			_, err := agent.NetworkingV1().NetworkPolicies(admNS).Create(t.Context(), &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("probe-%d", i)}}, metav1.CreateOptions{})
			return err
		},
		"service": func(i int) error {
			_, err := agent.CoreV1().Services(admNS).Create(t.Context(), &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("probe-%d", i)},
				Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Port: 80}}}}, metav1.CreateOptions{})
			return err
		},
		"host": func(i int) error {
			_, err := agent.CoreV1().Pods(admNS).Create(t.Context(), pod(fmt.Sprintf("probe-%d", i), func(s *corev1.PodSpec) { s.HostNetwork = yes }), metav1.CreateOptions{})
			return err
		},
		"node": func(i int) error {
			_, err := agent.CoreV1().Pods(admNS).Create(t.Context(), pod(fmt.Sprintf("probe-n-%d", i), func(s *corev1.PodSpec) {
				s.NodeSelector = map[string]string{render.DedicatedNodeKey: "someone-else"}
			}), metav1.CreateOptions{})
			return err
		},
	}
	for name, probe := range probes {
		envtesting.Eventually(t, 60*time.Second, name+" policy enforced", func() bool {
			n++
			return apierrors.IsForbidden(probe(n)) || apierrors.IsInvalid(probe(n))
		})
	}
}

// removeAdmission deletes the policies afterwards: they are cluster-wide and would refuse the other
// suites' network policies, which are applied as an administrator.
func removeAdmission(c *k8s.Client) {
	for _, n := range render.AdmissionPolicyNames {
		_ = c.Delete(context.Background(), gvkVAPB, "", n+"-binding")
		_ = c.Delete(context.Background(), gvkVAP, "", n)
	}
}

func pod(name string, mut func(*corev1.PodSpec)) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}
	if mut != nil {
		mut(&p.Spec)
	}
	return p
}

// M1-H1: the policies refuse what RC-19 lists inside team namespaces, allow the reconciler its own
// objects, and leave other namespaces alone.
func TestAdmissionPolicies(t *testing.T) {
	c := newClient(t)
	installAdmission(t, c)
	agent := asUser(t, "system:serviceaccount:"+admNS+":agent")
	rec := asUser(t, reconciler)
	ctx := t.Context()
	yes := true

	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "allow-all"}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{}, PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}}
	waitEnforced(t, agent)
	if _, err := rec.NetworkingV1().NetworkPolicies(admNS).Create(ctx, np, metav1.CreateOptions{}); err != nil {
		t.Errorf("reconciler must manage network policies: %v", err)
	}
	if err := agent.NetworkingV1().NetworkPolicies(admNS).Delete(ctx, "allow-all", metav1.DeleteOptions{}); err == nil {
		t.Error("agent deleted a network policy")
	}
	if _, err := agent.NetworkingV1().NetworkPolicies("adm-free").Create(ctx, np, metav1.CreateOptions{}); err != nil {
		t.Errorf("namespaces that are not team namespaces are unaffected: %v", err)
	}

	svc := func(name string, typ corev1.ServiceType) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.ServiceSpec{Type: typ, Ports: []corev1.ServicePort{{Port: 80}}}}
	}
	for _, typ := range []corev1.ServiceType{corev1.ServiceTypeLoadBalancer, corev1.ServiceTypeNodePort} {
		if _, err := agent.CoreV1().Services(admNS).Create(ctx, svc("bad-"+string(typ), typ), metav1.CreateOptions{}); err == nil {
			t.Errorf("service type %s allowed", typ)
		}
	}
	if _, err := agent.CoreV1().Services(admNS).Create(ctx, svc("ok", corev1.ServiceTypeClusterIP), metav1.CreateOptions{}); err != nil {
		t.Errorf("ClusterIP refused: %v", err)
	}

	pods := []struct {
		name string
		mut  func(*corev1.PodSpec)
		deny bool
	}{
		{"plain", nil, false},
		{"hostnet", func(s *corev1.PodSpec) { s.HostNetwork = true }, true},
		{"hostpid", func(s *corev1.PodSpec) { s.HostPID = true }, true},
		{"hostipc", func(s *corev1.PodSpec) { s.HostIPC = true }, true},
		{"hostpath", func(s *corev1.PodSpec) {
			s.Volumes = []corev1.Volume{{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
		}, true},
		{"privileged", func(s *corev1.PodSpec) { s.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: &yes} }, true},
		{"privileged-init", func(s *corev1.PodSpec) {
			s.InitContainers = []corev1.Container{{Name: "i", Image: "busybox", SecurityContext: &corev1.SecurityContext{Privileged: &yes}}}
		}, true},
		{"own-team-toleration", func(s *corev1.PodSpec) {
			s.Tolerations = []corev1.Toleration{{Key: render.DedicatedNodeKey, Operator: corev1.TolerationOpEqual, Value: admTeam}}
		}, false},
		{"other-team-toleration", func(s *corev1.PodSpec) {
			s.Tolerations = []corev1.Toleration{{Key: render.DedicatedNodeKey, Operator: corev1.TolerationOpEqual, Value: otherTeam}}
		}, true},
		{"exists-toleration", func(s *corev1.PodSpec) {
			s.Tolerations = []corev1.Toleration{{Key: render.DedicatedNodeKey, Operator: corev1.TolerationOpExists}}
		}, true},
		{"other-team-selector", func(s *corev1.PodSpec) { s.NodeSelector = map[string]string{render.DedicatedNodeKey: otherTeam} }, true},
		{"own-team-selector", func(s *corev1.PodSpec) { s.NodeSelector = map[string]string{render.DedicatedNodeKey: admTeam} }, false},
	}
	for _, tc := range pods {
		_, err := agent.CoreV1().Pods(admNS).Create(ctx, pod(tc.name, tc.mut), metav1.CreateOptions{})
		if tc.deny && err == nil {
			t.Errorf("pod %s allowed", tc.name)
		}
		if !tc.deny && err != nil {
			t.Errorf("pod %s refused: %v", tc.name, err)
		}
	}
	// Workload templates are held to the same rules.
	ss := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "ss"}, Spec: appsv1.StatefulSetSpec{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"a": "b"}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"a": "b"}}, Spec: pod("x", func(s *corev1.PodSpec) { s.HostNetwork = true }).Spec},
	}}
	if _, err := agent.AppsV1().StatefulSets(admNS).Create(ctx, ss, metav1.CreateOptions{}); err == nil {
		t.Error("StatefulSet with hostNetwork allowed")
	}
	ss.Spec.Template.Spec.HostNetwork = false
	if _, err := agent.AppsV1().StatefulSets(admNS).Create(ctx, ss, metav1.CreateOptions{}); err != nil {
		t.Errorf("plain StatefulSet refused: %v", err)
	}
	if _, err := agent.CoreV1().Pods("adm-free").Create(ctx, pod("free", func(s *corev1.PodSpec) { s.HostNetwork = true }), metav1.CreateOptions{}); err != nil {
		t.Errorf("hostNetwork outside team namespaces is not ours to refuse: %v", err)
	}

	// AFS labels: only the reconciler changes them.
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "labelled", Labels: map[string]string{render.LabelTeamID: admTeam, render.LabelManagedBy: render.ManagedByValue, "x": "1"}}}
	if _, err := rec.CoreV1().ConfigMaps(admNS).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	update := func(cs kubernetes.Interface, mut func(map[string]string)) error {
		cur, err := cs.CoreV1().ConfigMaps(admNS).Get(ctx, "labelled", metav1.GetOptions{})
		if err != nil {
			return err
		}
		mut(cur.Labels)
		_, err = cs.CoreV1().ConfigMaps(admNS).Update(ctx, cur, metav1.UpdateOptions{})
		return err
	}
	for name, mut := range map[string]func(map[string]string){
		"change":  func(l map[string]string) { l[render.LabelTeamID] = otherTeam },
		"remove":  func(l map[string]string) { delete(l, render.LabelManagedBy) },
		"add afs": func(l map[string]string) { l[render.LabelResourceKind] = "team" },
	} {
		if err := update(agent, mut); err == nil {
			t.Errorf("agent could %s an AFS label", name)
		}
	}
	if err := update(agent, func(l map[string]string) { l["x"] = "2" }); err != nil {
		t.Errorf("a non-AFS label change was refused: %v", err)
	}
	if err := update(rec, func(l map[string]string) { l[render.LabelResourceKind] = "team" }); err != nil {
		t.Errorf("reconciler could not change an AFS label: %v", err)
	}
}

// M1-H2: a deleted policy or binding comes back within 60 s with one drift.corrected record each.
func TestAdmissionPolicyRestored(t *testing.T) {
	c := newClient(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { removeAdmission(c) })
	t.Cleanup(cancel) // runs first: the engine must stop before the policies are removed
	ctl, err := admission.NewController(k8s.NewAdmissionCluster(c), admission.Spec{ReconcilerUsers: []string{reconciler}})
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := engine.NewRegistry(engine.Route{Table: "teams", Targets: []engine.Target{{Kind: engine.KindTeam, Self: true}}})
	rec := &memDrift{}
	e, err := engine.New(engine.Config{Feed: idleFeed{}, Lister: listRefs{}, Registry: reg, Controllers: []engine.Controller{ctl},
		Status: &memStatus{}, Drift: rec, Singletons: []engine.Ref{admission.Ref}, PollInterval: time.Hour, ResyncInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = e.Run(ctx) }()
	name := render.AdmissionPolicyNames[0]
	envtesting.Eventually(t, 60*time.Second, "policies installed", func() bool {
		o, _ := c.Get(ctx, gvkVAPB, "", name+"-binding")
		return o != nil
	})
	w, err := k8s.NewWatcher(c, func(r engine.Ref, object string) { e.NoteDrift(r, object) }, nil, gvkVAP, gvkVAPB)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = w.Run(ctx) }()
	time.Sleep(2 * time.Second)

	dyn := c.Dynamic()
	gvr := func(res string) schema.GroupVersionResource {
		return schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: res}
	}
	if err := dyn.Resource(gvr("validatingadmissionpolicybindings")).Delete(ctx, name+"-binding", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := dyn.Resource(gvr("validatingadmissionpolicies")).Delete(ctx, render.AdmissionPolicyNames[1], metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	envtesting.Eventually(t, 60*time.Second, "restored", func() bool {
		b, _ := c.Get(ctx, gvkVAPB, "", name+"-binding")
		p, _ := c.Get(ctx, gvkVAP, "", render.AdmissionPolicyNames[1])
		return b != nil && p != nil
	})
	envtesting.Eventually(t, 30*time.Second, "drift records", func() bool { return len(rec.objects()) >= 2 })
	seen := map[string]int{}
	for _, o := range rec.objects() {
		seen[o]++
	}
	for o, n := range seen {
		if n != 1 {
			t.Errorf("%s recorded %d times", o, n)
		}
	}
}

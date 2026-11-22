//go:build envtest

package k8s_test

import (
	"context"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/envtesting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/kubectl"
)

const (
	kcTeam = "team_01JD0000000000000000000061"
	kcRole = "arn:aws:iam::111122223333:role/afs-viewers-okta"
)

type liveKubectl struct {
	mu sync.Mutex
	t  kubectl.Team
}

func (s *liveKubectl) set(f func(*kubectl.Team)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.t)
}

func (s *liveKubectl) LoadTeam(context.Context, string) (kubectl.Team, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.t, nil
}

func (s *liveKubectl) ListViewerTeams(context.Context) ([]kubectl.Team, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.t.ViewerKubectl {
		return []kubectl.Team{s.t}, nil
	}
	return nil, nil
}

type recordedEntries struct {
	mu   sync.Mutex
	want map[string]string
}

func (r *recordedEntries) SyncViewerEntries(_ context.Context, want map[string]string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.want = want
	return nil, nil
}

// M5-E2 [e2e on envtest]: with the flag on a viewer who arrives through the federated role's group can
// read the namespace's workloads and nothing else, and is refused secrets, exec, attach, port-forward and
// every write; switching the flag off takes the access away on the next pass, well inside 60 s. The
// namespace's RBAC is the real kube-apiserver authorizer; the access entry is the fake the EKS adapter's
// own tests cover.
func TestViewerKubectlIsReadOnlyAndFollowsTheFlag(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	const ns = "afs-t-kc"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	envtesting.EnsureNamespace(t, env.Config, "afs-t-other", nil)
	admin := envtesting.Clientset(t, env.Config)

	src := &liveKubectl{t: kubectl.Team{ID: kcTeam, Slug: "kc", Namespace: ns, State: "active", ViewerKubectl: true,
		Providers: []kubectl.Provider{{ID: "idp_01JD0000000000000000000001", IsDefault: true}}}}
	entries := &recordedEntries{}
	ctl, err := kubectl.NewController(kubectl.Config{Source: src, Cluster: k8s.NewKubectlCluster(c), Entries: entries,
		ProviderRoles: map[string]string{kubectl.DefaultProvider: kcRole}})
	if err != nil {
		t.Fatal(err)
	}
	reconcile := func() {
		t.Helper()
		if _, err := ctl.Reconcile(ctx, engine.NewRef(kubectl.Kind, kcTeam, kcTeam)); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}

	// A workload and a secret for the viewer to (not) reach, made by the admin.
	if _, err := admin.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s"}, StringData: map[string]string{"k": "v"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	one := int32(1)
	labels := map[string]string{"app": "x"}
	if _, err := admin.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "d"},
		Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example.com/x:1"}}}}}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	group := kubectl.GroupFor(kcRole)
	viewer := kubernetes.NewForConfigOrDie(envtesting.As(env.Config, "viewer@example.com", group))
	reconcile()
	if entries.want[kcRole] != group || len(entries.want) != 1 {
		t.Fatalf("entry wanted = %v", entries.want)
	}
	// Reads in the namespace work (the authorizer's cache needs a moment to see a new binding).
	envtesting.Eventually(t, 10*time.Second, "the viewer to list deployments", func() bool {
		_, err := viewer.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		return err == nil
	})
	if _, err := viewer.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{}); err != nil {
		t.Errorf("list pods: %v", err)
	}
	if _, err := viewer.CoreV1().Events(ns).List(ctx, metav1.ListOptions{}); err != nil {
		t.Errorf("list events: %v", err)
	}
	if _, err := viewer.AppsV1().Deployments(ns).Get(ctx, "d", metav1.GetOptions{}); err != nil {
		t.Errorf("get deployment: %v", err)
	}
	// Everything else is refused (RC-US-11.1).
	forbidden := func(what string, err error) {
		t.Helper()
		if !apierrors.IsForbidden(err) {
			t.Errorf("%s: %v, want Forbidden", what, err)
		}
	}
	_, err = viewer.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
	forbidden("list secrets", err)
	_, err = viewer.CoreV1().Secrets(ns).Get(ctx, "s", metav1.GetOptions{})
	forbidden("get secret", err)
	_, err = viewer.CoreV1().ConfigMaps(ns).List(ctx, metav1.ListOptions{})
	forbidden("list configmaps", err)
	forbidden("delete deployment", viewer.AppsV1().Deployments(ns).Delete(ctx, "d", metav1.DeleteOptions{}))
	_, err = viewer.AppsV1().Deployments(ns).Patch(ctx, "d", "application/merge-patch+json", []byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{})
	forbidden("scale deployment", err)
	_, err = viewer.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example.com/x:1"}}}}, metav1.CreateOptions{})
	forbidden("create pod", err)
	_, err = viewer.RbacV1().Roles(ns).List(ctx, metav1.ListOptions{})
	forbidden("list roles", err)
	// exec, attach, port-forward and proxy are subresources of pods; a read of pods does not cover them.
	for _, sub := range []string{"exec", "attach", "portforward", "proxy"} {
		for _, verb := range []string{"create", "get"} {
			sar, err := admin.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
				User: "viewer@example.com", Groups: []string{group}, ResourceAttributes: &authzv1.ResourceAttributes{
					Namespace: ns, Verb: verb, Resource: "pods", Subresource: sub}}}, metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if sar.Status.Allowed {
				t.Errorf("pods/%s %s is allowed", sub, verb)
			}
		}
	}
	// pods/log is read.
	sar, err := admin.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
		User: "viewer@example.com", Groups: []string{group}, ResourceAttributes: &authzv1.ResourceAttributes{Namespace: ns, Verb: "get", Resource: "pods", Subresource: "log"}}}, metav1.CreateOptions{})
	if err != nil || !sar.Status.Allowed {
		t.Errorf("pods/log get: %v %v", sar, err)
	}
	// Another team's namespace is out of reach.
	_, err = viewer.CoreV1().Pods("afs-t-other").List(ctx, metav1.ListOptions{})
	forbidden("list pods of another namespace", err)
	// A different group (another role's) gets nothing here.
	stranger := kubernetes.NewForConfigOrDie(envtesting.As(env.Config, "x@example.com", kubectl.GroupFor("arn:aws:iam::111122223333:role/other")))
	_, err = stranger.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	forbidden("list pods as another role's group", err)

	// Drift: someone deletes the RoleBinding; the next pass puts it back.
	if err := admin.RbacV1().RoleBindings(ns).Delete(ctx, "afs-viewer", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if _, err := admin.RbacV1().RoleBindings(ns).Get(ctx, "afs-viewer", metav1.GetOptions{}); err != nil {
		t.Errorf("binding not restored: %v", err)
	}

	// Off: the binding and the Role are gone on the pass, and the viewer is refused.
	src.set(func(t *kubectl.Team) { t.ViewerKubectl = false })
	start := time.Now()
	reconcile()
	if len(entries.want) != 0 {
		t.Errorf("entry still wanted: %v", entries.want)
	}
	if _, err := admin.RbacV1().RoleBindings(ns).Get(ctx, "afs-viewer", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("binding after off: %v", err)
	}
	if _, err := admin.RbacV1().Roles(ns).Get(ctx, "afs-viewer", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("role after off: %v", err)
	}
	envtesting.Eventually(t, 60*time.Second, "the viewer to lose access", func() bool {
		_, err := viewer.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		return apierrors.IsForbidden(err)
	})
	if d := time.Since(start); d > 60*time.Second {
		t.Errorf("access ended after %v, want within 60 s", d)
	}
}

// A Role named afs-viewer that the reconciler did not make is not deleted when access goes off.
func TestViewerKubectlNeverDeletesAForeignRole(t *testing.T) {
	c := newClient(t)
	ctx := t.Context()
	const ns = "afs-t-kc-foreign"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	admin := envtesting.Clientset(t, env.Config)
	if _, err := admin.RbacV1().Roles(ns).Create(ctx, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "afs-viewer"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := k8s.NewKubectlCluster(c).RemoveViewerAccess(ctx, ns)
	if err != nil || len(res.Changed) != 0 {
		t.Fatalf("remove: %v %v", res, err)
	}
	if _, err := admin.RbacV1().Roles(ns).Get(ctx, "afs-viewer", metav1.GetOptions{}); err != nil {
		t.Errorf("a role the reconciler does not manage was deleted: %v", err)
	}
	// Removing what is not there is not an error.
	if _, err := k8s.NewKubectlCluster(c).RemoveViewerAccess(ctx, "afs-t-kc-empty"); err != nil {
		t.Errorf("remove in an empty namespace: %v", err)
	}
}

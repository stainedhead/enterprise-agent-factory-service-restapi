//go:build envtest

package k8s_test

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/envtesting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

const (
	poolTeam  = "team_01JD0000000000000000000051"
	poolA     = "pool_01JD0000000000000000000061"
	poolOther = "pool_01JD0000000000000000000062"
)

var deploymentGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

// releaseClaims clears the finalizers of the pool's terminating claims, as the controller manager would.
func releaseClaims(t *testing.T, ns, poolID string) {
	t.Helper()
	cs := envtesting.Clientset(t, env.Config)
	list, err := cs.CoreV1().PersistentVolumeClaims(ns).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, pvc := range list.Items {
		if pvc.DeletionTimestamp == nil || (pvc.Labels[render.LabelPoolID] != poolID && !strings.HasPrefix(pvc.Name, "pool-")) {
			continue
		}
		pvc.Finalizers = nil
		if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Update(t.Context(), &pvc, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

func poolDesired(ns, name, id string) pool.Desired {
	return pool.Desired{
		ID: id, Name: name, Template: "omp", Image: "registry.example.com/busybox@sha256:abc", ModelID: "m1",
		State: pool.StateEnabled, Generation: 1, Workers: 3, ScratchGiB: 20, SharedVolume: pool.SharedEFS,
		Resources: agent.Resources{CPU: "500m", Memory: "1Gi"},
		Team:      agent.Team{ID: poolTeam, Slug: "pl", Namespace: ns, State: "active", IsolationTier: agent.TierShared},
		Tags:      map[string]string{"afs-team-id": poolTeam, "afs-pool-id": id},
		Bindings: []agent.Binding{
			{SecretID: "sec_A", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/x/gh-AbCdEf", VersionID: "v1", Delivery: agent.DeliveryEnv, Target: "GH_TOKEN"},          //nolint:gosec // an example ARN, not a credential
			{SecretID: "sec_B", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/x/key-AbCdEf", VersionID: "v1", Delivery: agent.DeliveryFile, Target: "/etc/keys/k.pem"}, //nolint:gosec // an example ARN, not a credential
		},
		Config: []agent.ConfigValue{{Name: "LOG_LEVEL", Value: "info"}},
	}
}

func poolEnv(d pool.Desired) pool.Environment {
	inbox := "https://sqs.us-east-1.amazonaws.com/111122223333/afs-q"
	return pool.Environment{Pool: d, RoleARN: "arn:aws:iam::111122223333:role/afs/" + d.Team.Namespace + "/afs-" + d.ID, Region: "us-east-1",
		Gateway:  agent.Gateway{Namespace: "afs-system", Name: "afs-gateway", Port: 8443},
		Replicas: d.Replicas(), Hash: pool.PodTemplateHash(d, inbox, nil), InboxURL: inbox, EFSStorageClass: "efs-sc"}
}

func newPoolCluster(t *testing.T) (*k8s.PoolCluster, *k8s.Client) {
	t.Helper()
	c := newClient(t)
	return k8s.NewPoolCluster(c, render.NodePoolConfig{NodeClass: map[string]any{"role": "afs-node"}}), c
}

func deploymentOf(t *testing.T, c *k8s.Client, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := c.Get(t.Context(), gvkDeployment, ns, name)
	if err != nil || u == nil {
		t.Fatalf("Deployment %s/%s: %v, %v", ns, name, u, err)
	}
	return u
}

func replicasOf(u *unstructured.Unstructured) int64 {
	n, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas")
	return n
}

// M5-C1: applying a pool creates the Deployment with its workers, the ServiceAccount, the ConfigMaps, the
// ExternalSecrets, the egress policy and the shared claim; a second apply changes nothing.
func TestPoolWorkloadConverges(t *testing.T) {
	pc, c := newPoolCluster(t)
	ctx := t.Context()
	ns := "p-converge"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := poolDesired(ns, "triage", poolA)

	res, err := pc.ApplyPool(ctx, poolEnv(d))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) < 7 {
		t.Errorf("changed = %v", res.Changed)
	}
	// The API server stamps the new claim's managed fields once more on the apply that follows its creation
	// (a Pending claim's defaulting), so the no-op is asserted on the pass after that.
	if _, err := pc.ApplyPool(ctx, poolEnv(d)); err != nil {
		t.Fatal(err)
	}
	again, err := pc.ApplyPool(ctx, poolEnv(d))
	if err != nil || len(again.Changed) != 0 {
		t.Fatalf("second apply changed %v, err %v", again.Changed, err)
	}

	dep := deploymentOf(t, c, ns, "pool-triage")
	if replicasOf(dep) != 3 {
		t.Errorf("replicas = %d", replicasOf(dep))
	}
	var pod corev1.PodSpec
	raw, _, _ := unstructured.NestedMap(dep.Object, "spec", "template", "spec")
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &pod); err != nil {
		t.Fatal(err)
	}
	envs := map[string]corev1.EnvVar{}
	for _, e := range pod.Containers[0].Env {
		envs[e.Name] = e
	}
	if envs["AFS_INBOX_QUEUE_URL"].Value == "" || envs["GH_TOKEN"].ValueFrom == nil || envs["AFS_WORKER_POD"].ValueFrom == nil {
		t.Errorf("env = %v", envs)
	}
	if pod.ServiceAccountName != "pool-triage" {
		t.Errorf("service account = %q", pod.ServiceAccountName)
	}
	for _, name := range []string{"pool-triage"} {
		if got, err := c.Get(ctx, gvkSA, ns, name); err != nil || got == nil {
			t.Errorf("ServiceAccount %s: %v", name, err)
		}
	}
	for _, name := range []string{"pool-triage-config", "pool-triage-aws"} {
		if got, err := c.Get(ctx, gvkConfigMap, ns, name); err != nil || got == nil {
			t.Errorf("ConfigMap %s: %v", name, err)
		}
	}
	for _, name := range []string{"pool-triage-env", "pool-triage-files"} {
		if got, err := c.Get(ctx, gvkES, ns, name); err != nil || got == nil {
			t.Errorf("ExternalSecret %s: %v", name, err)
		}
	}
	if got, err := c.Get(ctx, gvkCNP, ns, pool.EgressPolicyName(d.ID)); err != nil || got == nil {
		t.Errorf("egress policy: %v, %v", got, err)
	}
	claim, err := c.Get(ctx, gvkPVCt, ns, pool.SharedClaimName(d.Name))
	if err != nil || claim == nil {
		t.Fatalf("shared claim: %v, %v", claim, err)
	}
	if modes, _, _ := unstructured.NestedStringSlice(claim.Object, "spec", "accessModes"); len(modes) != 1 || modes[0] != "ReadWriteMany" {
		t.Errorf("shared claim modes = %v", modes)
	}
	if got := dep.GetLabels()[render.LabelPoolID]; got != poolA {
		t.Errorf("pool label = %q", got)
	}
}

// M5-C1: scaling changes replicas only; pausing and a team suspend take the pool to zero and the resume brings
// back the count that was in the row; none of them touches the pod template.
func TestPoolScalingChangesReplicasOnly(t *testing.T) {
	pc, c := newPoolCluster(t)
	ctx := t.Context()
	ns := "p-scale"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := poolDesired(ns, "scaler", poolOther)
	if _, err := pc.ApplyPool(ctx, poolEnv(d)); err != nil {
		t.Fatal(err)
	}
	base := deploymentOf(t, c, ns, "pool-scaler")
	baseTpl, _, _ := unstructured.NestedMap(base.Object, "spec", "template")

	steps := []struct {
		name   string
		mutate func(*pool.Desired)
		want   int64
	}{
		{"scale up", func(d *pool.Desired) { d.Workers = 7; d.Generation++ }, 7},
		{"scale to zero", func(d *pool.Desired) { d.Workers = 0; d.Generation++ }, 0},
		{"back to three", func(d *pool.Desired) { d.Workers = 3; d.Generation++ }, 3},
		{"pause", func(d *pool.Desired) { d.State = pool.StatePaused }, 0},
		{"resume", func(d *pool.Desired) { d.State = pool.StateEnabled }, 3},
		{"team suspended", func(d *pool.Desired) { d.Team.State = "suspended" }, 0},
		{"team resumed", func(d *pool.Desired) { d.Team.State = "active" }, 3},
	}
	for _, s := range steps {
		s.mutate(&d)
		if _, err := pc.ApplyPool(ctx, poolEnv(d)); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		dep := deploymentOf(t, c, ns, "pool-scaler")
		if got := replicasOf(dep); got != s.want {
			t.Errorf("%s: replicas = %d, want %d", s.name, got, s.want)
		}
		tpl, _, _ := unstructured.NestedMap(dep.Object, "spec", "template")
		if !reflect.DeepEqual(tpl, baseTpl) {
			t.Errorf("%s: the pod template changed", s.name)
		}
	}
	// A hand-made `kubectl scale` is put back by the next apply.
	dep := deploymentOf(t, c, ns, "pool-scaler")
	_ = unstructured.SetNestedField(dep.Object, int64(9), "spec", "replicas")
	if _, err := c.Dynamic().Resource(deploymentGVR).Namespace(ns).Update(ctx, dep, metav1.UpdateOptions{FieldManager: "kubectl"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pc.ApplyPool(ctx, poolEnv(d)); err != nil {
		t.Fatal(err)
	}
	if got := replicasOf(deploymentOf(t, c, ns, "pool-scaler")); got != 3 {
		t.Errorf("drifted replicas = %d, want 3", got)
	}
	w, err := pc.Workers(ctx, d)
	if err != nil || w.Desired != 3 {
		t.Errorf("Workers = %+v, %v", w, err)
	}
}

// M5-C1: bindings removed remove the ExternalSecrets; the shared volume switched off removes the shared claim.
func TestPoolPrunesWhatItNoLongerHas(t *testing.T) {
	pc, c := newPoolCluster(t)
	ctx := t.Context()
	ns := "p-prune"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := poolDesired(ns, "pruner", poolA)
	if _, err := pc.ApplyPool(ctx, poolEnv(d)); err != nil {
		t.Fatal(err)
	}
	d.Bindings, d.SharedVolume = nil, pool.SharedNone
	res, err := pc.ApplyPool(ctx, poolEnv(d))
	if err != nil || len(res.Changed) == 0 {
		t.Fatalf("apply: %v %v", res, err)
	}
	for _, n := range []string{"pool-pruner-env", "pool-pruner-files"} {
		if got, _ := c.Get(ctx, gvkES, ns, n); got != nil {
			t.Errorf("ExternalSecret %s survived", n)
		}
	}
	// No controller manager runs in envtest, so the claim stays terminating on its pvc-protection finalizer.
	if got, _ := c.Get(ctx, gvkPVCt, ns, pool.SharedClaimName(d.Name)); got != nil && got.GetDeletionTimestamp() == nil {
		t.Error("the shared claim survived switching the volume off")
	}
}

// M5-C4: the delete steps remove the Deployment, then the volumes the pool owned, and leave another pool's
// objects alone.
func TestPoolDeleteSteps(t *testing.T) {
	pc, c := newPoolCluster(t)
	lc := k8s.NewPoolLifecycle(pc)
	ctx := t.Context()
	ns := "p-delete"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := poolDesired(ns, "doomed", poolA)
	keep := poolDesired(ns, "keeper", poolOther)
	for _, x := range []pool.Desired{d, keep} {
		if _, err := pc.ApplyPool(ctx, poolEnv(x)); err != nil {
			t.Fatal(err)
		}
	}
	// A scratch claim a worker left behind.
	cs := envtesting.Clientset(t, env.Config)
	if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "scratch-leftover", Labels: map[string]string{render.LabelPoolID: poolA}},
		Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if gone, err := lc.Stopped(ctx, d); err != nil || gone {
		t.Fatalf("Stopped before Stop = %v, %v", gone, err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := lc.Stop(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if gone, err := lc.Stopped(ctx, d); err != nil || !gone {
		t.Fatalf("Stopped after Stop = %v, %v", gone, err)
	}
	if done, err := lc.VolumesGone(ctx, d); err != nil || done {
		t.Fatalf("VolumesGone before = %v, %v", done, err)
	}
	for i := 0; i < 2; i++ {
		if err := lc.DeleteVolumes(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	// No controller manager runs in envtest, so the kubernetes.io/pvc-protection finalizer is never cleared:
	// do what it would, once the claims are terminating.
	releaseClaims(t, ns, poolA)
	if done, err := lc.VolumesGone(ctx, d); err != nil || !done {
		t.Fatalf("VolumesGone after = %v, %v", done, err)
	}
	if got, _ := c.Get(ctx, gvkDeployment, ns, "pool-keeper"); got == nil {
		t.Error("another pool's Deployment was removed")
	}
	if got, _ := c.Get(ctx, gvkPVCt, ns, pool.SharedClaimName(keep.Name)); got == nil {
		t.Error("another pool's shared claim was removed")
	}
	if !strings.HasPrefix(pool.SharedClaimName(d.Name), "pool-") {
		t.Error("claim naming")
	}
}

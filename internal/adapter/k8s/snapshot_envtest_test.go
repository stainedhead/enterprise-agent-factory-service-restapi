//go:build envtest

package k8s_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/envtesting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

var (
	gvkVS  = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}
	gvkVSC = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"}
	gvkVSK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass"}
)

// M2-G2: a snapshot request carries the agent's tags through its VolumeSnapshotClass, reports readiness and
// the EBS handle once the driver is done, and is pruned by flipping the content to Delete first.
func TestSnapshotRequestStatusAndPrune(t *testing.T) {
	c := newClient(t)
	sc := k8s.NewSnapshotCluster(c)
	ctx := t.Context()
	ns := "g-snap"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := agentDesired(ns, "snap", agentA, "snapper")
	name := render.SnapshotObjectName("vsnap_01JD0000000000000000000051")

	if st, err := sc.Status(ctx, d, name); err != nil || st.Exists {
		t.Fatalf("Status before = %+v, %v", st, err)
	}
	if err := sc.Prepare(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := sc.Request(ctx, d, name); err != nil {
		t.Fatal(err)
	}
	class, _ := c.Get(ctx, gvkVSK, "", agent.SnapshotClassName(agentA))
	if class == nil {
		t.Fatal("class missing")
	}
	params, _, _ := unstructured.NestedStringMap(class.Object, "parameters")
	if params["tagSpecification_1"] != "afs-agent-id="+agentA || params["tagSpecification_2"] != "afs-team-id="+agentTeam {
		t.Errorf("tag parameters = %v", params)
	}
	if st, err := sc.Status(ctx, d, name); err != nil || !st.Exists || st.Ready || st.EBSID != "" {
		t.Fatalf("Status requested = %+v, %v", st, err)
	}

	// The snapshot controller (absent in envtest) binds a content and marks the snapshot ready.
	content := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "snapshot.storage.k8s.io/v1", "kind": "VolumeSnapshotContent",
		"metadata": map[string]any{"name": "snapcontent-1"},
		"spec": map[string]any{
			"deletionPolicy": "Retain", "driver": render.EBSCSIDriver,
			"source":            map[string]any{"snapshotHandle": "snap-0abc"},
			"volumeSnapshotRef": map[string]any{"name": name, "namespace": ns},
		},
	}}
	if _, err := c.Dynamic().Resource(schema.GroupVersionResource{Group: gvkVSC.Group, Version: "v1", Resource: "volumesnapshotcontents"}).Create(ctx, content, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	vsRes := c.Dynamic().Resource(schema.GroupVersionResource{Group: gvkVS.Group, Version: "v1", Resource: "volumesnapshots"}).Namespace(ns)
	vs, err := vsRes.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	vs.Object["status"] = map[string]any{"boundVolumeSnapshotContentName": "snapcontent-1", "readyToUse": true, "restoreSize": "20Gi"}
	if _, err := vsRes.UpdateStatus(ctx, vs, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	vc, _ := c.Get(ctx, gvkVSC, "", "snapcontent-1")
	vc.Object["status"] = map[string]any{"snapshotHandle": "snap-0abc", "readyToUse": true}
	_, _ = c.Dynamic().Resource(schema.GroupVersionResource{Group: gvkVSC.Group, Version: "v1", Resource: "volumesnapshotcontents"}).UpdateStatus(ctx, vc, metav1.UpdateOptions{})

	st, err := sc.Status(ctx, d, name)
	if err != nil || !st.Ready || st.SizeGiB != 20 || st.EBSID != "snap-0abc" {
		t.Fatalf("Status ready = %+v, %v", st, err)
	}

	if err := sc.Delete(ctx, d, name); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get(ctx, gvkVS, ns, name); got != nil && got.GetDeletionTimestamp() == nil {
		t.Error("VolumeSnapshot still present")
	}
	vc, _ = c.Get(ctx, gvkVSC, "", "snapcontent-1")
	if p, _, _ := unstructured.NestedString(vc.Object, "spec", "deletionPolicy"); p != "Delete" {
		t.Errorf("content policy = %q, want Delete before the snapshot goes", p)
	}
	if err := sc.Delete(ctx, d, name); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// M2-G4: the delete steps' Kubernetes ports. Stop removes the workload; the claim is kept until a rule acts;
// retain flips the volume's reclaim policy; delete removes the claim.
func TestAgentLifecyclePorts(t *testing.T) {
	ac, c := newAgentCluster(t)
	lc := k8s.NewAgentLifecycle(ac)
	ctx := t.Context()
	ns := "g-life"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := agentDesired(ns, "life", agentA, "leaver")
	if _, err := ac.ApplyAgent(ctx, agentEnv(d)); err != nil {
		t.Fatal(err)
	}
	makePVC(t, ns, agent.PVCName("leaver"), 20)
	if _, err := ac.Volume(ctx, d); err != nil {
		t.Fatal(err)
	}

	cs := envtesting.Clientset(t, env.Config)
	pod, err := cs.CoreV1().Pods(ns).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-leaver-0", Labels: map[string]string{render.LabelAgentID: agentA}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.example.com/busybox@sha256:abc"}}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if err := lc.Stop(ctx, d); err != nil {
		t.Fatal(err)
	}
	if gone, err := lc.Stopped(ctx, d); err != nil || gone {
		t.Fatalf("Stopped with a pod = %v, %v", gone, err)
	}
	if err := cs.CoreV1().Pods(ns).Delete(ctx, pod.Name, metav1.DeleteOptions{GracePeriodSeconds: new(int64)}); err != nil {
		t.Fatal(err)
	}
	if gone, err := lc.Stopped(ctx, d); err != nil || !gone {
		t.Fatalf("Stopped without pods = %v, %v", gone, err)
	}
	if st, _ := ac.Volume(ctx, d); !st.Exists {
		t.Fatal("Stop removed the claim")
	}

	rv, err := lc.RetainVolume(ctx, d)
	if err != nil || rv.Volume != "pv-"+ns+"-"+agent.PVCName("leaver") || rv.Claim != agent.PVCName("leaver") {
		t.Fatalf("RetainVolume = %+v, %v", rv, err)
	}
	pv, err := cs.CoreV1().PersistentVolumes().Get(ctx, rv.Volume, metav1.GetOptions{})
	if err != nil || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Fatalf("reclaim policy = %v, %v", pv.Spec.PersistentVolumeReclaimPolicy, err)
	}

	if err := lc.DeleteVolume(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := lc.DeleteVolume(ctx, d); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if got, _ := c.Get(ctx, gvkPVCt, ns, agent.PVCName("leaver")); got != nil && got.GetDeletionTimestamp() == nil {
		t.Error("claim still present")
	}
	if rv, err := lc.RetainVolume(ctx, agentDesired(ns, "life", agentB, "never-existed")); err != nil || rv.Volume != "" {
		t.Errorf("RetainVolume of a missing claim = %+v, %v", rv, err)
	}

	if err := k8s.NewSnapshotCluster(c).Prepare(ctx, d); err != nil { // creates the class
		t.Fatal(err)
	}
	if err := lc.DeleteSnapshotClass(ctx, d); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get(ctx, gvkVSK, "", agent.SnapshotClassName(agentA)); got != nil {
		t.Error("class still present")
	}
}

// M2-G6: a suspended team's agents run at zero replicas and come back to their own prior state: the agent
// that was enabled returns to one, the one paused before stays paused.
func TestTeamSuspendResumeAgents(t *testing.T) {
	ac, c := newAgentCluster(t)
	ctx := t.Context()
	ns := "g-suspend"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	running := agentDesired(ns, "suspend", agentA, "runner")
	paused := agentDesired(ns, "suspend", agentB, "sleeper")
	paused.State = agent.StatePaused
	replicas := func(name string) int32 { return teammateSpec(t, c, ns, name).Replicas }
	apply := func(teamState string) {
		t.Helper()
		for _, d := range []agent.Desired{running, paused} {
			d.Team.State = teamState
			if _, err := ac.ApplyAgent(ctx, agentEnv(d)); err != nil {
				t.Fatal(err)
			}
		}
	}

	apply("active")
	if replicas("agent-runner") != 1 || replicas("agent-sleeper") != 0 {
		t.Fatalf("active: runner %d sleeper %d", replicas("agent-runner"), replicas("agent-sleeper"))
	}
	apply("suspended")
	if replicas("agent-runner") != 0 || replicas("agent-sleeper") != 0 {
		t.Fatalf("suspended: runner %d sleeper %d", replicas("agent-runner"), replicas("agent-sleeper"))
	}
	apply("active")
	if replicas("agent-runner") != 1 || replicas("agent-sleeper") != 0 {
		t.Fatalf("resumed: runner %d sleeper %d", replicas("agent-runner"), replicas("agent-sleeper"))
	}
}

//go:build envtest

package k8s_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/crd/agentictm"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/envtesting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent/agenttest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team/teamtest"
)

const (
	agentTeam = "team_01JD0000000000000000000031"
	agentA    = "agt_01JD0000000000000000000041"
	agentB    = "agt_01JD0000000000000000000042"
)

var (
	gvkTeammate = agentictm.GVK()
	gvkES       = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}
	gvkSA       = schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}
	gvkPVCt     = schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaim"}
	gvkNP       = schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodePool"}
	gvkNC       = schema.GroupVersionKind{Group: "karpenter.k8s.aws", Version: "v1", Kind: "EC2NodeClass"}
)

func agentDesired(ns, slug, id, name string) agent.Desired {
	return agent.Desired{
		ID: id, Name: name, Template: "hermes", Image: "registry.example.com/busybox@sha256:abc", ModelID: "m1",
		State: agent.StateEnabled, Generation: 1, IAMIdentity: agent.IdentityTeamShared,
		Volume: agent.Volume{SizeGiB: 20, StorageClass: "gp3", OnDelete: "snapshot_then_delete", SnapshotSchedule: "daily", SnapshotRetain: 7},
		Team:   agent.Team{ID: agentTeam, Slug: slug, Namespace: ns, State: "active", IsolationTier: agent.TierShared},
		Tags:   map[string]string{"afs-team-id": agentTeam, "afs-agent-id": id},
		Bindings: []agent.Binding{
			{SecretID: "sec_A", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/x/gh-AbCdEf", VersionID: "v1", Delivery: agent.DeliveryEnv, Target: "GH_TOKEN"},
			{SecretID: "sec_B", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/x/key-AbCdEf", VersionID: "v1", Delivery: agent.DeliveryFile, Target: "/etc/keys/deploy.pem"},
			{SecretID: "sec_C", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/x/okta-AbCdEf", VersionID: "v1", Delivery: agent.DeliveryEnv, Target: "OKTA_TOKEN", ExternalSystemID: "ext_1", SystemOff: true},
		},
		Config: []agent.ConfigValue{{Name: "LOG_LEVEL", Value: "info"}},
	}
}

func agentEnv(d agent.Desired) agent.Environment {
	env := agent.Environment{Agent: d, RoleARN: "arn:aws:iam::111122223333:role/afs/" + d.Team.Namespace + "/afs-team", Region: "us-east-1",
		Gateway: agent.Gateway{Namespace: "afs-system", Name: "afs-gateway", Port: 8443}, Replicas: d.Replicas(), Hash: agent.PodTemplateHash(d)}
	if d.Team.IsolationTier == agent.TierDedicated {
		env.NodePool = agent.NodePool{Dedicated: true, Tags: map[string]string{"afs-team-id": agentTeam}}
	}
	return env
}

func newAgentCluster(t *testing.T) (*k8s.AgentCluster, *k8s.Client) {
	t.Helper()
	c := newClient(t)
	return k8s.NewAgentCluster(c, render.NodePoolConfig{NodeClass: map[string]any{"role": "afs-node"}}), c
}

func teammateSpec(t *testing.T, c *k8s.Client, ns, name string) agentictm.Spec {
	t.Helper()
	u, err := c.Get(t.Context(), gvkTeammate, ns, name)
	if err != nil || u == nil {
		t.Fatalf("Teammate %s/%s: %v, %v", ns, name, u, err)
	}
	s, err := agentictm.SpecOf(u)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// M2-F1, F2, F3: applying an agent creates the Teammate, the ServiceAccount, the ConfigMaps and the
// ExternalSecrets; a secret of a switched-off system is absent, and present again after it is switched on.
func TestAgentWorkloadConverges(t *testing.T) {
	ac, c := newAgentCluster(t)
	ctx := t.Context()
	ns := "f-converge"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := agentDesired(ns, "converge", agentA, "reviewer")
	d.Team.WorkOrderAssigned = true

	res, err := ac.ApplyAgent(ctx, agentEnv(d))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) < 6 {
		t.Errorf("changed = %v", res.Changed)
	}
	again, err := ac.ApplyAgent(ctx, agentEnv(d))
	if err != nil || len(again.Changed) != 0 {
		t.Fatalf("second apply changed %v, err %v", again.Changed, err)
	}

	spec := teammateSpec(t, c, ns, "agent-reviewer")
	pod := spec.PodTemplate
	if spec.Replicas != 1 || spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted != "Retain" || spec.PersistentVolumeClaimRetentionPolicy.WhenScaled != "Retain" {
		t.Errorf("spec = replicas %d retention %+v", spec.Replicas, spec.PersistentVolumeClaimRetentionPolicy)
	}
	if pod.Annotations[agentictm.AnnotationPodTemplateHash] != agent.PodTemplateHash(d) {
		t.Errorf("hash annotation = %q", pod.Annotations[agentictm.AnnotationPodTemplateHash])
	}
	envs := map[string]corev1.EnvVar{}
	for _, e := range pod.Spec.Containers[0].Env {
		envs[e.Name] = e
	}
	if envs["AFS_GATEWAY_URL"].Value != "https://afs-gateway.afs-system.svc:8443" || envs["ANTHROPIC_API_KEY"].Value != "afs-gateway" || envs["AWS_CA_BUNDLE"].Value != "/var/run/secrets/afs/gateway/ca.crt" {
		t.Errorf("gateway env = %v", envs)
	}
	if envs["GH_TOKEN"].ValueFrom == nil {
		t.Error("GH_TOKEN is not a Secret reference")
	}
	if _, ok := envs["OKTA_TOKEN"]; ok {
		t.Error("the switched-off system's secret is in the pod")
	}
	apiMount := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/var/run/secrets/afs/api" {
			apiMount = true
		}
	}
	if !apiMount {
		t.Error("work order team: afs-api token not projected")
	}

	for _, o := range []struct {
		gvk  schema.GroupVersionKind
		name string
	}{{gvkSA, "agent-reviewer"}, {gvkConfigMap, "agent-reviewer-config"}, {gvkConfigMap, "agent-reviewer-aws"}, {gvkES, "agent-reviewer-env"}, {gvkES, "agent-reviewer-files"}} {
		if got, err := c.Get(ctx, o.gvk, ns, o.name); err != nil || got == nil {
			t.Errorf("%s %s missing: %v", o.gvk.Kind, o.name, err)
		}
	}
	cfg, _ := c.Get(ctx, gvkConfigMap, ns, "agent-reviewer-config")
	if v, _, _ := unstructured.NestedString(cfg.Object, "data", "LOG_LEVEL"); v != "info" {
		t.Errorf("config data = %v", cfg.Object["data"])
	}
	envES, _ := c.Get(ctx, gvkES, ns, "agent-reviewer-env")
	if data, _, _ := unstructured.NestedSlice(envES.Object, "spec", "data"); len(data) != 1 {
		t.Errorf("env ExternalSecret has %d keys, want 1 (OKTA_TOKEN withheld)", len(data))
	}

	// The system is switched on: the secret is delivered again on the next reconcile.
	d.Bindings[2].SystemOff = false
	d.Generation = 2
	if _, err := ac.ApplyAgent(ctx, agentEnv(d)); err != nil {
		t.Fatal(err)
	}
	envES, _ = c.Get(ctx, gvkES, ns, "agent-reviewer-env")
	if data, _, _ := unstructured.NestedSlice(envES.Object, "spec", "data"); len(data) != 2 {
		t.Errorf("after switching on: %d keys, want 2", len(data))
	}

	// All bindings removed: the ExternalSecrets are removed with them.
	d.Bindings = nil
	if _, err := ac.ApplyAgent(ctx, agentEnv(d)); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"agent-reviewer-env", "agent-reviewer-files"} {
		if got, _ := c.Get(ctx, gvkES, ns, n); got != nil {
			t.Errorf("ExternalSecret %s survived the removal of its bindings", n)
		}
	}
}

// M2-F4: rotating a secret changes the hash of the referencing agent's pod template only.
func TestRotationRollsOnlyTheReferencingAgent(t *testing.T) {
	ac, c := newAgentCluster(t)
	ns := "f-rotate"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	a := agentDesired(ns, "rotate", agentA, "alpha")
	b := agentDesired(ns, "rotate", agentB, "bravo")
	b.Bindings = nil
	for _, d := range []agent.Desired{a, b} {
		if _, err := ac.ApplyAgent(t.Context(), agentEnv(d)); err != nil {
			t.Fatal(err)
		}
	}
	hashOf := func(name string) string {
		return teammateSpec(t, c, ns, name).PodTemplate.Annotations[agentictm.AnnotationPodTemplateHash]
	}
	before := map[string]string{"alpha": hashOf("agent-alpha"), "bravo": hashOf("agent-bravo")}
	if before["alpha"] == before["bravo"] {
		t.Fatal("distinct agents share a hash")
	}

	a.Bindings[0].VersionID = "v2" // the rotation
	for _, d := range []agent.Desired{a, b} {
		if _, err := ac.ApplyAgent(t.Context(), agentEnv(d)); err != nil {
			t.Fatal(err)
		}
	}
	if hashOf("agent-alpha") == before["alpha"] {
		t.Error("the agent that references the rotated secret did not roll")
	}
	if hashOf("agent-bravo") != before["bravo"] {
		t.Error("an unrelated agent rolled")
	}
	// A configuration change rolls the agent that has the value only.
	b.Config = []agent.ConfigValue{{Name: "LOG_LEVEL", Value: "debug"}}
	for _, d := range []agent.Desired{a, b} {
		if _, err := ac.ApplyAgent(t.Context(), agentEnv(d)); err != nil {
			t.Fatal(err)
		}
	}
	if hashOf("agent-bravo") == before["bravo"] {
		t.Error("a changed configuration value did not roll the agent")
	}
}

// M2-F5: the dedicated tier gets a NodePool and class with taint, label and tags, and tolerations and a
// selector on the pods; going back to shared removes them.
func TestDedicatedTierNodePool(t *testing.T) {
	ac, c := newAgentCluster(t)
	ctx := t.Context()
	ns := "f-dedicated"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := agentDesired(ns, "dedicated", agentA, "worker")
	d.Team.IsolationTier = agent.TierDedicated
	if _, err := ac.ApplyAgent(ctx, agentEnv(d)); err != nil {
		t.Fatal(err)
	}
	np, _ := c.Get(ctx, gvkNP, "", "afs-t-dedicated")
	if np == nil {
		t.Fatal("NodePool not created")
	}
	taints, _, _ := unstructured.NestedSlice(np.Object, "spec", "template", "spec", "taints")
	if len(taints) != 1 || taints[0].(map[string]any)["key"] != render.DedicatedNodeKey || taints[0].(map[string]any)["value"] != agentTeam {
		t.Errorf("taints = %v", taints)
	}
	nc, _ := c.Get(ctx, gvkNC, "", "afs-t-dedicated")
	if nc == nil {
		t.Fatal("EC2NodeClass not created")
	}
	if tag, _, _ := unstructured.NestedString(nc.Object, "spec", "tags", "afs-team-id"); tag != agentTeam {
		t.Errorf("node class tags = %v", nc.Object["spec"])
	}
	pod := teammateSpec(t, c, ns, "agent-worker").PodTemplate
	if pod.Spec.NodeSelector[render.DedicatedNodeKey] != agentTeam || len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Value != agentTeam {
		t.Errorf("pod scheduling = %+v %+v", pod.Spec.NodeSelector, pod.Spec.Tolerations)
	}

	d.Team.IsolationTier = agent.TierShared
	if _, err := ac.ApplyAgent(ctx, agentEnv(d)); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Get(ctx, gvkNP, "", "afs-t-dedicated"); got != nil {
		t.Error("NodePool survived the move to the shared tier")
	}
	if got, _ := c.Get(ctx, gvkNC, "", "afs-t-dedicated"); got != nil {
		t.Error("EC2NodeClass survived the move to the shared tier")
	}
	pod = teammateSpec(t, c, ns, "agent-worker").PodTemplate
	if len(pod.Spec.Tolerations) != 0 || len(pod.Spec.NodeSelector) != 0 {
		t.Errorf("shared pod keeps scheduling constraints: %+v", pod.Spec)
	}
}

// M2-F6: with the real cluster adapter, TemplateRetired is True while the template is retired, the Teammate
// stays at replicas 1 and unchanged, and the condition is cleared on re-enable.
func TestTemplateRetiredKeepsAgentRunning(t *testing.T) {
	ac, c := newAgentCluster(t)
	ctx := t.Context()
	ns := "f-retired"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := agentDesired(ns, "retired", agentA, "keeper")
	src := agenttest.NewSource(d)
	ctl, err := agent.NewController(agent.Config{Source: src, Cluster: ac, IAM: teamtest.NewIAM(), PodIdentity: agenttest.NewPodIdentities(), Volumes: ac,
		AWS: team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: "111122223333", GatewayRoleARN: "arn:aws:iam::111122223333:role/afs-gateway", SecretsPrefix: "afs"}})
	if err != nil {
		t.Fatal(err)
	}
	ref := engine.NewRef(engine.KindAgent, agentA, agentTeam)
	out, err := ctl.Reconcile(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := condOf(out, domain.CondTemplateRetired); !ok || c.Status != domain.ConditionFalse {
		t.Fatalf("TemplateRetired before = %+v", out.Conditions)
	}
	before := teammateSpec(t, c, ns, "agent-keeper")

	d.TemplateRetired, d.TemplateReason = true, "the image digest is no longer allowed"
	src.Put(d)
	out, err = ctl.Reconcile(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := condOf(out, domain.CondTemplateRetired); c.Status != domain.ConditionTrue {
		t.Errorf("TemplateRetired = %+v", c)
	}
	after := teammateSpec(t, c, ns, "agent-keeper")
	if after.Replicas != 1 || after.PodTemplate.Annotations[agentictm.AnnotationPodTemplateHash] != before.PodTemplate.Annotations[agentictm.AnnotationPodTemplateHash] {
		t.Errorf("a retired template changed the running agent: replicas %d", after.Replicas)
	}
	d.TemplateRetired = false
	src.Put(d)
	out, _ = ctl.Reconcile(ctx, ref)
	if c, _ := condOf(out, domain.CondTemplateRetired); c.Status != domain.ConditionFalse {
		t.Errorf("TemplateRetired after re-enable = %+v", c)
	}
}

func condOf(out engine.Outcome, typ string) (domain.Condition, bool) {
	for _, c := range out.Conditions {
		if c.Type == typ {
			return c, true
		}
	}
	return domain.Condition{}, false
}

// makePVC creates a bound claim of gib GiB on a gp3 class that allows expansion, as the EBS CSI driver
// would provide: the API server refuses to resize a claim that is not bound to such a class.
func makePVC(t *testing.T, ns, name string, gib int) {
	t.Helper()
	ctx := t.Context()
	cs := envtesting.Clientset(t, env.Config)
	sc := "gp3"
	allow := true
	policy := storagev1.VolumeBindingImmediate
	if _, err := cs.StorageV1().StorageClasses().Create(ctx, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: sc},
		Provisioner: "ebs.csi.aws.com", AllowVolumeExpansion: &allow, VolumeBindingMode: &policy}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	size := resource.MustParse(fmtGiB(gib))
	pvc, err := cs.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pvName := "pv-" + ns + "-" + name
	if _, err := cs.CoreV1().PersistentVolumes().Create(ctx, &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:               corev1.ResourceList{corev1.ResourceStorage: size},
			AccessModes:            []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName:       sc,
			ClaimRef:               &corev1.ObjectReference{Namespace: ns, Name: name, UID: pvc.UID},
			PersistentVolumeSource: corev1.PersistentVolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/" + pvName}},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pvc.Spec.VolumeName = pvName
	if pvc, err = cs.CoreV1().PersistentVolumeClaims(ns).Update(ctx, pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: size}
	if _, err := cs.CoreV1().PersistentVolumeClaims(ns).UpdateStatus(ctx, pvc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func fmtGiB(n int) string { return resource.NewQuantity(int64(n)<<30, resource.BinarySI).String() }

// M2-G1: growing sizeGiB patches the existing claim; it is never shrunk and nothing else about it changes.
func TestVolumeExpansion(t *testing.T) {
	ac, c := newAgentCluster(t)
	ctx := t.Context()
	ns := "g-expand"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := agentDesired(ns, "expand", agentA, "grower")
	makePVC(t, ns, agent.PVCName("grower"), 20)

	st, err := ac.Volume(ctx, d)
	if err != nil || !st.Exists || st.RequestedGiB != 20 || !st.Bound || st.CapacityGiB != 20 {
		t.Fatalf("Volume = %+v, %v", st, err)
	}
	if changed, err := ac.Expand(ctx, d); err != nil || changed {
		t.Errorf("same size: changed %v, err %v", changed, err)
	}
	d.Volume.SizeGiB = 50
	if changed, err := ac.Expand(ctx, d); err != nil || !changed {
		t.Fatalf("grow: changed %v, err %v", changed, err)
	}
	st, _ = ac.Volume(ctx, d)
	if st.RequestedGiB != 50 {
		t.Errorf("claim requests %d GiB, want 50", st.RequestedGiB)
	}
	pvc, _ := c.Get(ctx, gvkPVCt, ns, agent.PVCName("grower"))
	if sc, _, _ := unstructured.NestedString(pvc.Object, "spec", "storageClassName"); sc != "gp3" {
		t.Errorf("storage class changed to %q", sc)
	}
	d.Volume.SizeGiB = 10
	if changed, err := ac.Expand(ctx, d); err != nil || changed {
		t.Errorf("shrink: changed %v, err %v", changed, err)
	}
	if st, _ = ac.Volume(ctx, d); st.RequestedGiB != 50 {
		t.Errorf("claim shrank to %d GiB", st.RequestedGiB)
	}
	// No claim: nothing to expand.
	d.Name = "other"
	d.Volume.SizeGiB = 80
	if changed, err := ac.Expand(ctx, d); err != nil || changed {
		t.Errorf("missing claim: changed %v, err %v", changed, err)
	}
}

// M2-G3: pause sets replicas to 0 and keeps the claim, resume goes back to 1 on the same claim, restart
// changes the pod-template annotation once.
func TestPauseResumeRestart(t *testing.T) {
	ac, c := newAgentCluster(t)
	ctx := t.Context()
	ns := "g-lifecycle"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := agentDesired(ns, "lifecycle", agentA, "napper")
	d.Bindings = nil
	makePVC(t, ns, agent.PVCName("napper"), 20)
	src := agenttest.NewSource(d)
	ctl, err := agent.NewController(agent.Config{Source: src, Cluster: ac, IAM: teamtest.NewIAM(), PodIdentity: agenttest.NewPodIdentities(), Volumes: ac,
		AWS: team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: "111122223333", GatewayRoleARN: "arn:aws:iam::111122223333:role/afs-gateway", SecretsPrefix: "afs"}})
	if err != nil {
		t.Fatal(err)
	}
	ref := engine.NewRef(engine.KindAgent, agentA, agentTeam)
	step := func(mutate func(d *agent.Desired)) agentictm.Spec {
		mutate(&d)
		src.Put(d)
		if _, err := ctl.Reconcile(ctx, ref); err != nil {
			t.Fatal(err)
		}
		return teammateSpec(t, c, ns, "agent-napper")
	}
	running := step(func(*agent.Desired) {})
	hash := running.PodTemplate.Annotations[agentictm.AnnotationPodTemplateHash]
	if running.Replicas != 1 {
		t.Fatalf("replicas = %d", running.Replicas)
	}

	paused := step(func(d *agent.Desired) { d.State = agent.StatePaused })
	if paused.Replicas != 0 || paused.PodTemplate.Annotations[agentictm.AnnotationPodTemplateHash] != hash {
		t.Errorf("paused: replicas %d, hash %q (pause must not roll the template)", paused.Replicas, paused.PodTemplate.Annotations[agentictm.AnnotationPodTemplateHash])
	}
	if pvc, _ := c.Get(ctx, gvkPVCt, ns, agent.PVCName("napper")); pvc == nil {
		t.Fatal("the claim was removed by pause")
	}
	pvcBefore, _ := c.Get(ctx, gvkPVCt, ns, agent.PVCName("napper"))

	resumed := step(func(d *agent.Desired) { d.State = agent.StateEnabled })
	if resumed.Replicas != 1 {
		t.Errorf("resumed replicas = %d", resumed.Replicas)
	}
	pvcAfter, _ := c.Get(ctx, gvkPVCt, ns, agent.PVCName("napper"))
	if pvcAfter.GetUID() != pvcBefore.GetUID() {
		t.Error("resume did not keep the same volume claim")
	}
	if _, ok := resumed.PodTemplate.Annotations[agentictm.AnnotationRestartNonce]; ok {
		t.Error("restart annotation present without a restart")
	}
	restarted := step(func(d *agent.Desired) { d.RestartNonce = "41" })
	if restarted.PodTemplate.Annotations[agentictm.AnnotationRestartNonce] != "41" || restarted.Replicas != 1 {
		t.Errorf("restart: %+v", restarted.PodTemplate.Annotations)
	}
	again := step(func(*agent.Desired) {})
	if again.PodTemplate.Annotations[agentictm.AnnotationRestartNonce] != "41" {
		t.Error("restart annotation lost on the next reconcile")
	}
	restarted2 := step(func(d *agent.Desired) { d.RestartNonce = "42" })
	if restarted2.PodTemplate.Annotations[agentictm.AnnotationRestartNonce] != "42" {
		t.Error("second restart did not change the annotation")
	}
}

// M2-G5: a missing claim is re-created from the latest ready snapshot, adopted into the cluster by handle.
func TestMissingVolumeRestoredFromSnapshot(t *testing.T) {
	ac, c := newAgentCluster(t)
	ctx := t.Context()
	ns := "g-restore"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := agentDesired(ns, "restore", agentA, "phoenix")
	d.Bindings = nil
	src := agenttest.NewSource(d)
	ctl, err := agent.NewController(agent.Config{Source: src, Cluster: ac, IAM: teamtest.NewIAM(), PodIdentity: agenttest.NewPodIdentities(), Volumes: ac,
		Snapshots: &agenttest.Snapshots{Latest: map[string]string{agentA: "snap-0123456789abcdef0"}},
		AWS:       team.AWSConfig{Partition: "aws", Region: "us-east-1", AccountID: "111122223333", GatewayRoleARN: "arn:aws:iam::111122223333:role/afs-gateway", SecretsPrefix: "afs"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ctl.Reconcile(ctx, engine.NewRef(engine.KindAgent, agentA, agentTeam))
	if err != nil {
		t.Fatal(err)
	}
	if vc, _ := condOf(out, domain.CondVolumeReady); vc.Reason != "Restoring" {
		t.Errorf("VolumeReady = %+v", vc)
	}
	pvc, _ := c.Get(ctx, gvkPVCt, ns, "data-agent-phoenix-0")
	if pvc == nil {
		t.Fatal("claim not re-created")
	}
	if ds, _, _ := unstructured.NestedString(pvc.Object, "spec", "dataSource", "name"); ds != render.RestoreSnapshotName(agentA) {
		t.Errorf("claim data source = %q", ds)
	}
	content, _ := c.Get(ctx, schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"}, "", render.RestoreSnapshotName(agentA))
	if content == nil {
		t.Fatal("snapshot content not imported")
	}
	if h, _, _ := unstructured.NestedString(content.Object, "spec", "source", "snapshotHandle"); h != "snap-0123456789abcdef0" {
		t.Errorf("handle = %q", h)
	}
	if got, _ := c.Get(ctx, schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}, ns, render.RestoreSnapshotName(agentA)); got == nil {
		t.Error("snapshot not imported")
	}
	// Second pass: the claim exists now, so nothing is restored again.
	out, _ = ctl.Reconcile(ctx, engine.NewRef(engine.KindAgent, agentA, agentTeam))
	if vc, _ := condOf(out, domain.CondVolumeReady); vc.Reason == "Restoring" {
		t.Errorf("restore repeated: %+v", vc)
	}
}

// DeleteAgent removes the workload and its objects and keeps the claim.
func TestDeleteAgentKeepsClaim(t *testing.T) {
	ac, c := newAgentCluster(t)
	ctx := t.Context()
	ns := "g-delete-agent"
	envtesting.EnsureNamespace(t, env.Config, ns, nil)
	d := agentDesired(ns, "del", agentA, "goner")
	makePVC(t, ns, agent.PVCName("goner"), 20)
	if _, err := ac.ApplyAgent(ctx, agentEnv(d)); err != nil {
		t.Fatal(err)
	}
	if gone, _ := ac.TeammateGone(ctx, d); gone {
		t.Fatal("Teammate reported gone before the delete")
	}
	if err := ac.DeleteAgent(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := ac.DeleteAgent(ctx, d); err != nil {
		t.Errorf("second delete: %v", err)
	}
	if gone, err := ac.TeammateGone(ctx, d); err != nil || !gone {
		t.Errorf("gone = %v, %v", gone, err)
	}
	for _, o := range []struct {
		gvk  schema.GroupVersionKind
		name string
	}{{gvkSA, "agent-goner"}, {gvkConfigMap, "agent-goner-config"}, {gvkES, "agent-goner-env"}} {
		if got, _ := c.Get(ctx, o.gvk, ns, o.name); got != nil {
			t.Errorf("%s %s survived", o.gvk.Kind, o.name)
		}
	}
	if pvc, _ := c.Get(ctx, gvkPVCt, ns, "data-agent-goner-0"); pvc == nil {
		t.Error("the claim was deleted with the agent")
	}
}

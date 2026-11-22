package render_test

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

const pID = "pool_01JD0000000000000000000031"

func poolDesired() pool.Desired {
	return pool.Desired{
		ID: pID, Name: "fixer", Template: "hermes", Image: "registry.example.com/hermes@sha256:abc", ModelID: "m1",
		State: pool.StateEnabled, Generation: 2, Workers: 5, ScratchGiB: 30, SharedVolume: pool.SharedNone,
		Resources: agent.Resources{CPU: "500m", Memory: "1Gi"},
		Team:      agent.Team{ID: tID, Slug: "swarm", Namespace: "afs-t-swarm", State: "active", IsolationTier: agent.TierShared},
		Tags:      map[string]string{"afs-team-id": tID, "afs-pool-id": pID, "cost-center": "1234"},
		Bindings: []agent.Binding{
			{SecretID: "sec_A", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/afs-t-swarm/fixer/gh-AbCdEf", VersionID: "v1", Delivery: agent.DeliveryEnv, Target: "GH_TOKEN"},                                               //nolint:gosec // an example ARN
			{SecretID: "sec_B", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/afs-t-swarm/fixer/key-AbCdEf", VersionID: "v7", Delivery: agent.DeliveryFile, Target: "/etc/keys/k.pem"},                                      //nolint:gosec // an example ARN
			{SecretID: "sec_C", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/afs-t-swarm/fixer/ok-AbCdEf", VersionID: "v1", Delivery: agent.DeliveryEnv, Target: "OKTA_TOKEN", ExternalSystemID: "ext_1", SystemOff: true}, //nolint:gosec // an example ARN
		},
		Config: []agent.ConfigValue{{Name: "LOG_LEVEL", Value: "info"}},
		RoleLinks: []pool.RoleLink{
			{ID: "prl_1", AccountID: "444455556666", RoleARN: "arn:aws:iam::444455556666:role/shared", ProfileName: "acct-a", ExternalID: "afs-0123456789abcdefghij1", MaxSessionSeconds: 3600, Status: domain.RoleLinkStatusVerified},
			{ID: "prl_2", AccountID: "777788889999", RoleARN: "arn:aws:iam::777788889999:role/shared", ProfileName: "acct-b", ExternalID: "afs-0123456789abcdefghij2", Status: domain.RoleLinkStatusDisabled},
		},
	}
}

func poolEnv(d pool.Desired) pool.Environment {
	return pool.Environment{Pool: d, RoleARN: "arn:aws:iam::111122223333:role/afs/afs-t-swarm/afs-01JD0000000000000000000031", Region: "us-east-1",
		Gateway: agent.Gateway{Namespace: "afs-system", Name: "afs-gateway", Port: 8443}, Replicas: d.Replicas(), Hash: pool.PodTemplateHash(d, "", nil),
		EFSStorageClass: "efs-sc"}
}

func TestPoolPodTemplate(t *testing.T) {
	d := poolDesired()
	env := poolEnv(d)
	env.InboxURL = "https://sqs.us-east-1.amazonaws.com/111122223333/afs-in"
	env.HandoffURLs = map[string]string{"reviewer": "https://sqs.us-east-1.amazonaws.com/111122223333/afs-rev", "cve-fixer": "https://sqs.us-east-1.amazonaws.com/111122223333/afs-cve"}
	pod := render.PoolPodTemplate(env)
	e := envOf(pod)

	want := map[string]string{
		"AFS_POOL_ID": pID, "AFS_POOL_NAME": "fixer", "AFS_TEAM_ID": tID, "AFS_MODEL_ID": "m1", "AWS_REGION": "us-east-1",
		"AFS_SCRATCH_DIR": "/scratch", "AFS_INBOX_QUEUE_URL": env.InboxURL,
		"AFS_HANDOFF_REVIEWER_QUEUE_URL": env.HandoffURLs["reviewer"], "AFS_HANDOFF_CVE_FIXER_QUEUE_URL": env.HandoffURLs["cve-fixer"],
		"AFS_GATEWAY_URL": "https://afs-gateway.afs-system.svc:8443", "AWS_CONFIG_FILE": "/etc/afs/aws/config", "ANTHROPIC_API_KEY": "afs-gateway", //nolint:gosec // placeholder, not a credential
	}
	for k, v := range want {
		if e[k].Value != v {
			t.Errorf("%s = %q, want %q", k, e[k].Value, v)
		}
	}
	if w := e["AFS_WORKER_POD"]; w.ValueFrom == nil || w.ValueFrom.FieldRef == nil || w.ValueFrom.FieldRef.FieldPath != "metadata.name" {
		t.Errorf("AFS_WORKER_POD = %+v, want the pod's own name", w)
	}
	if _, ok := e["AFS_AGENT_ID"]; ok {
		t.Error("a pool worker carries an agent id")
	}
	for _, forbidden := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		if _, ok := e[forbidden]; ok {
			t.Errorf("pod carries %s", forbidden)
		}
	}
	// Secrets: the env binding references the Secret, the switched-off one is absent, files are mounted at their target.
	if ref := e["GH_TOKEN"].ValueFrom; ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != "pool-fixer-env" {
		t.Errorf("GH_TOKEN = %+v", e["GH_TOKEN"])
	}
	if _, ok := e["OKTA_TOKEN"]; ok {
		t.Error("a binding of a switched-off system reaches the pod")
	}
	if m := mountOf(pod)["/etc/keys/k.pem"]; m.SubPath != render.FileKey("/etc/keys/k.pem") || !m.ReadOnly {
		t.Errorf("file mount = %+v", m)
	}
	// Identity, labels, hash and security.
	if pod.Spec.ServiceAccountName != "pool-fixer" || *pod.Spec.AutomountServiceAccountToken {
		t.Errorf("service account %q automount %v", pod.Spec.ServiceAccountName, *pod.Spec.AutomountServiceAccountToken)
	}
	if pod.Labels[render.LabelPoolID] != pID || pod.Labels[render.LabelTeamID] != tID {
		t.Errorf("labels = %v", pod.Labels)
	}
	if _, ok := pod.Labels[render.LabelAgentID]; ok {
		t.Error("a pool worker is labelled as an agent")
	}
	if pod.Annotations["afs.stainedhead.io/pod-template-hash"] != env.Hash {
		t.Errorf("annotations = %v", pod.Annotations)
	}
	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil || *sc.AllowPrivilegeEscalation || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Error("the worker is not restricted")
	}
	if got := pod.Spec.Containers[0].Resources.Limits.Memory().String(); got != "1Gi" {
		t.Errorf("memory limit = %s", got)
	}
	// Volumes: the scratch volume is a generic ephemeral volume of the requested size, so it goes with the pod.
	var scratch *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "scratch" {
			scratch = &pod.Spec.Volumes[i]
		}
		if pod.Spec.Volumes[i].Name == "shared" {
			t.Error("a pool without a shared volume mounts one")
		}
	}
	if scratch == nil || scratch.Ephemeral == nil || scratch.Ephemeral.VolumeClaimTemplate == nil {
		t.Fatalf("scratch volume = %+v", scratch)
	}
	if got := scratch.Ephemeral.VolumeClaimTemplate.Spec.Resources.Requests.Storage().String(); got != "30Gi" {
		t.Errorf("scratch size = %s", got)
	}
	if mountOf(pod)["/scratch"].Name != "scratch" {
		t.Error("scratch is not mounted")
	}
	if !strings.Contains(strings.Join(envNames(pod), ","), "AFS_HANDOFF_CVE_FIXER_QUEUE_URL") {
		t.Error("hyphenated pool name not turned into UPPER_SNAKE")
	}
}

func envNames(pod corev1.PodTemplateSpec) []string {
	var out []string
	for _, e := range pod.Spec.Containers[0].Env {
		out = append(out, e.Name)
	}
	return out
}

func TestPoolPodTemplateOptions(t *testing.T) {
	t.Run("shared EFS volume", func(t *testing.T) {
		d := poolDesired()
		d.SharedVolume = pool.SharedEFS
		pod := render.PoolPodTemplate(poolEnv(d))
		var found bool
		for _, v := range pod.Spec.Volumes {
			if v.Name == "shared" && v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == "pool-fixer-shared" {
				found = true
			}
		}
		if !found || mountOf(pod)["/shared"].Name != "shared" || envOf(pod)["AFS_SHARED_DIR"].Value != "/shared" {
			t.Errorf("shared volume not delivered: %+v", pod.Spec.Volumes)
		}
	})
	t.Run("dedicated team", func(t *testing.T) {
		d := poolDesired()
		env := poolEnv(d)
		env.NodePool = agent.NodePool{Dedicated: true}
		pod := render.PoolPodTemplate(env)
		if pod.Spec.NodeSelector[render.DedicatedNodeKey] != tID || len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Value != tID {
			t.Errorf("selector %v tolerations %v", pod.Spec.NodeSelector, pod.Spec.Tolerations)
		}
	})
	t.Run("work order assigned", func(t *testing.T) {
		d := poolDesired()
		d.Team.WorkOrderAssigned = true
		pod := render.PoolPodTemplate(poolEnv(d))
		if envOf(pod)["AFS_API_TOKEN_FILE"].Value != "/var/run/secrets/afs/api/token" {
			t.Error("the afs-api token is not projected for a team with a work order")
		}
	})
	t.Run("no links no aws config", func(t *testing.T) {
		d := poolDesired()
		d.RoleLinks = nil
		pod := render.PoolPodTemplate(poolEnv(d))
		if _, ok := envOf(pod)["AWS_CONFIG_FILE"]; ok {
			t.Error("AWS_CONFIG_FILE set without links")
		}
	})
}

func TestPoolAWSConfig(t *testing.T) {
	cfg := render.PoolAWSConfig(poolDesired())
	if !strings.Contains(cfg, "[profile acct-a]") || strings.Contains(cfg, "acct-b") {
		t.Errorf("config = %q: want the verified link and not the disabled one", cfg)
	}
	if !strings.Contains(cfg, "role_session_name = "+pID) || !strings.Contains(cfg, "credential_source = EcsContainer") || !strings.Contains(cfg, "duration_seconds = 3600") {
		t.Errorf("config = %q", cfg)
	}
	var tags map[string]string
	if err := json.Unmarshal([]byte(render.PoolTagsJSON(poolDesired())), &tags); err != nil || tags["afs-pool-id"] != pID {
		t.Errorf("tags = %v %v", tags, err)
	}
	cm := render.PoolAWSConfigMap(poolDesired())
	data, _, _ := nestedMap(cm.Object, "data")
	if cm.GetName() != "pool-fixer-aws" || data["config"] == nil || data["tags.json"] == nil {
		t.Errorf("aws config map = %v", cm.Object)
	}
}

func TestPoolObjectsAreManagedByThePool(t *testing.T) {
	d := poolDesired()
	env := poolEnv(d)
	dep, err := render.PoolDeployment(env)
	if err != nil {
		t.Fatal(err)
	}
	objs := []interface {
		GetLabels() map[string]string
		GetName() string
		GetKind() string
	}{render.PoolServiceAccount(d), render.PoolConfigMap(d), render.PoolAWSConfigMap(d), dep, render.SharedClaim(env)}
	for _, es := range render.PoolExternalSecrets(d) {
		objs = append(objs, es)
	}
	for _, o := range objs {
		l := o.GetLabels()
		if l[render.LabelManagedBy] != render.ManagedByValue || l[render.LabelResourceKind] != "pool" || l[render.LabelResourceID] != pID || l[render.LabelTeamID] != tID {
			t.Errorf("%s %s labels = %v", o.GetKind(), o.GetName(), l)
		}
	}
	sa := render.PoolServiceAccount(d)
	if v, _, _ := nestedBool(sa.Object, "automountServiceAccountToken"); v {
		t.Error("the service account mounts a Kubernetes token")
	}
	cm := render.PoolConfigMap(d)
	if data, _, _ := nestedMap(cm.Object, "data"); data["LOG_LEVEL"] != "info" || cm.GetName() != "pool-fixer-config" {
		t.Errorf("config map = %v", cm.Object)
	}
}

func TestPoolExternalSecrets(t *testing.T) {
	ess := render.PoolExternalSecrets(poolDesired())
	if len(ess) != 2 {
		t.Fatalf("external secrets = %d, want the env and the files Secret", len(ess))
	}
	names := map[string]bool{}
	for _, es := range ess {
		names[es.GetName()] = true
		if s, _, _ := nestedString(es.Object, "spec", "secretStoreRef", "name"); s != render.SecretStoreName {
			t.Errorf("store = %q", s)
		}
		data, _, _ := nestedSlice(es.Object, "spec", "data")
		for _, raw := range data {
			if strings.Contains(strings.ToLower(jsonOf(raw)), "okta") {
				t.Errorf("a switched-off binding is rendered: %v", raw)
			}
		}
	}
	if !names["pool-fixer-env"] || !names["pool-fixer-files"] {
		t.Errorf("names = %v", names)
	}
	d := poolDesired()
	d.Bindings = nil
	if got := render.PoolExternalSecrets(d); len(got) != 0 {
		t.Errorf("secrets without bindings = %d", len(got))
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

// M5-C1: one Deployment per pool; scaling changes replicas only.
func TestPoolDeployment(t *testing.T) {
	d := poolDesired()
	render1, err := render.PoolDeployment(poolEnv(d))
	if err != nil {
		t.Fatal(err)
	}
	var dep appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(render1.Object, &dep); err != nil {
		t.Fatal(err)
	}
	if dep.Name != "pool-fixer" || dep.Namespace != "afs-t-swarm" || *dep.Spec.Replicas != 5 {
		t.Errorf("deployment = %s/%s replicas %v", dep.Namespace, dep.Name, *dep.Spec.Replicas)
	}
	if dep.Spec.Selector.MatchLabels[render.LabelPoolID] != pID || dep.Spec.Template.Labels[render.LabelPoolID] != pID {
		t.Errorf("selector %v template labels %v", dep.Spec.Selector.MatchLabels, dep.Spec.Template.Labels)
	}
	if dep.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Errorf("strategy = %s", dep.Spec.Strategy.Type)
	}
	if _, found, _ := nestedMap(render1.Object, "status"); found {
		t.Error("status stub in the rendered object")
	}

	// Scale to 0 and to 200: only spec.replicas differs.
	diff := func(workers int32) (map[string]any, int64) {
		cur := poolDesired()
		cur.Workers = workers
		out, err := render.PoolDeployment(poolEnv(cur))
		if err != nil {
			t.Fatal(err)
		}
		spec, _, _ := nestedMap(out.Object, "spec")
		reps, _, _ := unstructuredInt(spec, "replicas")
		delete(spec, "replicas")
		return spec, reps
	}
	base, _ := diff(5)
	for _, w := range []int32{0, 1, 200} {
		spec, reps := diff(w)
		if reps != int64(w) {
			t.Errorf("replicas = %d, want %d", reps, w)
		}
		if jsonOf(spec) != jsonOf(base) {
			t.Errorf("workers=%d changed more than the replica count", w)
		}
	}
	paused := poolDesired()
	paused.State = pool.StatePaused
	out, _ := render.PoolDeployment(poolEnv(paused))
	if reps, found, _ := unstructuredInt(mustMap(out.Object, "spec"), "replicas"); !found || reps != 0 {
		t.Errorf("a paused pool renders replicas %d (found %v); 0 must be present, not dropped", reps, found)
	}
}

func mustMap(o map[string]any, f ...string) map[string]any {
	m, _, _ := nestedMap(o, f...)
	return m
}

func unstructuredInt(m map[string]any, key string) (int64, bool, error) {
	switch v := m[key].(type) {
	case int64:
		return v, true, nil
	case float64:
		return int64(v), true, nil
	}
	return 0, false, nil
}

func TestPoolEgress(t *testing.T) {
	d := poolDesired()
	pol, err := egress.PolicyFor(tID, egress.Agent{ID: pID, Systems: []egress.System{
		{ID: "ext_1", Slug: "github", Endpoints: []string{"api.github.com", "*.githubusercontent.com:8443"}}}})
	if err != nil {
		t.Fatal(err)
	}
	u := render.PoolEgress(d.Team.Namespace, d, pol)
	if u.GetName() != "afs-egress-pool-01jd0000000000000000000031" || u.GetNamespace() != "afs-t-swarm" {
		t.Errorf("policy = %s/%s", u.GetNamespace(), u.GetName())
	}
	sel, _, _ := nestedMap(u.Object, "spec", "endpointSelector", "matchLabels")
	if len(sel) != 1 || sel[render.LabelPoolID] != pID {
		t.Errorf("selector = %v: the policy must select the pool's workers and nothing else", sel)
	}
	if l := u.GetLabels(); l[render.LabelResourceKind] != "pool" || l[render.LabelResourceID] != pID || l[render.LabelPoolID] != pID || l[render.LabelManagedBy] != render.ManagedByValue {
		t.Errorf("labels = %v", l)
	}
	rules, _, _ := nestedSlice(u.Object, "spec", "egress")
	if len(rules) != 2 {
		t.Fatalf("egress rules = %d, want one per port", len(rules))
	}
	empty := render.PoolEgress(d.Team.Namespace, d, egress.AgentPolicy{TeamID: tID, AgentID: pID})
	if r, _, _ := nestedSlice(empty.Object, "spec", "egress"); len(r) != 1 {
		t.Errorf("a pool with no system on must still select the workers with one empty rule: %v", r)
	}
}

func TestSharedClaim(t *testing.T) {
	d := poolDesired()
	d.SharedVolume = pool.SharedEFS
	c := render.SharedClaim(poolEnv(d))
	modes, _, _ := nestedSlice(c.Object, "spec", "accessModes")
	if c.GetName() != "pool-fixer-shared" || len(modes) != 1 || modes[0] != "ReadWriteMany" {
		t.Errorf("claim = %v", c.Object)
	}
	if sc, _, _ := nestedString(c.Object, "spec", "storageClassName"); sc != "efs-sc" {
		t.Errorf("storage class = %q", sc)
	}
}

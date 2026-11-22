package render_test

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/crd/agentictm"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

const (
	tID = "team_01JD0000000000000000000011"
	aID = "agt_01JD0000000000000000000021"
)

func agentDesired() agent.Desired {
	return agent.Desired{
		ID: aID, Name: "reviewer", Template: "hermes", Image: "registry.example.com/hermes@sha256:abc", ModelID: "m1", Persona: "terse",
		State: agent.StateEnabled, Generation: 3, IAMIdentity: agent.IdentityTeamShared,
		Resources: agent.Resources{CPU: "500m", Memory: "1Gi"},
		Volume:    agent.Volume{SizeGiB: 20, StorageClass: "gp3"},
		Team:      agent.Team{ID: tID, Slug: "alpha", Namespace: "afs-t-alpha", State: "active", IsolationTier: agent.TierShared},
		Tags:      map[string]string{"afs-team-id": tID, "afs-agent-id": aID, "cost-center": "1234"},
		Bindings: []agent.Binding{
			{SecretID: "sec_A", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/afs-t-alpha/gh-AbCdEf", VersionID: "v1", Delivery: agent.DeliveryEnv, Target: "GH_TOKEN"},                                                 //nolint:gosec // an example ARN, not a credential
			{SecretID: "sec_B", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/afs-t-alpha/key-AbCdEf", VersionID: "v7", Delivery: agent.DeliveryFile, Target: "/etc/keys/deploy.pem"},                                   //nolint:gosec // an example ARN, not a credential
			{SecretID: "sec_C", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/afs-t-alpha/okta-AbCdEf", VersionID: "v1", Delivery: agent.DeliveryEnv, Target: "OKTA_TOKEN", ExternalSystemID: "ext_1", SystemOff: true}, //nolint:gosec // an example ARN, not a credential
		},
		Config: []agent.ConfigValue{{Name: "LOG_LEVEL", Value: "info"}},
	}
}

func environment(d agent.Desired) agent.Environment {
	return agent.Environment{Agent: d, RoleARN: "arn:aws:iam::111122223333:role/afs/afs-t-alpha/afs-01JD", Region: "us-east-1",
		Gateway:  agent.Gateway{Namespace: "afs-system", Name: "afs-gateway", Port: 8443},
		Replicas: d.Replicas(), Hash: agent.PodTemplateHash(d)}
}

func envOf(pod corev1.PodTemplateSpec) map[string]corev1.EnvVar {
	m := map[string]corev1.EnvVar{}
	for _, e := range pod.Spec.Containers[0].Env {
		m[e.Name] = e
	}
	return m
}

func mountOf(pod corev1.PodTemplateSpec) map[string]corev1.VolumeMount {
	m := map[string]corev1.VolumeMount{}
	for _, v := range pod.Spec.Containers[0].VolumeMounts {
		m[v.MountPath] = v
	}
	return m
}

// M2-F3, RC-US-2.1: the model-call path and the CA bundle reach the pod through ordinary variables and files,
// and no AWS key is among them.
func TestPodGatewayDelivery(t *testing.T) {
	pod := render.PodTemplate(environment(agentDesired()))
	env := envOf(pod)
	url := "https://afs-gateway.afs-system.svc:8443"
	want := map[string]string{ //nolint:gosec // gateway placeholder key, not a credential
		"AFS_GATEWAY_URL": url, "AWS_ENDPOINT_URL_BEDROCK_RUNTIME": url, "ANTHROPIC_BASE_URL": url + "/anthropic", "ANTHROPIC_API_KEY": "afs-gateway",
		"AFS_GATEWAY_TOKEN_FILE": "/var/run/secrets/afs/gateway/token",
		"AWS_CA_BUNDLE":          "/var/run/secrets/afs/gateway/ca.crt", "SSL_CERT_FILE": "/var/run/secrets/afs/gateway/ca.crt",
		"REQUESTS_CA_BUNDLE": "/var/run/secrets/afs/gateway/ca.crt", "NODE_EXTRA_CA_CERTS": "/var/run/secrets/afs/gateway/ca.crt",
		"AFS_AGENT_ID": aID, "AFS_TEAM_ID": tID, "AWS_REGION": "us-east-1",
	}
	for k, v := range want {
		if env[k].Value != v {
			t.Errorf("%s = %q, want %q", k, env[k].Value, v)
		}
	}
	for _, forbidden := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
		if _, ok := env[forbidden]; ok {
			t.Errorf("pod carries %s", forbidden)
		}
	}
	if m, ok := mountOf(pod)["/var/run/secrets/afs/gateway"]; !ok || !m.ReadOnly {
		t.Error("gateway directory not mounted read-only")
	}
	var vol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "afs-gateway" {
			vol = &pod.Spec.Volumes[i]
		}
	}
	if vol == nil || vol.Projected == nil || len(vol.Projected.Sources) != 2 {
		t.Fatalf("gateway volume = %+v", vol)
	}
	tok := vol.Projected.Sources[1].ServiceAccountToken
	if tok == nil || tok.Audience != "afs-gateway" || *tok.ExpirationSeconds != 3600 {
		t.Errorf("gateway token projection = %+v", tok)
	}
	if vol.Projected.Sources[0].ConfigMap.Name != render.GatewayCAConfigMap {
		t.Error("CA bundle not read from the copied ConfigMap")
	}
}

// RC-US-2.1: variables and files for bindings, configuration, tags and AWS profiles.
func TestPodBindingsConfigAndTags(t *testing.T) {
	d := agentDesired()
	d.RoleLinks = []agent.RoleLink{{ProfileName: "prod-deployer", RoleARN: "arn:aws:iam::444455556666:role/deployer", ExternalID: "afs-0123456789abcdefghij", MaxSessionSeconds: 3600, Status: "verified"}}
	pod := render.PodTemplate(environment(d))
	env, mounts := envOf(pod), mountOf(pod)

	ref := env["GH_TOKEN"].ValueFrom
	if ref == nil || ref.SecretKeyRef.Name != "agent-reviewer-env" || ref.SecretKeyRef.Key != "GH_TOKEN" || env["GH_TOKEN"].Value != "" {
		t.Errorf("GH_TOKEN = %+v", env["GH_TOKEN"])
	}
	if _, ok := env["OKTA_TOKEN"]; ok {
		t.Error("a secret of a switched-off system is in the pod")
	}
	m, ok := mounts["/etc/keys/deploy.pem"]
	if !ok || m.SubPath != render.FileKey("/etc/keys/deploy.pem") || !m.ReadOnly {
		t.Errorf("file binding mount = %+v", m)
	}
	if mounts["/etc/afs/config"].Name != "afs-config" || pod.Spec.Containers[0].EnvFrom[0].ConfigMapRef.Name != "agent-reviewer-config" {
		t.Error("configuration is not delivered both as variables and as files")
	}
	if mounts["/etc/afs/tags.json"].SubPath != "tags.json" || mounts["/etc/afs/aws/config"].SubPath != "config" || env["AWS_CONFIG_FILE"].Value != "/etc/afs/aws/config" {
		t.Errorf("tags or AWS config not mounted: %+v", mounts)
	}
	var tags map[string]string
	if err := json.Unmarshal([]byte(env["AFS_TAGS"].Value), &tags); err != nil || tags["cost-center"] != "1234" || tags["afs-agent-id"] != aID {
		t.Errorf("AFS_TAGS = %q, %v", env["AFS_TAGS"].Value, err)
	}

	// No link: no AWS config file and no variable.
	pod = render.PodTemplate(environment(agentDesired()))
	if _, ok := mountOf(pod)["/etc/afs/aws/config"]; ok {
		t.Error("AWS config mounted without links")
	}
	if _, ok := envOf(pod)["AWS_CONFIG_FILE"]; ok {
		t.Error("AWS_CONFIG_FILE set without links")
	}
}

// RC-US-2.1, D51: every profile names the agent as the session name.
func TestAWSConfig(t *testing.T) {
	d := agentDesired()
	d.RoleLinks = []agent.RoleLink{
		{ProfileName: "b-prod", RoleARN: "arn:aws:iam::444455556666:role/b", ExternalID: "afs-bbbbbbbbbbbbbbbbbbbb", Status: "verified"},
		{ProfileName: "a-dev", RoleARN: "arn:aws:iam::777788889999:role/a", ExternalID: "afs-aaaaaaaaaaaaaaaaaaaa", MaxSessionSeconds: 1800, Status: "pending_trust"},
		{ProfileName: "off", RoleARN: "arn:aws:iam::123412341234:role/off", Status: "disabled"},
	}
	cfg := render.AWSConfig(d)
	if strings.Contains(cfg, "off") {
		t.Error("a disabled link got a profile")
	}
	if n := strings.Count(cfg, "role_session_name = "+aID); n != 2 {
		t.Errorf("profiles naming the agent = %d, want 2:\n%s", n, cfg)
	}
	if strings.Index(cfg, "a-dev") > strings.Index(cfg, "b-prod") {
		t.Error("profiles are not sorted")
	}
	if !strings.Contains(cfg, "duration_seconds = 1800") || !strings.Contains(cfg, "external_id = afs-bbbbbbbbbbbbbbbbbbbb") || !strings.Contains(cfg, "credential_source = EcsContainer") {
		t.Errorf("profile fields missing:\n%s", cfg)
	}
	d.RoleLinks = nil
	if render.AWSConfig(d) != "" {
		t.Error("config without links")
	}
}

// D29: the afs-api token is projected only in teams with an assigned work order.
func TestAPITokenProjection(t *testing.T) {
	d := agentDesired()
	pod := render.PodTemplate(environment(d))
	if _, ok := mountOf(pod)["/var/run/secrets/afs/api"]; ok {
		t.Error("afs-api token projected without a work order")
	}
	d.Team.WorkOrderAssigned = true
	pod = render.PodTemplate(environment(d))
	if _, ok := mountOf(pod)["/var/run/secrets/afs/api"]; !ok {
		t.Fatal("afs-api token not projected for a team with a work order")
	}
	for _, v := range pod.Spec.Volumes {
		if v.Name == "afs-api" && v.Projected.Sources[0].ServiceAccountToken.Audience != "afs-api" {
			t.Errorf("audience = %s", v.Projected.Sources[0].ServiceAccountToken.Audience)
		}
	}
}

func TestPodIsRestricted(t *testing.T) {
	pod := render.PodTemplate(environment(agentDesired()))
	psc, c := pod.Spec.SecurityContext, pod.Spec.Containers[0]
	if !*psc.RunAsNonRoot || *psc.RunAsUser != render.RunAsID || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security context = %+v", psc)
	}
	if *c.SecurityContext.AllowPrivilegeEscalation || c.SecurityContext.Capabilities.Drop[0] != "ALL" || c.SecurityContext.Privileged != nil {
		t.Errorf("container security context = %+v", c.SecurityContext)
	}
	if *pod.Spec.AutomountServiceAccountToken || pod.Spec.HostNetwork || pod.Spec.ServiceAccountName != "agent-reviewer" {
		t.Errorf("pod spec = %+v", pod.Spec)
	}
	if got := c.Resources.Limits.Cpu().String(); got != "500m" {
		t.Errorf("cpu limit = %s", got)
	}
	if pod.Labels[render.LabelAgentID] != aID || pod.Labels[render.LabelTeamID] != tID {
		t.Errorf("pod labels = %v", pod.Labels)
	}
	// Resources not set: left to the LimitRange.
	d := agentDesired()
	d.Resources = agent.Resources{}
	if r := render.PodTemplate(environment(d)).Spec.Containers[0].Resources; len(r.Limits) != 0 {
		t.Errorf("limits invented: %+v", r)
	}
}

// M2-F5: a dedicated team's pods tolerate and select its own nodes; a shared team's pods do neither.
func TestDedicatedTierScheduling(t *testing.T) {
	d := agentDesired()
	pod := render.PodTemplate(environment(d))
	if len(pod.Spec.Tolerations) != 0 || len(pod.Spec.NodeSelector) != 0 {
		t.Error("shared tier pod has scheduling constraints")
	}
	env := environment(d)
	env.NodePool = agent.NodePool{Dedicated: true}
	pod = render.PodTemplate(env)
	tol := pod.Spec.Tolerations
	if len(tol) != 1 || tol[0].Key != render.DedicatedNodeKey || tol[0].Value != tID || tol[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("tolerations = %+v", tol)
	}
	if pod.Spec.NodeSelector[render.DedicatedNodeKey] != tID {
		t.Errorf("nodeSelector = %v", pod.Spec.NodeSelector)
	}
}

func TestNodePoolAndClass(t *testing.T) {
	d := agentDesired()
	np := render.NodePool(d)
	taints, _, _ := nestedSlice(np.Object, "spec", "template", "spec", "taints")
	if len(taints) != 1 || taints[0].(map[string]any)["key"] != render.DedicatedNodeKey || taints[0].(map[string]any)["value"] != tID {
		t.Errorf("taints = %v", taints)
	}
	labels, _, _ := nestedMap(np.Object, "spec", "template", "metadata", "labels")
	if labels[render.DedicatedNodeKey] != tID {
		t.Errorf("node labels = %v", labels)
	}
	if np.GetName() != "afs-t-alpha" || !render.IsManaged(np) {
		t.Errorf("nodepool = %s managed %v", np.GetName(), render.IsManaged(np))
	}
	nc := render.NodeClass(d, map[string]string{"afs-team-id": tID}, render.NodePoolConfig{NodeClass: map[string]any{"role": "node-role"}})
	spec := nc.Object["spec"].(map[string]any)
	if spec["role"] != "node-role" || spec["tags"].(map[string]any)["afs-team-id"] != tID {
		t.Errorf("node class spec = %v", spec)
	}
}

// RC-US-2.3: file and env bindings render two ExternalSecrets that never contain a value, and a binding
// of a switched-off system is absent.
func TestExternalSecrets(t *testing.T) {
	d := agentDesired()
	ess := render.ExternalSecrets(d)
	if len(ess) != 2 {
		t.Fatalf("ExternalSecrets = %d", len(ess))
	}
	env, files := ess[0], ess[1]
	if env.GetName() != "agent-reviewer-env" || files.GetName() != "agent-reviewer-files" {
		t.Errorf("names = %s, %s", env.GetName(), files.GetName())
	}
	data, _, _ := nestedSlice(env.Object, "spec", "data")
	if len(data) != 1 || data[0].(map[string]any)["secretKey"] != "GH_TOKEN" {
		t.Errorf("env data = %v (the off system's secret must be absent)", data)
	}
	ref := data[0].(map[string]any)["remoteRef"].(map[string]any)
	if !strings.HasPrefix(ref["key"].(string), "arn:aws:secretsmanager:") || ref["version"] != "v1" {
		t.Errorf("remoteRef = %v", ref)
	}
	if s, _, _ := nestedString(env.Object, "spec", "secretStoreRef", "name"); s != render.SecretStoreName {
		t.Errorf("secretStoreRef = %s", s)
	}
	if rf, _, _ := nestedString(env.Object, "spec", "refreshInterval"); rf != "30s" {
		t.Errorf("refreshInterval = %s", rf)
	}
	// Switch the system on: the secret is delivered again.
	d.Bindings[2].SystemOff = false
	env = render.ExternalSecrets(d)[0]
	if data, _, _ := nestedSlice(env.Object, "spec", "data"); len(data) != 2 {
		t.Errorf("after switching on: %v", data)
	}
	// Only env bindings: no files ExternalSecret; no bindings: none.
	d.Bindings = d.Bindings[:1]
	if got := render.ExternalSecrets(d); len(got) != 1 {
		t.Errorf("env only: %d", len(got))
	}
	d.Bindings = nil
	if got := render.ExternalSecrets(d); len(got) != 0 {
		t.Errorf("no bindings: %d", len(got))
	}
	// A connected worker's Paperclip key without a value is in no ExternalSecret and no environment variable.
	d.Bindings = []agent.Binding{{SecretID: "sec_K", SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/x/paperclip-agent-api-key-AbCdEf", //nolint:gosec // an example ARN, not a credential
		Delivery: agent.DeliveryEnv, Target: agent.CollabKeyTarget}}
	if got := render.ExternalSecrets(d); len(got) != 0 {
		t.Errorf("key without a value: %d ExternalSecrets", len(got))
	}
}

func TestFileKeyIsStableAndValid(t *testing.T) {
	a, b := render.FileKey("/etc/a"), render.FileKey("/etc/b")
	if a == b || a != render.FileKey("/etc/a") || len(a) != 18 {
		t.Errorf("keys %q %q", a, b)
	}
}

func TestConfigMaps(t *testing.T) {
	d := agentDesired()
	cm := render.AgentConfigMap(d)
	if data, _, _ := nestedMap(cm.Object, "data"); data["LOG_LEVEL"] != "info" {
		t.Errorf("config data = %v", data)
	}
	d.Config = nil
	if data, _, _ := nestedMap(render.AgentConfigMap(d).Object, "data"); len(data) != 0 {
		t.Errorf("empty config = %v", data)
	}
	aws := render.AgentAWSConfigMap(d)
	data, _, _ := nestedMap(aws.Object, "data")
	if _, ok := data["config"]; ok || data["tags.json"] == "" {
		t.Errorf("aws configmap = %v", data)
	}
}

// M2-F1, RC-16: the Teammate carries replicas, the hash, the restart nonce, the claim template and the Retain
// policy.
func TestTeammate(t *testing.T) {
	d := agentDesired()
	d.RestartNonce = "17"
	u, err := render.Teammate(environment(d))
	if err != nil {
		t.Fatal(err)
	}
	if u.GetAPIVersion() != agentictm.APIVersion() || u.GetKind() != "Teammate" || u.GetName() != "agent-reviewer" || u.GetNamespace() != "afs-t-alpha" {
		t.Errorf("object = %s %s %s/%s", u.GetAPIVersion(), u.GetKind(), u.GetNamespace(), u.GetName())
	}
	if !render.IsManaged(u) || render.OwnerOf(u).ID != aID {
		t.Errorf("labels = %v", u.GetLabels())
	}
	spec, err := agentictm.SpecOf(u)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Replicas != 1 || spec.Template != "hermes" || spec.ModelID != "m1" || spec.Persona != "terse" {
		t.Errorf("spec = %+v", spec)
	}
	ann := spec.PodTemplate.Annotations
	if ann[agentictm.AnnotationPodTemplateHash] != agent.PodTemplateHash(d) || ann[agentictm.AnnotationRestartNonce] != "17" {
		t.Errorf("annotations = %v", ann)
	}
	pol := spec.PersistentVolumeClaimRetentionPolicy
	if pol.WhenScaled != "Retain" || pol.WhenDeleted != "Retain" {
		t.Errorf("retention = %+v", pol)
	}
	if len(spec.VolumeClaimTemplates) != 1 || spec.VolumeClaimTemplates[0].Name != "data" || *spec.VolumeClaimTemplates[0].Spec.StorageClassName != "gp3" {
		t.Fatalf("claim templates = %+v", spec.VolumeClaimTemplates)
	}
	if q := spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String(); q != "20Gi" {
		t.Errorf("claim size = %s", q)
	}
	// Null stubs are pruned so the apply owns only what the reconciler sets.
	raw, _ := json.Marshal(u.Object)
	if strings.Contains(string(raw), "null") || strings.Contains(string(raw), `"creationTimestamp"`) {
		t.Errorf("rendered object carries null stubs: %s", raw)
	}
	// Paused: replicas 0, same template.
	d.State = agent.StatePaused
	env := environment(d)
	u, _ = render.Teammate(env)
	if spec, _ = agentictm.SpecOf(u); spec.Replicas != 0 {
		t.Errorf("paused replicas = %d", spec.Replicas)
	}
}

func TestServiceAccountHasNoAPIToken(t *testing.T) {
	sa := render.ServiceAccount(agentDesired())
	if v, _, _ := nestedBool(sa.Object, "automountServiceAccountToken"); v {
		t.Error("ServiceAccount mounts an API token")
	}
	if sa.GetName() != "agent-reviewer" || sa.GetNamespace() != "afs-t-alpha" {
		t.Errorf("sa = %s/%s", sa.GetNamespace(), sa.GetName())
	}
}

func TestSnapshotObjects(t *testing.T) {
	d := agentDesired()
	cls := render.SnapshotClass(d)
	if cls.GetName() != "afs-01jd0000000000000000000021" || cls.Object["driver"] != "ebs.csi.aws.com" || cls.Object["deletionPolicy"] != "Retain" {
		t.Errorf("class = %v", cls.Object)
	}
	params := cls.Object["parameters"].(map[string]any)
	if params["tagSpecification_1"] != "afs-agent-id="+aID || params["tagSpecification_3"] != "cost-center=1234" {
		t.Errorf("tag parameters = %v", params)
	}
	vs := render.VolumeSnapshot(d, render.SnapshotObjectName("vsnap_01JD0000000000000000000099"))
	if vs.GetName() != "afs-vsnap-01jd0000000000000000000099" || vs.GetNamespace() != "afs-t-alpha" {
		t.Errorf("snapshot = %s/%s", vs.GetNamespace(), vs.GetName())
	}
	if pvc, _, _ := nestedString(vs.Object, "spec", "source", "persistentVolumeClaimName"); pvc != "data-agent-reviewer-0" {
		t.Errorf("source pvc = %s", pvc)
	}
	content := render.ImportedContent(d, "afs-restore-x", "snap-0123456789abcdef0")
	if h, _, _ := nestedString(content.Object, "spec", "source", "snapshotHandle"); h != "snap-0123456789abcdef0" {
		t.Errorf("handle = %s", h)
	}
	pvc := render.RestoredPVC(d, "afs-restore-x")
	if pvc.GetName() != "data-agent-reviewer-0" || !render.IsManaged(pvc) {
		t.Errorf("pvc = %s", pvc.GetName())
	}
	if ds, _, _ := nestedString(pvc.Object, "spec", "dataSource", "name"); ds != "afs-restore-x" {
		t.Errorf("data source = %s", ds)
	}
	if render.RestoreSnapshotName(aID) != "afs-restore-01jd0000000000000000000021" {
		t.Errorf("restore name = %s", render.RestoreSnapshotName(aID))
	}
	if render.StorageQuantity(50) != "50Gi" {
		t.Error("quantity")
	}
}

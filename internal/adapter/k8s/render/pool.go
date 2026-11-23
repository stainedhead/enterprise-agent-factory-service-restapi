package render

import (
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/crd/agentictm"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

// Paths, volume names and the container of a pool worker.
const (
	// PoolContainerName is the worker's container.
	PoolContainerName = "worker"
	// ScratchDir is where a worker's own scratch volume is mounted; it goes with the pod.
	ScratchDir = "/scratch"
	// SharedDir is where the pool's shared EFS volume is mounted when the pool has one.
	SharedDir = "/shared"

	volumeScratch = "scratch"
	volumeShared  = "shared"
)

// OwnerOfPool is the owner of every object of a pool.
func OwnerOfPool(d pool.Desired) Owner {
	return Owner{Kind: string(engine.KindPool), ID: d.ID, TeamID: d.Team.ID, TeamSlug: d.Team.Slug}
}

// PoolServiceAccount renders the pool's ServiceAccount, shared by every worker. Like an agent's it mounts no
// Kubernetes API token; Pod Identity maps it to the pool's role.
func PoolServiceAccount(d pool.Desired) *unstructured.Unstructured {
	u := New("v1", "ServiceAccount", d.Team.Namespace, pool.ServiceAccountName(d.Name))
	u.Object["automountServiceAccountToken"] = false
	return Managed(u, OwnerOfPool(d))
}

// PoolConfigMap renders the pool's non-secret configuration values: delivered as environment variables
// (envFrom) and as files under /etc/afs/config.
func PoolConfigMap(d pool.Desired) *unstructured.Unstructured {
	u := New("v1", "ConfigMap", d.Team.Namespace, pool.ConfigMapName(d.Name))
	data := map[string]any{}
	for _, c := range d.Config {
		data[c.Name] = c.Value
	}
	u.Object["data"] = data
	return Managed(u, OwnerOfPool(d))
}

// asAgent is the pool as the shared renderers see it: the id and the role links are what the AWS profiles
// and the tags file need, and the session name of every profile is the pool's id (D51).
func asAgent(d pool.Desired) agent.Desired {
	links := make([]agent.RoleLink, 0, len(d.RoleLinks))
	for _, l := range d.RoleLinks {
		links = append(links, agent.RoleLink{ProfileName: l.ProfileName, RoleARN: l.RoleARN, ExternalID: l.ExternalID,
			MaxSessionSeconds: l.MaxSessionSeconds, Status: string(l.Status)})
	}
	return agent.Desired{ID: d.ID, Name: d.Name, Tags: d.Tags, RoleLinks: links}
}

// PoolAWSConfig renders /etc/afs/aws/config of a pool: one profile per link that is not disabled, with the
// pool's id as the role session name; "" when there is none.
func PoolAWSConfig(d pool.Desired) string { return AWSConfig(asAgent(d)) }

// PoolTagsJSON is the pool's effective tags as one JSON object (/etc/afs/tags.json and AFS_TAGS).
func PoolTagsJSON(d pool.Desired) string { return TagsJSON(asAgent(d)) }

// PoolAWSConfigMap renders the ConfigMap that holds /etc/afs/tags.json and, when the pool has links,
// /etc/afs/aws/config.
func PoolAWSConfigMap(d pool.Desired) *unstructured.Unstructured {
	u := New("v1", "ConfigMap", d.Team.Namespace, pool.AWSConfigMapName(d.Name))
	data := map[string]any{tagsKey: PoolTagsJSON(d), modelsKey: ModelsJSON(d.ModelID, d.Models)}
	if cfg := PoolAWSConfig(d); cfg != "" {
		data[awsConfigKey] = cfg
	}
	u.Object["data"] = data
	return Managed(u, OwnerOfPool(d))
}

// PoolExternalSecrets renders at most two ExternalSecrets, the env Secret and the files Secret, like an
// agent's. A binding whose secret names a system that is off for the pool is left out (EXT-7). The value is
// never rendered: only the Secrets Manager reference and the version.
func PoolExternalSecrets(d pool.Desired) []*unstructured.Unstructured {
	var env, files []agent.Binding
	for _, b := range d.Bindings {
		if !b.Delivered() {
			continue
		}
		if b.Delivery == agent.DeliveryEnv {
			env = append(env, b)
		} else {
			files = append(files, b)
		}
	}
	var out []*unstructured.Unstructured
	if len(env) > 0 {
		out = append(out, poolExternalSecret(d, pool.EnvSecretName(d.Name), env, func(b agent.Binding) string { return b.Target }))
	}
	if len(files) > 0 {
		out = append(out, poolExternalSecret(d, pool.FilesSecretName(d.Name), files, func(b agent.Binding) string { return FileKey(b.Target) }))
	}
	return out
}

func poolExternalSecret(d pool.Desired, name string, bindings []agent.Binding, key func(agent.Binding) string) *unstructured.Unstructured {
	sort.Slice(bindings, func(i, j int) bool { return key(bindings[i]) < key(bindings[j]) })
	data := make([]any, 0, len(bindings))
	for _, b := range bindings {
		ref := map[string]any{"key": b.SecretARN}
		if b.VersionID != "" {
			ref["version"] = b.VersionID
		}
		data = append(data, map[string]any{"secretKey": key(b), "remoteRef": ref})
	}
	u := New("external-secrets.io/v1", "ExternalSecret", d.Team.Namespace, name)
	u.Object["spec"] = map[string]any{
		"refreshInterval": externalSecretRefresh,
		"secretStoreRef":  map[string]any{"name": SecretStoreName, "kind": "SecretStore"},
		"target":          map[string]any{"name": name, "creationPolicy": "Owner"},
		"data":            data,
	}
	return Managed(u, OwnerOfPool(d))
}

// PoolEgress renders the pool's egress allowance: the same FQDN rules as an agent's, selecting the pool's
// worker pods by the pool label. The team's default deny and its DNS and gateway allowances stay in force;
// this policy only adds. It is owned by the pool, so a drift on it reconciles the pool.
func PoolEgress(namespace string, d pool.Desired, p egress.AgentPolicy) *unstructured.Unstructured {
	u := CiliumAgentEgress(namespace, p)
	u.SetName(pool.EgressPolicyName(d.ID))
	spec, _ := u.Object["spec"].(map[string]any)
	spec["endpointSelector"] = map[string]any{"matchLabels": map[string]any{LabelPoolID: d.ID}}
	l := map[string]string{}
	for k, v := range OwnerOfPool(d).Labels() {
		l[k] = v
	}
	l[LabelPoolID] = d.ID
	u.SetLabels(l)
	return u
}

// SharedClaim renders the pool's shared EFS claim: ReadWriteMany on the platform's EFS storage class. EFS is
// elastic, so the request is a nominal 1Gi.
func SharedClaim(env pool.Environment) *unstructured.Unstructured {
	d := env.Pool
	u := New("v1", "PersistentVolumeClaim", d.Team.Namespace, pool.SharedClaimName(d.Name))
	u.Object["spec"] = map[string]any{
		"accessModes":      []any{"ReadWriteMany"},
		"storageClassName": env.EFSStorageClass,
		"resources":        map[string]any{"requests": map[string]any{"storage": "1Gi"}},
	}
	u.SetLabels(map[string]string{"app.kubernetes.io/name": pool.WorkloadName(d.Name), LabelPoolID: d.ID})
	return Managed(u, OwnerOfPool(d))
}

// PoolPodTemplate builds the worker pod: the container with the delivered variables and files, the pool's
// queue URLs, the volumes (a scratch volume of the requested size that is created and deleted with the pod,
// the shared EFS claim when chosen), the restricted security context, the tolerations of a dedicated team and
// the pod-template hash. A worker knows its own pod name (AFS_WORKER_POD), which the gateway and the
// collectors read from the pod, not from the worker.
func PoolPodTemplate(env pool.Environment) corev1.PodTemplateSpec {
	d := env.Pool
	gwVol, gwMount := gatewayVolume()
	volumes := []corev1.Volume{
		gwVol,
		{Name: volumeConfig, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: pool.ConfigMapName(d.Name)}}}},
		{Name: volumeScratch, VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{
			VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelPoolID: d.ID}},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse(StorageQuantity(d.ScratchGiB))}},
				},
			}}}},
	}
	mounts := []corev1.VolumeMount{
		gwMount,
		{Name: volumeScratch, MountPath: ScratchDir},
		{Name: volumeConfig, MountPath: ConfigDir, ReadOnly: true},
	}
	awsItems := []corev1.KeyToPath{{Key: tagsKey, Path: tagsKey}, {Key: modelsKey, Path: modelsKey}}
	mounts = append(mounts, corev1.VolumeMount{Name: volumeAWS, MountPath: TagsPath, SubPath: tagsKey, ReadOnly: true},
		corev1.VolumeMount{Name: volumeAWS, MountPath: ModelsPath, SubPath: modelsKey, ReadOnly: true})

	envVars := append([]corev1.EnvVar{}, GatewayEnv(env.Gateway.URL())...)
	envVars = append(envVars,
		corev1.EnvVar{Name: "AFS_TAGS", Value: PoolTagsJSON(d)},
		corev1.EnvVar{Name: "AFS_TEAM_ID", Value: d.Team.ID},
		corev1.EnvVar{Name: "AFS_POOL_ID", Value: d.ID},
		corev1.EnvVar{Name: "AFS_POOL_NAME", Value: d.Name},
		corev1.EnvVar{Name: "AFS_WORKER_POD", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		corev1.EnvVar{Name: "AFS_MODEL_ID", Value: d.ModelID},
		corev1.EnvVar{Name: "AFS_DEFAULT_MODEL", Value: d.ModelID},
		corev1.EnvVar{Name: "AFS_MODELS", Value: ModelsEnv(d.Models)},
		corev1.EnvVar{Name: "AFS_SCRATCH_DIR", Value: ScratchDir},
		corev1.EnvVar{Name: "AWS_REGION", Value: env.Region},
		corev1.EnvVar{Name: "AWS_DEFAULT_REGION", Value: env.Region},
	)
	if env.InboxURL != "" {
		envVars = append(envVars, corev1.EnvVar{Name: pool.InboxEnvName, Value: env.InboxURL})
	}
	for name, url := range env.HandoffURLs {
		envVars = append(envVars, corev1.EnvVar{Name: pool.HandoffEnvName(name), Value: url})
	}
	if d.SharedVolume == pool.SharedEFS {
		volumes = append(volumes, corev1.Volume{Name: volumeShared, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: pool.SharedClaimName(d.Name)}}})
		mounts = append(mounts, corev1.VolumeMount{Name: volumeShared, MountPath: SharedDir})
		envVars = append(envVars, corev1.EnvVar{Name: "AFS_SHARED_DIR", Value: SharedDir})
	}
	if PoolAWSConfig(d) != "" {
		awsItems = append(awsItems, corev1.KeyToPath{Key: awsConfigKey, Path: awsConfigKey})
		mounts = append(mounts, corev1.VolumeMount{Name: volumeAWS, MountPath: AWSConfigPath, SubPath: awsConfigKey, ReadOnly: true})
		envVars = append(envVars, corev1.EnvVar{Name: "AWS_CONFIG_FILE", Value: AWSConfigPath})
	}
	volumes = append(volumes, corev1.Volume{Name: volumeAWS, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: pool.AWSConfigMapName(d.Name)}, Items: awsItems}}})

	if d.Team.WorkOrderAssigned {
		v, m := apiVolume()
		volumes, mounts = append(volumes, v), append(mounts, m)
		envVars = append(envVars, corev1.EnvVar{Name: "AFS_API_TOKEN_FILE", Value: APITokenPath})
	}

	var files []agent.Binding
	for _, b := range d.Bindings {
		if !b.Delivered() {
			continue
		}
		switch b.Delivery {
		case agent.DeliveryEnv:
			envVars = append(envVars, corev1.EnvVar{Name: b.Target, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: pool.EnvSecretName(d.Name)}, Key: b.Target}}})
		case agent.DeliveryFile:
			files = append(files, b)
		}
	}
	if len(files) > 0 {
		sort.Slice(files, func(i, j int) bool { return files[i].Target < files[j].Target })
		items := make([]corev1.KeyToPath, 0, len(files))
		for _, b := range files {
			k := FileKey(b.Target)
			items = append(items, corev1.KeyToPath{Key: k, Path: k})
			mounts = append(mounts, corev1.VolumeMount{Name: volumeFiles, MountPath: b.Target, SubPath: k, ReadOnly: true})
		}
		volumes = append(volumes, corev1.Volume{Name: volumeFiles, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: pool.FilesSecretName(d.Name), Items: items, DefaultMode: ptr.To[int32](0o440)}}})
	}
	sortEnv(envVars)

	pod := corev1.PodTemplateSpec{}
	pod.Labels = map[string]string{
		"app.kubernetes.io/name": pool.WorkloadName(d.Name),
		LabelTeamID:              d.Team.ID,
		LabelPoolID:              d.ID,
	}
	pod.Annotations = map[string]string{agentictm.AnnotationPodTemplateHash: env.Hash}
	pod.Spec = corev1.PodSpec{
		ServiceAccountName:           pool.ServiceAccountName(d.Name),
		AutomountServiceAccountToken: ptr.To(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(RunAsID), RunAsGroup: ptr.To(RunAsID), FSGroup: ptr.To(RunAsID),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{{
			Name:  PoolContainerName,
			Image: d.Image,
			Env:   envVars,
			EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: pool.ConfigMapName(d.Name)}}}},
			Resources:    containerResources(d.Resources),
			VolumeMounts: mounts,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		}},
		Volumes: volumes,
	}
	if env.NodePool.Dedicated {
		pod.Spec.NodeSelector = map[string]string{DedicatedNodeKey: d.Team.ID}
		pod.Spec.Tolerations = []corev1.Toleration{{Key: DedicatedNodeKey, Operator: corev1.TolerationOpEqual, Value: d.Team.ID, Effect: corev1.TaintEffectNoSchedule}}
	}
	return pod
}

// PoolDeployment renders the pool's one Deployment: `Replicas` workers (the worker count, or 0 when the pool
// is paused or its team suspended) of the one pod template. A scale changes spec.replicas and nothing else, so
// no worker is replaced. A rolling update replaces workers a quarter at a time when a referenced version
// changes.
func PoolDeployment(env pool.Environment) (*unstructured.Unstructured, error) {
	d := env.Pool
	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: pool.WorkloadName(d.Name), Namespace: d.Team.Namespace,
			Labels: map[string]string{LabelPoolID: d.ID}},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(env.Replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{LabelPoolID: d.ID}},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: ptr.To(intstr.FromString("25%")), MaxSurge: ptr.To(intstr.FromString("25%"))}},
			Template: PoolPodTemplate(env),
		},
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(dep)
	if err != nil {
		return nil, fmt.Errorf("render: convert deployment: %w", err)
	}
	u := &unstructured.Unstructured{Object: agentictm.PruneNulls(m)}
	return Managed(u, OwnerOfPool(d)), nil
}

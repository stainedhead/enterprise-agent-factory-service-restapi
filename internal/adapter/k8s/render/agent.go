package render

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/crd/agentictm"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// Labels and paths of an agent pod.
const (
	// LabelAgentID selects an agent's pods (egress policies, flow attribution).
	LabelAgentID = traffic.LabelAgentID
	// LabelPoolID selects a pool worker's pods; the one constant the traffic collector shares.
	LabelPoolID = traffic.LabelPoolID
	// ContainerName is the agent's container.
	ContainerName = "agent"
	// DataDir is where the agent's persistent volume is mounted.
	DataDir = "/data"
	// RunAsID is the user and group of an agent pod: restricted Pod Security needs a non-root user, and an
	// image that sets none (busybox) would run as root.
	RunAsID int64 = 10001

	volumeData   = agent.VolumeClaimName
	volumeConfig = "afs-config"
	volumeAWS    = "afs-aws"
	volumeFiles  = "afs-files"
)

// OwnerOfAgent is the owner of every object of an agent.
func OwnerOfAgent(d agent.Desired) Owner {
	return Owner{Kind: string(engine.KindAgent), ID: d.ID, TeamID: d.Team.ID, TeamSlug: d.Team.Slug}
}

// ServiceAccount renders the agent's ServiceAccount. It does not mount a Kubernetes API token: the pod
// has no use for one, and the tokens it needs are projected with their own audience. Pod Identity
// maps this ServiceAccount to the agent's role (the association is made through the EKS API).
func ServiceAccount(d agent.Desired) *unstructured.Unstructured {
	u := New("v1", "ServiceAccount", d.Team.Namespace, agent.ServiceAccountName(d.Name))
	u.Object["automountServiceAccountToken"] = false
	return Managed(u, OwnerOfAgent(d))
}

// PodTemplate builds the agent's pod: the container with the delivered variables and files, the volumes,
// the restricted security context, the tolerations of a dedicated team and the pod-template hash and
// restart annotations. A container image with no AFS-specific code receives everything through the
// ordinary mechanisms: environment variables, mounted files and the standard SDK variables (SEC-6).
func PodTemplate(env agent.Environment) corev1.PodTemplateSpec {
	d := env.Agent
	gwVol, gwMount := gatewayVolume()
	volumes := []corev1.Volume{
		gwVol,
		{Name: volumeConfig, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: agent.ConfigMapName(d.Name)}}}},
	}
	mounts := []corev1.VolumeMount{
		gwMount,
		{Name: volumeData, MountPath: DataDir},
		{Name: volumeConfig, MountPath: ConfigDir, ReadOnly: true},
	}

	// Tags and the AWS profiles come from one ConfigMap, mounted file by file.
	awsItems := []corev1.KeyToPath{{Key: tagsKey, Path: tagsKey}}
	mounts = append(mounts, corev1.VolumeMount{Name: volumeAWS, MountPath: TagsPath, SubPath: tagsKey, ReadOnly: true})
	envVars := append([]corev1.EnvVar{}, GatewayEnv(env.Gateway.URL())...)
	envVars = append(envVars,
		corev1.EnvVar{Name: "AFS_TAGS", Value: TagsJSON(d)},
		corev1.EnvVar{Name: "AFS_TEAM_ID", Value: d.Team.ID},
		corev1.EnvVar{Name: "AFS_AGENT_ID", Value: d.ID},
		corev1.EnvVar{Name: "AFS_AGENT_NAME", Value: d.Name},
		corev1.EnvVar{Name: "AFS_MODEL_ID", Value: d.ModelID},
		corev1.EnvVar{Name: "AFS_DATA_DIR", Value: DataDir},
		corev1.EnvVar{Name: "AWS_REGION", Value: env.Region},
		corev1.EnvVar{Name: "AWS_DEFAULT_REGION", Value: env.Region},
	)
	if AWSConfig(d) != "" {
		awsItems = append(awsItems, corev1.KeyToPath{Key: awsConfigKey, Path: awsConfigKey})
		mounts = append(mounts, corev1.VolumeMount{Name: volumeAWS, MountPath: AWSConfigPath, SubPath: awsConfigKey, ReadOnly: true})
		envVars = append(envVars, corev1.EnvVar{Name: "AWS_CONFIG_FILE", Value: AWSConfigPath})
	}
	volumes = append(volumes, corev1.Volume{Name: volumeAWS, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
		LocalObjectReference: corev1.LocalObjectReference{Name: agent.AWSConfigMapName(d.Name)}, Items: awsItems}}})

	if d.Team.WorkOrderAssigned {
		v, m := apiVolume()
		volumes, mounts = append(volumes, v), append(mounts, m)
		envVars = append(envVars, corev1.EnvVar{Name: "AFS_API_TOKEN_FILE", Value: APITokenPath})
	}

	// Secret bindings: environment variables reference the env Secret, files are mounted at their exact
	// targets. A binding that is not delivered (EXT-7) is in neither.
	var files []agent.Binding
	for _, b := range d.Bindings {
		if !b.Delivered() {
			continue
		}
		switch b.Delivery {
		case agent.DeliveryEnv:
			envVars = append(envVars, corev1.EnvVar{Name: b.Target, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: agent.EnvSecretName(d.Name)}, Key: b.Target}}})
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
			SecretName: agent.FilesSecretName(d.Name), Items: items, DefaultMode: ptr.To[int32](0o440)}}})
	}
	sortEnv(envVars)

	pod := corev1.PodTemplateSpec{}
	pod.Labels = map[string]string{
		"app.kubernetes.io/name": agent.WorkloadName(d.Name),
		LabelTeamID:              d.Team.ID,
		LabelAgentID:             d.ID,
	}
	pod.Annotations = map[string]string{agentictm.AnnotationPodTemplateHash: env.Hash}
	if d.RestartNonce != "" {
		pod.Annotations[agentictm.AnnotationRestartNonce] = d.RestartNonce
	}
	pod.Spec = corev1.PodSpec{
		ServiceAccountName:           agent.ServiceAccountName(d.Name),
		AutomountServiceAccountToken: ptr.To(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(RunAsID), RunAsGroup: ptr.To(RunAsID), FSGroup: ptr.To(RunAsID),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{{
			Name:  ContainerName,
			Image: d.Image,
			Env:   envVars,
			EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: agent.ConfigMapName(d.Name)}}}},
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

func sortEnv(env []corev1.EnvVar) {
	sort.SliceStable(env, func(i, j int) bool { return env[i].Name < env[j].Name })
}

func containerResources(r agent.Resources) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{}
	set := func(name corev1.ResourceName, v string) {
		q, err := resource.ParseQuantity(v)
		if v == "" || err != nil {
			return
		}
		if out.Requests == nil {
			out.Requests, out.Limits = corev1.ResourceList{}, corev1.ResourceList{}
		}
		out.Requests[name], out.Limits[name] = q, q
	}
	set(corev1.ResourceCPU, r.CPU)
	set(corev1.ResourceMemory, r.Memory)
	return out
}

// VolumeClaimTemplate is the agent's persistent volume claim template.
func VolumeClaimTemplate(d agent.Desired) corev1.PersistentVolumeClaim {
	pvc := corev1.PersistentVolumeClaim{}
	pvc.Name = agent.VolumeClaimName
	pvc.Spec = corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", d.Volume.SizeGiB))}},
	}
	if d.Volume.StorageClass != "" {
		pvc.Spec.StorageClassName = ptr.To(d.Volume.StorageClass)
	}
	return pvc
}

package render

import (
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/crd/agentictm"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Objects of a team gateway (gateway contract §12.1, MEG-RC-1..4). Every one is named afs-gateway (or a
// suffix of it) in the team namespace and owned by the team.
const (
	teamGatewayCredentials   = team.TeamGatewayName + "-credentials"
	teamGatewayDB            = team.TeamGatewayName + "-db"
	teamGatewayTLS           = team.TeamGatewayName + "-tls"
	teamGatewayPolicy        = team.TeamGatewayName + "-policy"
	teamGatewayCredDir       = "/etc/afs/gateway-credentials"
	teamGatewayFederationDir = "/var/run/secrets/afs/federation"
	teamGatewayTLSDir        = "/etc/afs/gateway-tls"
)

// TeamGatewayGVKs are the kinds of a team gateway's objects, in the order they are removed when the team
// returns to the shared gateway.
var TeamGatewayGVKs = []struct {
	GVK  schema.GroupVersionKind
	Name string
}{
	{schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, team.TeamGatewayName},
	{schema.GroupVersionKind{Version: "v1", Kind: "Service"}, team.TeamGatewayName},
	{schema.GroupVersionKind{Group: "policy", Version: "v1", Kind: "PodDisruptionBudget"}, team.TeamGatewayName},
	{schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}, teamGatewayPolicy},
	{schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}, teamGatewayCredentials},
	{schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}, teamGatewayDB},
	{schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}, teamGatewayTLS},
	{schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}, team.TeamGatewayName},
}

var teamGatewayLabels = map[string]string{gatewayNameLabel: team.TeamGatewayName}

// TeamGateway renders the team gateway: its ServiceAccount, the Deployment of two replicas, the Service on
// 8443, a PodDisruptionBudget, the Cilium policy (ingress from the namespace only, egress to the route hosts,
// Bedrock while it is on, the token exchange hosts and the database) and the ExternalSecrets of the route
// credentials and the database URL. Credential values are never rendered, only Secrets Manager references.
func TeamGateway(env team.Environment) ([]*unstructured.Unstructured, error) {
	t, g, spec := env.Team, env.Team.Gateway, env.TeamGateway
	owner := OwnerOfTeam(t)
	sa := New("v1", "ServiceAccount", t.Namespace, team.TeamGatewayName)
	sa.SetLabels(map[string]string{gatewayNameLabel: team.TeamGatewayName})

	dep, err := teamGatewayDeployment(env)
	if err != nil {
		return nil, err
	}
	svc := New("v1", "Service", t.Namespace, team.TeamGatewayName)
	svc.SetLabels(map[string]string{gatewayNameLabel: team.TeamGatewayName})
	svc.Object["spec"] = map[string]any{
		"selector": map[string]any{gatewayNameLabel: team.TeamGatewayName},
		"ports":    []any{map[string]any{"name": "https", "port": int64(team.TeamGatewayPort), "targetPort": int64(team.TeamGatewayPort), "protocol": "TCP"}},
	}
	pdb := New("policy/v1", "PodDisruptionBudget", t.Namespace, team.TeamGatewayName)
	pdb.Object["spec"] = map[string]any{"minAvailable": int64(1), "selector": map[string]any{"matchLabels": map[string]any{gatewayNameLabel: team.TeamGatewayName}}}

	out := []*unstructured.Unstructured{sa, dep, svc, pdb, teamGatewayCilium(env)}
	creds := make([]any, 0)
	for _, c := range g.Credentials() {
		creds = append(creds, map[string]any{"secretKey": c.Key, "remoteRef": map[string]any{"key": c.SecretARN}})
	}
	out = append(out, gatewayExternalSecret(t.Namespace, teamGatewayCredentials, creds))
	if spec.DatabaseSecretARN != "" {
		out = append(out, gatewayExternalSecret(t.Namespace, teamGatewayDB,
			[]any{map[string]any{"secretKey": "url", "remoteRef": map[string]any{"key": spec.DatabaseSecretARN}}}))
	}
	if spec.TLSIssuer != "" {
		cert := New("cert-manager.io/v1", "Certificate", t.Namespace, teamGatewayTLS)
		cert.Object["spec"] = map[string]any{
			"secretName": teamGatewayTLS,
			"dnsNames":   []any{team.TeamGatewayName + "." + t.Namespace + ".svc", team.TeamGatewayName + "." + t.Namespace + ".svc.cluster.local"},
			"issuerRef":  map[string]any{"name": spec.TLSIssuer, "kind": "ClusterIssuer", "group": "cert-manager.io"},
		}
		out = append(out, cert)
	}
	for _, o := range out {
		Managed(o, owner)
	}
	return out, nil
}

func gatewayExternalSecret(ns, name string, data []any) *unstructured.Unstructured {
	u := New("external-secrets.io/v1", "ExternalSecret", ns, name)
	u.Object["spec"] = map[string]any{
		"refreshInterval": externalSecretRefresh,
		"secretStoreRef":  map[string]any{"name": SecretStoreName, "kind": "SecretStore"},
		"target":          map[string]any{"name": name, "creationPolicy": "Owner"},
		"data":            data,
	}
	return u
}

// federationAudience is the audience of the projected token: Entra ID's when a route is Azure-federated,
// else the first Google provider's. A team that federates with both uses the Azure audience, and its Google
// provider must list it as an allowed audience (implementation note).
func federationAudience(g team.TeamGateway) string {
	if g.Federated("azure") {
		return team.AzureTokenAudience
	}
	if a := g.GoogleAudiences(); len(a) > 0 {
		return a[0]
	}
	return ""
}

func teamGatewayDeployment(env team.Environment) (*unstructured.Unstructured, error) {
	t, g, spec := env.Team, env.Team.Gateway, env.TeamGateway
	envVars := []corev1.EnvVar{
		{Name: "AFS_GATEWAY_TEAM_ID", Value: t.ID},
		{Name: "AFS_GATEWAY_TEAM_NAMESPACE", Value: t.Namespace},
		{Name: "AFS_GATEWAY_ADDR", Value: ":" + strconv.Itoa(team.TeamGatewayPort)},
		{Name: "AFS_GATEWAY_TLS_CERT", Value: teamGatewayTLSDir + "/tls.crt"},
		{Name: "AFS_GATEWAY_TLS_KEY", Value: teamGatewayTLSDir + "/tls.key"},
		{Name: "AFS_GATEWAY_CREDENTIALS_DIR", Value: teamGatewayCredDir},
		{Name: "AFS_DATABASE_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: teamGatewayDB}, Key: "url"}}},
		{Name: "AWS_REGION", Value: env.Region},
	}
	mounts := []corev1.VolumeMount{
		{Name: "credentials", MountPath: teamGatewayCredDir, ReadOnly: true},
		{Name: "tls", MountPath: teamGatewayTLSDir, ReadOnly: true},
	}
	volumes := []corev1.Volume{
		{Name: "credentials", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: teamGatewayCredentials, DefaultMode: ptr.To[int32](0o400), Optional: ptr.To(true)}}},
		{Name: "tls", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: teamGatewayTLS, DefaultMode: ptr.To[int32](0o400)}}},
	}
	if aud := federationAudience(g); aud != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "AFS_GATEWAY_FEDERATION_TOKEN", Value: teamGatewayFederationDir + "/token"})
		mounts = append(mounts, corev1.VolumeMount{Name: "federation", MountPath: teamGatewayFederationDir, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "federation", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr.To[int32](0o444),
			Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience: aud, ExpirationSeconds: ptr.To(TokenLifetimeSeconds), Path: "token"}}},
		}}})
	}
	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: team.TeamGatewayName, Namespace: t.Namespace, Labels: teamGatewayLabels},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](team.TeamGatewayReplicas),
			Selector: &metav1.LabelSelector{MatchLabels: teamGatewayLabels},
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxUnavailable: ptr.To(intstr.FromInt32(0)), MaxSurge: ptr.To(intstr.FromInt32(1))}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: teamGatewayLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName:           team.TeamGatewayName,
					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(RunAsID), RunAsGroup: ptr.To(RunAsID), FSGroup: ptr.To(RunAsID),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name: "gateway", Image: spec.Image, Env: envVars, VolumeMounts: mounts,
						Args:  []string{"--team-namespace", t.Namespace},
						Ports: []corev1.ContainerPort{{Name: "https", ContainerPort: team.TeamGatewayPort, Protocol: corev1.ProtocolTCP}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
					Volumes: volumes,
				},
			},
		},
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(dep)
	if err != nil {
		return nil, fmt.Errorf("render: convert team gateway: %w", err)
	}
	return &unstructured.Unstructured{Object: agentictm.PruneNulls(m)}, nil
}

// teamGatewayCilium admits the namespace's own pods on 8443 and lets the gateway reach only its destinations
// (MEG-RC-2). FQDN rules rely on the DNS visibility of the team's DNS policy.
func teamGatewayCilium(env team.Environment) *unstructured.Unstructured {
	t := env.Team
	egress := []any{map[string]any{
		"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{podNamespaceLabel: "kube-system", "k8s-app": "kube-dns"}}},
		"toPorts": []any{map[string]any{
			"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}},
			"rules": map[string]any{"dns": []any{map[string]any{"matchPattern": "*"}}},
		}},
	}}
	tcp := func(p int) []any {
		return []any{map[string]any{"ports": []any{map[string]any{"port": strconv.Itoa(p), "protocol": "TCP"}}}}
	}
	rules := t.Gateway.Egress(env.Region)
	if h := env.TeamGateway.DatabaseHost; h != "" {
		p := env.TeamGateway.DatabasePort
		if p == 0 {
			p = 5432
		}
		rules = append(rules, team.EgressRule{Host: h, Port: p})
	}
	for _, r := range rules {
		if r.InNamespace {
			egress = append(egress, map[string]any{"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{podNamespaceLabel: t.Namespace}}}, "toPorts": tcp(r.Port)})
			continue
		}
		egress = append(egress, map[string]any{"toFQDNs": []any{map[string]any{"matchName": r.Host}}, "toPorts": tcp(r.Port)})
	}
	u := New(ciliumAPIVersion, "CiliumNetworkPolicy", t.Namespace, teamGatewayPolicy)
	u.Object["spec"] = map[string]any{
		"endpointSelector": map[string]any{"matchLabels": map[string]any{gatewayNameLabel: team.TeamGatewayName}},
		"ingress": []any{map[string]any{
			"fromEndpoints": []any{map[string]any{"matchLabels": map[string]any{podNamespaceLabel: t.Namespace}}},
			"toPorts":       tcp(team.TeamGatewayPort),
		}},
		"egress": egress,
	}
	return u
}

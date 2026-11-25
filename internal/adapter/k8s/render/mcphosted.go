package render

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/crd/agentictm"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Hosted MCP servers (MCP PRD §10.2, MCP-RA-6): one Deployment afs-mcp-<slug> per hosted server switched on for a
// team on its own gateway. An init container copies afs-mcp-bridge into the pod, and the server container runs
// the bridge with the server's command as its child, so the stdio server is served as Streamable HTTP on 8080.
// The pod is restricted (non-root, read-only root filesystem, no capabilities, no ServiceAccount token unless the
// server needs AWS), admits only the team gateway and reaches only the hosts its spec declares.

// LabelMcpServer marks a hosted server's objects with its slug, so the reconciler can remove the ones of servers
// that are no longer on.
const LabelMcpServer = "afs.stainedhead.io/mcp-server"

const (
	mcpBridgeDir  = "/afs/bin"
	mcpBridgePath = mcpBridgeDir + "/afs-mcp-bridge"
)

// McpHostedGVKs are the kinds a hosted server is made of, in removal order.
var McpHostedGVKs = []schema.GroupVersionKind{
	{Group: "apps", Version: "v1", Kind: "Deployment"},
	{Version: "v1", Kind: "Service"},
	{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"},
	{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"},
	{Version: "v1", Kind: "ServiceAccount"},
}

// McpHosted renders the objects of every hosted server of the team; none while the team is on the shared gateway,
// MCP is off or no bridge image is configured.
func McpHosted(env team.Environment) ([]*unstructured.Unstructured, error) {
	t := env.Team
	if !t.Gateway.PerNamespace() || env.TeamGateway.McpBridgeImage == "" {
		return nil, nil
	}
	var out []*unstructured.Unstructured
	for _, s := range t.Gateway.Mcp.HostedServers() {
		objs, err := mcpHostedServer(env, s)
		if err != nil {
			return nil, err
		}
		out = append(out, objs...)
	}
	return out, nil
}

// McpHostedSlugs lists the slugs McpHosted renders.
func McpHostedSlugs(env team.Environment) []string {
	if !env.Team.Gateway.PerNamespace() || env.TeamGateway.McpBridgeImage == "" {
		return nil
	}
	var out []string
	for _, s := range env.Team.Gateway.Mcp.HostedServers() {
		out = append(out, s.Slug)
	}
	return out
}

func mcpHostedServer(env team.Environment, s team.McpServer) ([]*unstructured.Unstructured, error) {
	t, h := env.Team, s.Hosted
	name := domain.McpHostedName(s.Slug)
	labels := map[string]string{gatewayNameLabel: name, LabelMcpServer: s.Slug}
	withLabels := func(u *unstructured.Unstructured) *unstructured.Unstructured {
		u.SetLabels(map[string]string{gatewayNameLabel: name, LabelMcpServer: s.Slug})
		return Managed(u, OwnerOfTeam(t))
	}
	var out []*unstructured.Unstructured
	if h.NeedsAWS {
		out = append(out, withLabels(New("v1", "ServiceAccount", t.Namespace, name)))
	}
	if s.CredentialARN != "" {
		es := New("external-secrets.io/v1", "ExternalSecret", t.Namespace, name)
		es.Object["spec"] = map[string]any{
			"refreshInterval": externalSecretRefresh,
			"secretStoreRef":  map[string]any{"name": SecretStoreName, "kind": "SecretStore"},
			"target":          map[string]any{"name": name, "creationPolicy": "Owner"},
			"dataFrom":        []any{map[string]any{"extract": map[string]any{"key": s.CredentialARN}}},
		}
		out = append(out, withLabels(es))
	}
	dep, err := mcpHostedDeployment(env, s, name, labels)
	if err != nil {
		return nil, err
	}
	svc := New("v1", "Service", t.Namespace, name)
	svc.Object["spec"] = map[string]any{
		"selector": map[string]any{gatewayNameLabel: name},
		"ports":    []any{map[string]any{"name": "mcp", "port": int64(domain.McpHostedPort), "targetPort": int64(domain.McpHostedPort), "protocol": "TCP"}},
	}
	out = append(out, withLabels(dep), withLabels(svc), withLabels(mcpHostedCilium(t.Namespace, name, h)))
	return out, nil
}

func mcpHostedDeployment(env team.Environment, s team.McpServer, name string, labels map[string]string) (*unstructured.Unstructured, error) {
	h := s.Hosted
	replicas := int32(1)
	if s.Suspended {
		replicas = 0
	}
	cmd := append([]string{mcpBridgePath, "--listen", ":" + strconv.Itoa(domain.McpHostedPort)}, passEnv(h.EnvNames)...)
	cmd = append(append(cmd, "--"), h.Args...)
	res := corev1.ResourceRequirements{}
	for k, v := range h.Resources {
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return nil, fmt.Errorf("render: hosted MCP server %s resource %s: %w", s.Slug, k, err)
		}
		if res.Requests == nil {
			res.Requests, res.Limits = corev1.ResourceList{}, corev1.ResourceList{}
		}
		res.Requests[corev1.ResourceName(k)] = q
		res.Limits[corev1.ResourceName(k)] = q
	}
	c := corev1.Container{
		Name: "server", Image: h.Image, Command: cmd, Resources: res,
		Ports:        []corev1.ContainerPort{{Name: "mcp", ContainerPort: domain.McpHostedPort, Protocol: corev1.ProtocolTCP}},
		VolumeMounts: []corev1.VolumeMount{{Name: "bridge", MountPath: mcpBridgeDir, ReadOnly: true}, {Name: "tmp", MountPath: "/tmp"}},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz",
			Port: intstr.FromInt32(domain.McpHostedPort)}}, PeriodSeconds: 10},
		SecurityContext: restrictedContainer(),
	}
	if s.CredentialARN != "" {
		c.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: name},
			Optional: ptr.To(true)}}}
	}
	spec := corev1.PodSpec{
		AutomountServiceAccountToken: ptr.To(h.NeedsAWS),
		EnableServiceLinks:           ptr.To(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(RunAsID), RunAsGroup: ptr.To(RunAsID), FSGroup: ptr.To(RunAsID),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		InitContainers: []corev1.Container{{
			Name: "bridge", Image: env.TeamGateway.McpBridgeImage, Args: []string{"--install", mcpBridgeDir},
			VolumeMounts:    []corev1.VolumeMount{{Name: "bridge", MountPath: mcpBridgeDir}},
			SecurityContext: restrictedContainer(),
		}},
		Containers: []corev1.Container{c},
		Volumes: []corev1.Volume{
			{Name: "bridge", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		},
	}
	if h.NeedsAWS {
		spec.ServiceAccountName = name
	}
	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: env.Team.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{gatewayNameLabel: name}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: spec},
		},
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(dep)
	if err != nil {
		return nil, fmt.Errorf("render: convert hosted MCP server: %w", err)
	}
	return &unstructured.Unstructured{Object: agentictm.PruneNulls(m)}, nil
}

func passEnv(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	ns := slices.Clone(names)
	slices.Sort(ns)
	return []string{"--pass-env", strings.Join(ns, ",")}
}

func restrictedContainer() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), RunAsNonRoot: ptr.To(true),
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// mcpHostedCilium admits only the team gateway on the bridge port and lets the server reach DNS and the hosts its
// spec declares on 443 (default none).
func mcpHostedCilium(ns, name string, h *team.HostedServer) *unstructured.Unstructured {
	egress := []any{map[string]any{
		"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{podNamespaceLabel: "kube-system", "k8s-app": "kube-dns"}}},
		"toPorts": []any{map[string]any{
			"ports": []any{map[string]any{"port": "53", "protocol": "ANY"}},
			"rules": map[string]any{"dns": []any{map[string]any{"matchPattern": "*"}}},
		}},
	}}
	hosts := slices.Clone(h.EgressHosts)
	slices.Sort(hosts)
	for _, host := range slices.Compact(hosts) {
		sel := map[string]any{"matchName": host}
		if strings.HasPrefix(host, "*.") {
			sel = map[string]any{"matchPattern": host}
		}
		egress = append(egress, map[string]any{"toFQDNs": []any{sel},
			"toPorts": []any{map[string]any{"ports": []any{map[string]any{"port": "443", "protocol": "TCP"}}}}})
	}
	u := New(ciliumAPIVersion, "CiliumNetworkPolicy", ns, name)
	u.Object["spec"] = map[string]any{
		"endpointSelector": map[string]any{"matchLabels": map[string]any{gatewayNameLabel: name}},
		"ingress": []any{map[string]any{
			"fromEndpoints": []any{map[string]any{"matchLabels": map[string]any{podNamespaceLabel: ns, gatewayNameLabel: team.TeamGatewayName}}},
			"toPorts":       []any{map[string]any{"ports": []any{map[string]any{"port": strconv.Itoa(domain.McpHostedPort), "protocol": "TCP"}}}},
		}},
		"egress": egress,
	}
	return u
}

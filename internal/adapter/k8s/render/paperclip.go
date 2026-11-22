package render

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// Names of the collaboration agent's objects (RC-21, RC-23).
const (
	// PaperclipServiceName is the Service in front of the collaboration agent's pod. The name is fixed so
	// worker agents and afs-edge can find Paperclip without a lookup.
	PaperclipServiceName = "paperclip"
	// PaperclipIngressPolicyName admits afs-edge and the team's workers to the collaboration agent.
	PaperclipIngressPolicyName = "afs-paperclip-ingress"
	// PaperclipEgressPolicyName lets the team's pods open connections to the collaboration agent.
	PaperclipEgressPolicyName = "afs-paperclip-egress"

	edgeNameLabel = "app.kubernetes.io/name"
)

// PaperclipService renders the Service `paperclip` of the team namespace: ClusterIP, port 3100, selecting
// the collaboration agent's pod by its agent id label.
func PaperclipService(d agent.Desired) *unstructured.Unstructured {
	u := New("v1", "Service", d.Team.Namespace, PaperclipServiceName)
	u.Object["spec"] = map[string]any{
		"type":     "ClusterIP",
		"selector": map[string]any{LabelAgentID: d.ID},
		"ports": []any{map[string]any{
			"name": "http", "port": int64(agent.CollabPort), "targetPort": int64(agent.CollabPort), "protocol": "TCP",
		}},
	}
	return Managed(u, OwnerOfAgent(d))
}

// LabelCollabConnected is on the pod of a worker that is connected to Paperclip: the ingress policy admits
// exactly the pods that carry it, so connecting or disconnecting a worker (which rolls its pod) is what
// opens or closes its path.
const LabelCollabConnected = "afs.stainedhead.io/paperclip-connected"

// PaperclipIngress renders the policy that admits connections to the collaboration agent on port 3100 from
// the connected workers of the namespace, afs-edge and the reconciler (the last two by namespace and name,
// evaluated on pod identity). Everything else stays denied by the team's default deny (RC-22).
func PaperclipIngress(d agent.Desired, edge agent.EdgeRef) *unstructured.Unstructured {
	u := New(ciliumAPIVersion, "CiliumNetworkPolicy", d.Team.Namespace, PaperclipIngressPolicyName)
	ports := []any{map[string]any{"ports": []any{map[string]any{"port": port(agent.CollabPort), "protocol": "TCP"}}}}
	from := []any{map[string]any{"matchLabels": map[string]any{LabelTeamID: d.Team.ID, LabelCollabConnected: "true"}}}
	if edge.Namespace != "" {
		for _, n := range []string{edge.Name, edge.ReconcilerName} {
			if n != "" {
				from = append(from, map[string]any{"matchLabels": map[string]any{podNamespaceLabel: edge.Namespace, edgeNameLabel: n}})
			}
		}
	}
	u.Object["spec"] = map[string]any{
		"endpointSelector": map[string]any{"matchLabels": map[string]any{LabelAgentID: d.ID}},
		"ingress":          []any{map[string]any{"fromEndpoints": from, "toPorts": ports}},
	}
	return Managed(u, OwnerOfAgent(d))
}

// PaperclipEgress renders the allowance for the connected workers to open connections to the collaboration
// agent on port 3100, the same namespace only; the team's egress policy grants nothing else here.
func PaperclipEgress(d agent.Desired) *unstructured.Unstructured {
	u := New(ciliumAPIVersion, "CiliumNetworkPolicy", d.Team.Namespace, PaperclipEgressPolicyName)
	u.Object["spec"] = map[string]any{
		"endpointSelector": map[string]any{"matchLabels": map[string]any{LabelTeamID: d.Team.ID, LabelCollabConnected: "true"}},
		"egress": []any{map[string]any{
			"toEndpoints": []any{map[string]any{"matchLabels": map[string]any{LabelAgentID: d.ID}}},
			"toPorts":     []any{map[string]any{"ports": []any{map[string]any{"port": port(agent.CollabPort), "protocol": "TCP"}}}},
		}},
	}
	return Managed(u, OwnerOfAgent(d))
}

// PaperclipEnv is the environment of the collaboration agent beyond the delivered secrets (RC-21): the
// listening address, authenticated deployment behind the edge, the public URL while a hostname exists, the
// allowed hostnames, the data directory on the agent's volume, Paperclip's AWS Secrets Manager provider
// settings (SEC-7) and the bootstrap switch the image honours (AFS_PAPERCLIP_BOOTSTRAP is a recorded
// upstream dependency).
func PaperclipEnv(d agent.Desired, c agent.Collab) map[string]string {
	internal := "paperclip." + d.Team.Namespace + ".svc.cluster.local"
	env := map[string]string{
		"PORT":                          "3100",
		"HOST":                          "0.0.0.0",
		"PAPERCLIP_DEPLOYMENT_MODE":     "authenticated",
		"PAPERCLIP_DEPLOYMENT_EXPOSURE": "public",
		"PAPERCLIP_HOME":                DataDir + "/paperclip",
		"PAPERCLIP_ALLOWED_HOSTNAMES":   internal,
		"AFS_PAPERCLIP_BOOTSTRAP":       c.Mode,
		"PAPERCLIP_SECRETS_PROVIDER":    "aws_secrets_manager",
	}
	if c.SecretsPrefix != "" {
		env["PAPERCLIP_SECRETS_AWS_PREFIX"] = c.SecretsPrefix
	}
	if c.Host != "" {
		env["PAPERCLIP_PUBLIC_URL"] = "https://" + c.Host
		env["PAPERCLIP_ALLOWED_HOSTNAMES"] = c.Host + "," + internal
	}
	return env
}

// PaperclipWorkerEnv is what a connected worker receives: where Paperclip is and, once registered, its own
// agent id there. The key arrives as a bound secret (PAPERCLIP_AGENT_API_KEY).
func PaperclipWorkerEnv(d agent.Desired, l agent.CollabLink) map[string]string {
	env := map[string]string{"PAPERCLIP_API_URL": "http://paperclip." + d.Team.Namespace + ".svc.cluster.local:3100"}
	if l.PaperclipAgentID != "" {
		env["PAPERCLIP_AGENT_ID"] = l.PaperclipAgentID
	}
	return env
}

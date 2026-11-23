package render

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// Names of the collaboration agent's objects (RC-21, RC-23).
const (
	// PaperclipServiceName is the Service in front of the collaboration agent's pod. The name is fixed so
	// worker agents and afs-edge can find Paperclip without a lookup.
	PaperclipServiceName = "paperclip"
	// BuzzServiceName is the Service in front of the orchestration agent when the tool is Buzz.
	BuzzServiceName = "buzz"
	// PaperclipIngressPolicyName admits afs-edge and the team's workers to the collaboration agent.
	PaperclipIngressPolicyName = "afs-paperclip-ingress"
	// PaperclipEgressPolicyName lets the team's pods open connections to the collaboration agent.
	PaperclipEgressPolicyName = "afs-paperclip-egress"

	edgeNameLabel = "app.kubernetes.io/name"
)

// toolOf is the orchestration tool an agent is rendered for: the team's tool for the orchestration agent and for
// a connected worker, Paperclip for a 1.0.1 row without one.
func toolOf(d agent.Desired) string {
	switch {
	case d.Collab != nil && d.Collab.Tool != "":
		return d.Collab.Tool
	case d.Link != nil && d.Link.Tool != "":
		return d.Link.Tool
	}
	return agent.ToolPaperclip
}

// CollabService renders the Service of the team namespace in front of the orchestration agent: named after the
// tool (`paperclip` or `buzz`), ClusterIP, the tool's port (3100 or 3000), selecting the agent's pod by its
// agent id label.
func CollabService(d agent.Desired) *unstructured.Unstructured {
	tool := toolOf(d)
	p := int64(agent.ToolPort(tool))
	u := New("v1", "Service", d.Team.Namespace, agent.ToolService(tool))
	u.Object["spec"] = map[string]any{
		"type":     "ClusterIP",
		"selector": map[string]any{LabelAgentID: d.ID},
		"ports":    []any{map[string]any{"name": "http", "port": p, "targetPort": p, "protocol": "TCP"}},
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
	ports := []any{map[string]any{"ports": []any{map[string]any{"port": port(agent.ToolPort(toolOf(d))), "protocol": "TCP"}}}}
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
			"toPorts":     []any{map[string]any{"ports": []any{map[string]any{"port": port(agent.ToolPort(toolOf(d))), "protocol": "TCP"}}}},
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
	if c.Tool == agent.ToolBuzz {
		return BuzzEnv(d, c)
	}
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
		"AFS_COLLAB_TOOL":               agent.ToolPaperclip,
		"AFS_COLLAB_URL":                "http://" + internal + ":3100",
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

// BuzzEnv is the environment of the orchestration agent when the tool is Buzz (ORC-12): the relay listens on
// 3000 and keeps its PostgreSQL and Redis data on the agent's volume. Buzz has no outside access, so there is
// no public URL; the relay answers on the in-namespace Service only. The relay's own secrets arrive as bound
// secrets (the boot secrets written by the API).
func BuzzEnv(d agent.Desired, c agent.Collab) map[string]string {
	env := map[string]string{
		"PORT":                "3000",
		"HOST":                "0.0.0.0",
		"BUZZ_HOME":           DataDir + "/buzz",
		"BUZZ_RELAY_HOSTNAME": agent.ToolService(agent.ToolBuzz) + "." + d.Team.Namespace + ".svc.cluster.local",
		"AFS_BUZZ_BOOTSTRAP":  c.Mode,
		"AFS_COLLAB_TOOL":     agent.ToolBuzz,
		"AFS_COLLAB_URL":      "ws://" + agent.ToolService(agent.ToolBuzz) + "." + d.Team.Namespace + ".svc.cluster.local:3000",
	}
	if c.SecretsPrefix != "" {
		env["BUZZ_SECRETS_PREFIX"] = c.SecretsPrefix
	}
	return env
}

// CollabWorkerEnv is what a connected worker receives (ORC-9): the tool-neutral AFS_COLLAB_TOOL and
// AFS_COLLAB_URL (the in-namespace address), plus the tool's own variables. Paperclip: PAPERCLIP_API_URL and,
// once registered, PAPERCLIP_AGENT_ID (the key arrives as the bound secret PAPERCLIP_AGENT_API_KEY). Buzz:
// BUZZ_RELAY_URL (BUZZ_PRIVATE_KEY arrives as the bound secret buzz-agent-private-key).
func CollabWorkerEnv(d agent.Desired, l agent.CollabLink) map[string]string {
	tool := l.Tool
	if tool == "" {
		tool = agent.ToolPaperclip
	}
	host := agent.ToolService(tool) + "." + d.Team.Namespace + ".svc.cluster.local"
	if tool == agent.ToolBuzz {
		url := fmt.Sprintf("ws://%s:%d", host, agent.BuzzPort)
		return map[string]string{"AFS_COLLAB_TOOL": tool, "AFS_COLLAB_URL": url, "BUZZ_RELAY_URL": url}
	}
	url := fmt.Sprintf("http://%s:%d", host, agent.CollabPort)
	env := map[string]string{"AFS_COLLAB_TOOL": tool, "AFS_COLLAB_URL": url, "PAPERCLIP_API_URL": url}
	if l.PaperclipAgentID != "" {
		env["PAPERCLIP_AGENT_ID"] = l.PaperclipAgentID
	}
	return env
}

// PaperclipWorkerEnv is CollabWorkerEnv for a Paperclip team (kept for 1.0.1 callers).
func PaperclipWorkerEnv(d agent.Desired, l agent.CollabLink) map[string]string {
	l.Tool = agent.ToolPaperclip
	return CollabWorkerEnv(d, l)
}

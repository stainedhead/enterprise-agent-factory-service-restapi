package render_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Gateway contract §13.5: on a team gateway an agent that reaches MCP servers gets AFS_MCP_* after AFS_GATEWAY_URL
// and /etc/afs/mcp.json; the delivery changes the pod-template hash. Without a server, or on the shared gateway,
// nothing is delivered.
func TestAgentMcpDelivery(t *testing.T) {
	d := agentDesired()
	d.Team.GatewayDeployment = agent.GatewayPerNamespace
	d.Mcp = agent.McpAccess{Mode: "search", Servers: []agent.McpServerRef{{Slug: "jira", Kind: "team", ReadOnly: true}, {Slug: "git-hub", Kind: "enterprise"}}}
	env := environment(d)
	env.Gateway = env.Gateway.For(d.Team)
	pod := render.PodTemplate(env)
	e := envOf(pod)
	if e["AFS_MCP_URL"].Value != "$(AFS_GATEWAY_URL)/mcp" || e["AFS_MCP_MODE"].Value != "search" || e["AFS_MCP_SERVERS"].Value != "git-hub,jira" ||
		e["AFS_MCP_GIT_HUB_URL"].Value != "$(AFS_GATEWAY_URL)/mcp/git-hub" {
		t.Errorf("env = %+v", e)
	}
	names := make([]string, 0)
	for _, v := range pod.Spec.Containers[0].Env {
		names = append(names, v.Name)
	}
	if slices.Index(names, "AFS_MCP_URL") < slices.Index(names, "AFS_GATEWAY_URL") {
		t.Error("AFS_MCP_URL must follow AFS_GATEWAY_URL")
	}
	if _, ok := mountOf(pod)[render.McpPath]; !ok {
		t.Error("mcp.json not mounted")
	}
	data, _ := render.AgentAWSConfigMap(d).Object["data"].(map[string]any)
	raw, _ := data["mcp.json"].(string)
	var file struct {
		Version    int               `json:"version"`
		Aggregated map[string]string `json:"aggregated"`
		Servers    []struct {
			Name     string `json:"name"`
			ReadOnly bool   `json:"readOnly"`
		} `json:"servers"`
		McpServers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &file); err != nil || file.Version != 1 || file.Aggregated["mode"] != "search" ||
		len(file.Servers) != 2 || file.Servers[0].Name != "git-hub" || !file.Servers[1].ReadOnly ||
		file.McpServers["afs"].URL != "https://afs-gateway.afs-t-alpha.svc:8443/mcp" || file.McpServers["afs"].Type != "http" {
		t.Errorf("mcp.json = %s (%v)", raw, err)
	}
	plain := d
	plain.Mcp = agent.McpAccess{}
	if agent.PodTemplateHash(d) == agent.PodTemplateHash(plain) {
		t.Error("MCP does not change the pod-template hash")
	}
	if agent.McpHash(plain.Team, plain.Mcp) != "" || agent.McpEnv(plain.Team, plain.Mcp) != nil {
		t.Error("an agent without servers gets MCP")
	}
	shared := agentDesired()
	shared.Mcp = d.Mcp
	if _, ok := envOf(render.PodTemplate(environment(shared)))["AFS_MCP_URL"]; ok {
		t.Error("a team on the shared gateway gets MCP")
	}
}

func mcpGatewayEnv() team.Environment {
	arn := "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/afs-t-x/_gateway/mcp/jira-AbCdEf" //nolint:gosec // fixture ARN
	env := gatewayEnv(team.TeamGateway{Deployment: team.DeploymentPerNamespace, Mcp: team.McpGateway{Enabled: true, Servers: []team.McpServer{
		{ID: "mcp_1", Slug: "jira", Location: "external", URL: "https://jira.example.com/mcp", AuthKind: "static", CredentialARN: arn, CredentialState: "set"},
		{ID: "mcp_2", Slug: "fed", Location: "corporate", URL: "https://tools.corp.example:8443/mcp", AuthKind: "workload_federation", //nolint:gosec // test fixture
			Audience: "api://tools", TokenEndpoint: "https://sts.corp.example/token"},
		{ID: "mcp_3", Slug: "files", Location: "in_namespace", AuthKind: "static", CredentialARN: arn, CredentialState: "expired",
			Hosted: &team.HostedServer{Image: "registry.example.com/files@sha256:" + strings.Repeat("b", 64), Args: []string{"/app/server", "--root", "/data"},
				EnvNames: []string{"TOKEN"}, InstanceMode: "per_team", EgressHosts: []string{"api.files.example", "*.cdn.example"},
				Resources: map[string]string{"cpu": "250m", "memory": "256Mi"}}, DriftPending: 2},
	}}})
	env.TeamGateway.McpSessionKeys = [][]byte{[]byte(strings.Repeat("k", 32))}
	env.TeamGateway.McpBridgeImage = "registry.example.com/afs-mcp-bridge@sha256:" + strings.Repeat("c", 64)
	return env
}

// MCP-RA-6: the team gateway reaches each server's host (and a federation token endpoint), the hosted server's
// bridge port in the namespace, gets mcp.<slug> credential keys, one projected token per audience and its session
// keys from a Secret.
func TestTeamGatewayMcp(t *testing.T) {
	env := mcpGatewayEnv()
	objs, err := render.TeamGateway(env)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := json.Marshal(objs)
	for _, want := range []string{`"secretKey":"mcp.jira"`, `"secretKey":"mcp.files"`, `"matchName":"jira.example.com"`, `"matchName":"tools.corp.example"`,
		`"matchName":"sts.corp.example"`, `"port":"8080"`, `"audience":"api://tools"`, `"path":"api-tools/token"`, `"AFS_GATEWAY_MCP_SEALING_KEYS"`,
		domain.McpGatewayFederationRoot, `"sealing-keys":"` + team.SessionKeys(env.TeamGateway.McpSessionKeys, env.Team.ID) + `"`} {
		if !strings.Contains(string(all), want) {
			t.Errorf("missing %s", want)
		}
	}
	if team.SessionKeys(env.TeamGateway.McpSessionKeys, "team_a") == team.SessionKeys(env.TeamGateway.McpSessionKeys, "team_b") {
		t.Error("two teams share session keys")
	}
	env.Team.Gateway.Mcp.Enabled = false
	off, _ := render.TeamGateway(env)
	b, _ := json.Marshal(off)
	if strings.Contains(string(b), "mcp.jira") || strings.Contains(string(b), "SEALING") || strings.Contains(string(b), "jira.example.com") {
		t.Error("MCP objects while mcp.enabled is off")
	}
	conds := mcpGatewayEnv().Team.Gateway.McpConditions()
	byType := func(s team.McpStatus, typ string) domain.Condition {
		for _, c := range s.Conditions {
			if c.Type == typ {
				return c
			}
		}
		return domain.Condition{}
	}
	if len(conds) != 3 || conds[0].Slug != "fed" || byType(conds[0], "McpServerReady").Status != domain.ConditionTrue ||
		byType(conds[1], "McpAuthExpired").Status != domain.ConditionTrue || byType(conds[1], "McpDriftPending").Status != domain.ConditionTrue ||
		byType(conds[1], "McpServerReady").Reason != team.ReasonMcpCredentialExpired || byType(conds[2], "McpServerReady").Status != domain.ConditionTrue {
		t.Errorf("conditions = %+v", conds)
	}
}

// MCP PRD §10.2: a hosted server runs as afs-mcp-<slug> with the bridge copied in by an init container, the
// server as the bridge's child, a restricted pod, its credential as environment, ingress from the gateway only and
// egress to its declared hosts.
func TestMcpHostedRender(t *testing.T) {
	env := mcpGatewayEnv()
	objs, err := render.McpHosted(env)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, o := range objs {
		kinds[o.GetKind()] = true
		if o.GetName() != "afs-mcp-files" || o.GetLabels()[render.LabelMcpServer] != "files" || !render.IsManaged(o) {
			t.Errorf("%s %s labels %v", o.GetKind(), o.GetName(), o.GetLabels())
		}
	}
	if len(objs) != 4 || !kinds["Deployment"] || !kinds["Service"] || !kinds["CiliumNetworkPolicy"] || !kinds["ExternalSecret"] {
		t.Fatalf("objects = %v", kinds)
	}
	b, _ := json.Marshal(objs)
	for _, want := range []string{`"--install","/afs/bin"`, `"command":["/afs/bin/afs-mcp-bridge","--listen",":8080","--pass-env","TOKEN","--","/app/server","--root","/data"]`,
		`"readOnlyRootFilesystem":true`, `"automountServiceAccountToken":false`, `"matchName":"api.files.example"`, `"matchPattern":"*.cdn.example"`,
		`"app.kubernetes.io/name":"afs-gateway"`, `"cpu":"250m"`, `"extract"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s", want)
		}
	}
	if s := render.McpHostedSlugs(env); len(s) != 1 || s[0] != "files" {
		t.Errorf("slugs = %v", s)
	}
	env.Team.Gateway.Mcp.Servers[2].Suspended = true
	env.Team.Gateway.Mcp.Servers[2].Hosted.NeedsAWS = true
	sus, _ := render.McpHosted(env)
	b, _ = json.Marshal(sus)
	if !strings.Contains(string(b), `"replicas":0`) || !strings.Contains(string(b), `"serviceAccountName":"afs-mcp-files"`) || len(sus) != 5 {
		t.Errorf("suspended with AWS = %s", b)
	}
	env.Team.Gateway.Mcp.Servers[2].Hosted.Resources = map[string]string{"cpu": "lots"}
	if _, err := render.McpHosted(env); err == nil {
		t.Error("a bad quantity rendered")
	}
	env.TeamGateway.McpBridgeImage = ""
	if o, _ := render.McpHosted(env); o != nil || render.McpHostedSlugs(env) != nil {
		t.Error("hosted servers without a bridge image")
	}
}

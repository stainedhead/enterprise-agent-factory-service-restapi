package agent_test

import (
	"encoding/json"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

func TestMcpDelivery(t *testing.T) {
	tm := agent.Team{GatewayDeployment: agent.GatewayPerNamespace, Namespace: "afs-t-a"}
	m := agent.McpAccess{Mode: "full", Servers: []agent.McpServerRef{{Slug: "b", Kind: "team"}, {Slug: "a", Kind: "enterprise", ReadOnly: true}}}
	env := agent.McpEnv(tm, m)
	if len(env) != 5 || env[2].Value != "a,b" || env[3].Name != "AFS_MCP_A_URL" || env[1].Value != "full" {
		t.Errorf("env = %+v", env)
	}
	var f struct {
		Aggregated map[string]string `json:"aggregated"`
		Servers    []struct {
			Name, URL string
			ReadOnly  bool `json:"readOnly"`
		} `json:"servers"`
	}
	raw := agent.McpJSON("https://gw", m)
	if err := json.Unmarshal([]byte(raw), &f); err != nil || f.Aggregated["url"] != "https://gw/mcp" || f.Servers[0].Name != "a" ||
		f.Servers[0].URL != "https://gw/mcp/a" || !f.Servers[0].ReadOnly {
		t.Errorf("mcp.json = %s %v", raw, err)
	}
	if agent.McpHash(tm, m) == "" || agent.McpHash(agent.Team{}, m) != "" || agent.McpEnv(agent.Team{}, m) != nil {
		t.Error("delivery on the shared gateway")
	}
}

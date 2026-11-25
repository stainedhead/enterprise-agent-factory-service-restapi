package agent

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// MCP delivery to an agent or pool (gateway contract §13.5, MCP-RA-6): the AFS_MCP_* variables and
// /etc/afs/mcp.json, on a team gateway only and only when the subject reaches at least one server. No MCP
// credential is ever delivered to a pod.

// McpServerRef is one server the subject reaches.
type McpServerRef struct {
	Slug string
	// Kind is enterprise or team.
	Kind string
	// ReadOnly is the team's read_only mode of the server.
	ReadOnly bool
}

// McpAccess is the subject's effective MCP servers and mode.
type McpAccess struct {
	// Mode is search or full.
	Mode    string
	Servers []McpServerRef
}

// McpDelivered reports whether the subject receives MCP variables and the file.
func McpDelivered(t Team, m McpAccess) bool { return Delivered(t) && len(m.Servers) > 0 }

func (m McpAccess) slugs() []string {
	out := make([]string, 0, len(m.Servers))
	for _, s := range m.Servers {
		out = append(out, s.Slug)
	}
	return out
}

// McpEnv is the AFS_MCP_* variables in render order; they reference $(AFS_GATEWAY_URL), so they must follow it.
func McpEnv(t Team, m McpAccess) []domain.EnvVar {
	if !McpDelivered(t, m) {
		return nil
	}
	return domain.McpEnvVars(domain.McpAgentMode(m.Mode), m.slugs())
}

// McpJSON is /etc/afs/mcp.json with URLs on gatewayURL (the file cannot expand variables). The mcpServers block
// is the shape most MCP hosts read, so a runtime can point at the file without code.
func McpJSON(gatewayURL string, m McpAccess) string {
	type server struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Kind     string `json:"kind"`
		ReadOnly bool   `json:"readOnly"`
	}
	type host struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	agg := gatewayURL + "/mcp"
	servers := slices.Clone(m.Servers)
	slices.SortFunc(servers, func(a, b McpServerRef) int { return strings.Compare(a.Slug, b.Slug) })
	list := make([]server, 0, len(servers))
	for _, s := range servers {
		list = append(list, server{Name: s.Slug, URL: agg + "/" + s.Slug, Kind: s.Kind, ReadOnly: s.ReadOnly})
	}
	b, _ := json.Marshal(struct {
		Version    int               `json:"version"`
		Aggregated map[string]string `json:"aggregated"`
		Servers    []server          `json:"servers"`
		McpServers map[string]host   `json:"mcpServers"`
	}{1, map[string]string{"url": agg, "mode": m.Mode}, list,
		map[string]host{"afs": {Type: "http", URL: agg, Headers: map[string]string{}}}}) // plain values always marshal
	return string(b)
}

// McpHash is the pod-template hash line of the MCP delivery; empty when nothing is delivered, so the hash of an
// agent without MCP does not change.
func McpHash(t Team, m McpAccess) string {
	if !McpDelivered(t, m) {
		return ""
	}
	return "mcp=" + McpJSON("", m)
}

package team

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"slices"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// MCP servers on the team gateway (gateway contract §13, MCP PRD §10 and MCP-RA-6): the gateway reaches each
// remote server's host and each hosted server's Service, gets the stored credential of a server as the key
// mcp.<slug> of its credentials Secret, a projected token per workload federation audience, and a per-team
// session sealing key. Hosted servers run as afs-mcp-<slug> in the team namespace.

// McpServer is one server switched on for the team, as the reconciler sees it. No credential value: only the
// reference.
type McpServer struct {
	ID, Slug string
	// Location is external, corporate or in_namespace (hosted).
	Location string
	// URL is the remote server's endpoint; empty for a hosted server.
	URL string
	// AuthKind is none, static, oauth_client, workload_federation, oauth_user or aws_sigv4.
	AuthKind string
	// CredentialARN is the Secrets Manager reference the team's calls use (the team's, else the shared one); empty
	// when none is stored. CredentialState is set, missing or expired.
	CredentialARN, CredentialState string
	// Audience is the projected token audience of workload federation; TokenEndpoint is where it is exchanged.
	Audience, TokenEndpoint string
	// Hosted is the hosted spec of a hosted server.
	Hosted *HostedServer
	// Suspended is review_status suspended: the gateway refuses it; a hosted server is scaled to zero.
	Suspended bool
	// DriftPending counts primitives held for review.
	DriftPending int
}

// HostedServer is mcp_hosted_specs.
type HostedServer struct {
	Image        string
	Args         []string
	EnvNames     []string
	InstanceMode string
	EgressHosts  []string
	Resources    map[string]string
	NeedsAWS     bool
}

// McpGateway is the MCP part of a team gateway.
type McpGateway struct {
	// Enabled is the mcp.enabled service setting.
	Enabled bool
	Servers []McpServer
}

// needsStoredCredential reports whether the server's calls carry a stored credential.
func (s McpServer) needsStoredCredential() bool {
	switch s.AuthKind {
	case "static", "oauth_client", "oauth_user":
		return true
	}
	return false
}

// HostedServers lists the team's hosted servers, sorted by slug; none while MCP is off.
func (m McpGateway) HostedServers() []McpServer {
	if !m.Enabled {
		return nil
	}
	var out []McpServer
	for _, s := range m.Servers {
		if s.Hosted != nil {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b McpServer) int { return strings.Compare(a.Slug, b.Slug) })
	return out
}

// mcpEgress lists the gateway's MCP destinations: each remote server's host and, for hosted servers, the
// bridge port inside the namespace.
func (m McpGateway) mcpEgress() []EgressRule {
	if !m.Enabled {
		return nil
	}
	var out []EgressRule
	for _, s := range m.Servers {
		if s.Hosted != nil {
			out = append(out, EgressRule{Port: domain.McpHostedPort, InNamespace: true})
			continue
		}
		if h, p, ok := hostPort(s.URL); ok {
			out = append(out, EgressRule{Host: h, Port: p})
		}
		if h, p, ok := hostPort(s.TokenEndpoint); ok && s.AuthKind == "workload_federation" {
			out = append(out, EgressRule{Host: h, Port: p})
		}
	}
	return out
}

// mcpCredentials lists the mcp.<slug> keys of the gateway's credentials Secret.
func (m McpGateway) mcpCredentials() []Credential {
	if !m.Enabled {
		return nil
	}
	var out []Credential
	for _, s := range m.Servers {
		if s.needsStoredCredential() && s.CredentialARN != "" {
			out = append(out, Credential{Key: domain.McpCredentialKey(s.Slug), SecretARN: s.CredentialARN})
		}
	}
	return out
}

// Audiences lists the distinct workload federation audiences, sorted: one projected token each.
func (m McpGateway) Audiences() []string {
	if !m.Enabled {
		return nil
	}
	var out []string
	for _, s := range m.Servers {
		if s.AuthKind == "workload_federation" && s.Audience != "" {
			out = append(out, s.Audience)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// SessionKeys derives the team's MCP session sealing keys from the reconciler's keys (one, or two while
// rotating): HMAC-SHA256(key, "afs-mcp-session/" + teamID), base64, comma separated, the first sealing. Each team
// gets its own keys, so a session id sealed by one team's gateway does not open at another's. Empty when no key
// is configured.
func SessionKeys(keys [][]byte, teamID string) string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		mac := hmac.New(sha256.New, k)
		_, _ = mac.Write([]byte("afs-mcp-session/" + teamID))
		out = append(out, base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}
	return strings.Join(out, ",")
}

// MCP condition reasons (McpServerReady, McpAuthExpired, McpDriftPending; MCP-RA-6).
const (
	ReasonMcpReady             = "Ready"
	ReasonMcpOff               = "mcp_disabled"
	ReasonMcpCredentialMissing = "credential_missing" //nolint:gosec // a reason code
	ReasonMcpCredentialExpired = "credential_expired" //nolint:gosec // a reason code
	ReasonMcpSuspended         = "server_suspended"
	ReasonMcpDriftPending      = "primitives_pending_review"
)

// McpStatus is the conditions of one server for the team.
type McpStatus struct {
	ServerID, Slug string
	Conditions     []domain.Condition
}

// McpReporter records the MCP conditions of a team's servers (resource_status kind team_mcp_server). A
// RouteReporter that also implements it is called after the routes.
type McpReporter interface {
	ReportMcp(ctx context.Context, teamID string, servers []McpStatus) error
}

// McpConditions derives each server's conditions: McpServerReady (False while MCP is off, the gateway is shared,
// the server is suspended or its credential is missing or expired), McpAuthExpired (True when the credential
// expired, so a person must reconnect) and McpDriftPending (True while primitives wait for review).
func (g TeamGateway) McpConditions() []McpStatus {
	out := make([]McpStatus, 0, len(g.Mcp.Servers))
	for _, s := range g.Mcp.Servers {
		ready := domain.Condition{Type: "McpServerReady", Status: domain.ConditionTrue, Reason: ReasonMcpReady}
		switch {
		case !g.Mcp.Enabled:
			ready.Status, ready.Reason = domain.ConditionFalse, ReasonMcpOff
		case !g.PerNamespace():
			ready.Status, ready.Reason = domain.ConditionFalse, ReasonGatewayShared
		case s.Suspended:
			ready.Status, ready.Reason = domain.ConditionFalse, ReasonMcpSuspended
		case s.needsStoredCredential() && s.CredentialState == "expired":
			ready.Status, ready.Reason = domain.ConditionFalse, ReasonMcpCredentialExpired
		case s.needsStoredCredential() && (s.CredentialARN == "" || s.CredentialState != "set"):
			ready.Status, ready.Reason = domain.ConditionFalse, ReasonMcpCredentialMissing
		}
		auth := domain.Condition{Type: "McpAuthExpired", Status: domain.ConditionFalse, Reason: ReasonMcpReady}
		if s.needsStoredCredential() && s.CredentialState == "expired" {
			auth.Status, auth.Reason = domain.ConditionTrue, ReasonMcpCredentialExpired
		}
		drift := domain.Condition{Type: "McpDriftPending", Status: domain.ConditionFalse, Reason: ReasonMcpReady}
		if s.DriftPending > 0 {
			drift.Status, drift.Reason = domain.ConditionTrue, ReasonMcpDriftPending
		}
		out = append(out, McpStatus{ServerID: s.ID, Slug: s.Slug, Conditions: []domain.Condition{ready, auth, drift}})
	}
	slices.SortFunc(out, func(a, b McpStatus) int { return strings.Compare(a.Slug, b.Slug) })
	return out
}

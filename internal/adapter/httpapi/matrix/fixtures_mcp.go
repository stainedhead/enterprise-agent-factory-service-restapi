package matrix

import (
	"context"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of release 1.3.0 (contract v1.6.0 §4.14): governed MCP servers.

const (
	mcpRequestNoEvent = "§4.12: a refresh or federation test only sets a request timestamp the gateway acts on"
	mcpDigest         = "sha256:" + "5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e"
)

// mcpServerSeed describes a server row a case needs.
type mcpServerSeed struct {
	team      bool   // a team A server; otherwise enterprise
	status    string // review_status
	published bool
	auth      string // auth_kind, default static
}

// mcpTeamReady lets team A add servers and switch enterprise ones on (a per-namespace gateway), for the subtest.
func (w *World) mcpTeamReady(t *testing.T) {
	t.Helper()
	var dep string
	if err := w.Admin.QueryRow(t.Context(), `SELECT gateway_deployment FROM afs.teams WHERE id = $1`, w.TeamA.ID).Scan(&dep); err != nil {
		t.Fatalf("matrix prepare: %v", err)
	}
	w.exec(t, `UPDATE afs.teams SET gateway_deployment = 'per_namespace', may_add_mcp_servers = true WHERE id = $1`, w.TeamA.ID)
	t.Cleanup(func() {
		_, _ = w.Admin.Exec(context.WithoutCancel(t.Context()),
			`UPDATE afs.teams SET gateway_deployment = $2, may_add_mcp_servers = false WHERE id = $1`, w.TeamA.ID, dep)
	})
}

// seedMcpServer inserts a server and returns its placeholders: mcpServerId, slug, ver.
func (w *World) seedMcpServer(t *testing.T, s mcpServerSeed) map[string]string {
	t.Helper()
	id, slug := pgtest.NewID("mcp"), "mx-"+strings.ToLower(unique())
	var team any
	if s.team {
		team = w.TeamA.ID
	}
	if s.auth == "" {
		s.auth = "static"
	}
	w.exec(t, `INSERT INTO afs.mcp_servers (id, team_id, slug, display_name, owner, purpose, data_classification, transport, location, url, auth_kind,
  review_status, published, risk_tier)
VALUES ($1, $2, $3, 'Matrix', 'Platform', 'Matrix checks', 'internal', 'streamable_http', 'corporate', 'https://mx.corp.example/mcp', $4, $5, $6, 'low')`,
		id, team, slug, s.auth, s.status, s.published)
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		_, _ = w.Admin.Exec(ctx, `DELETE FROM afs.mcp_servers WHERE id = $1`, id)
	})
	return map[string]string{"mcpServerId": id, "slug": slug, "ver": ver(t, w, `SELECT version FROM afs.mcp_servers WHERE id = $1`, id)}
}

// seedMcpPrimitive adds a tool pending review to the server named by p.
func (w *World) seedMcpPrimitive(t *testing.T, p map[string]string) map[string]string {
	t.Helper()
	id := pgtest.NewID("mpr")
	w.exec(t, `INSERT INTO afs.mcp_primitives (id, server_id, kind, name, annotations, definition, digest, status)
VALUES ($1, $2, 'tool', 'search', '{"readOnlyHint": true, "openWorldHint": false}',
  '{"name": "search", "annotations": {"readOnlyHint": true, "openWorldHint": false}}', $3, 'pending_review')`, id, p["mcpServerId"], mcpDigest)
	p["primitiveId"] = id
	return p
}

func mcpPrep(s mcpServerSeed) func(*testing.T, *World) map[string]string {
	return func(t *testing.T, w *World) map[string]string {
		if s.team {
			w.mcpTeamReady(t)
		}
		return w.seedMcpServer(t, s)
	}
}

func mcpPrimPrep(s mcpServerSeed) func(*testing.T, *World) map[string]string {
	return func(t *testing.T, w *World) map[string]string {
		if s.team {
			w.mcpTeamReady(t)
		}
		return w.seedMcpPrimitive(t, w.seedMcpServer(t, s))
	}
}

// seedMcpCapture closes any open query capture window and opens one, so StopMcpQueryCapture has one to stop.
func (w *World) seedMcpCapture(t *testing.T, open bool) {
	t.Helper()
	w.exec(t, `UPDATE afs.mcp_query_captures SET stopped_at = now() WHERE stopped_at IS NULL`)
	if open {
		w.exec(t, `INSERT INTO afs.mcp_query_captures (id, started_by, until, reason) VALUES ($1, 'usr_mx', now() + interval '1 day', 'matrix')`,
			pgtest.NewID("mqc"))
	}
}

// seedFederated adds the federated identity mx-tools to an agent or pool of team A.
func (w *World) seedFederated(t *testing.T, kind string, p map[string]string) map[string]string {
	t.Helper()
	w.allowAudience(t)
	table, col, key, prefix := "afs.agent_federated_identities", "agent_id", p["agentId"], "afi"
	if kind == "pool" {
		table, col, key, prefix = "afs.pool_federated_identities", "pool_id", p["poolId"], "pfi"
	}
	w.exec(t, `INSERT INTO `+table+` (id, team_id, `+col+`, name, audience) VALUES ($1, $2, $3, 'mx-tools', 'api://mx-tools')`,
		pgtest.NewID(prefix), w.TeamA.ID, key)
	p["ver"] = ver(t, w, `SELECT version FROM `+table+` WHERE `+col+` = $1 AND name = 'mx-tools'`, key)
	return p
}

// allowAudience lets agents request api://mx-tools for the subtest.
func (w *World) allowAudience(t *testing.T) {
	t.Helper()
	w.exec(t, `UPDATE afs.service_settings SET value = '["api://mx-*"]' WHERE key = 'federation.allowed_audiences'`)
	t.Cleanup(func() {
		_, _ = w.Admin.Exec(context.WithoutCancel(t.Context()), `UPDATE afs.service_settings SET value = '[]' WHERE key = 'federation.allowed_audiences'`)
	})
}

func init() {
	ifm := map[string]string{"If-Match": ifMatch}
	draft := mcpServerSeed{status: "draft"}
	pending := mcpServerSeed{status: "pending_review"}
	approved := mcpServerSeed{status: "approved"}
	published := mcpServerSeed{status: "approved", published: true}
	teamDraft := mcpServerSeed{team: true, status: "draft"}
	teamPending := mcpServerSeed{team: true, status: "pending_review"}
	teamApproved := mcpServerSeed{team: true, status: "approved"}
	remote := `"owner":"Platform","purpose":"Matrix checks","dataClassification":"internal","transport":"streamable_http","location":"corporate","url":"https://mx.corp.example/mcp","authKind":"none"`

	// The enterprise catalog.
	Register(Case{Op: "ListMcpServers", Path: "/v1/mcp-servers", Prepare: mcpPrep(draft)})
	Register(Case{Op: "GetMcpServer", Path: "/v1/mcp-servers/{mcpServerId}", Prepare: mcpPrep(draft)})
	Register(Case{Op: "CreateMcpServer", Path: "/v1/mcp-servers", Body: `{"slug":"mx-{n}","displayName":"Matrix",` + remote + `}`,
		Prepare: func(*testing.T, *World) map[string]string { return map[string]string{"n": strings.ToLower(unique())} }})
	Register(Case{Op: "ImportMcpServerManifest", Path: "/v1/mcp-servers:import",
		Body:    `{"slug":"mx-{n}","manifest":{"name":"io.example/matrix","remotes":[{"type":"streamable-http","url":"https://mx.example.com/mcp"}]}}`,
		Prepare: func(*testing.T, *World) map[string]string { return map[string]string{"n": strings.ToLower(unique())} }})
	Register(Case{Op: "UpdateMcpServer", Path: "/v1/mcp-servers/{mcpServerId}", Body: `{"purpose":"matrix"}`, Headers: ifm, Prepare: mcpPrep(draft)})
	Register(Case{Op: "DeleteMcpServer", Path: "/v1/mcp-servers/{mcpServerId}", Headers: ifm, Prepare: mcpPrep(draft)})
	Register(Case{Op: "PublishMcpServer", Path: "/v1/mcp-servers/{mcpServerId}:publish", Prepare: mcpPrep(approved)})
	Register(Case{Op: "RetireMcpServer", Path: "/v1/mcp-servers/{mcpServerId}:retire", Prepare: mcpPrep(published)})
	Register(Case{Op: "SuspendMcpServer", Path: "/v1/mcp-servers/{mcpServerId}:suspend", Body: `{"reason":"matrix"}`, Prepare: mcpPrep(approved)})
	Register(Case{Op: "ResumeMcpServer", Path: "/v1/mcp-servers/{mcpServerId}:resume", Body: `{"reason":"matrix"}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			p := w.seedMcpServer(t, approved)
			w.exec(t, `UPDATE afs.mcp_servers SET review_status = 'suspended', suspended_reason = 'matrix' WHERE id = $1`, p["mcpServerId"])
			return p
		}})
	Register(Case{Op: "RefreshMcpInventory", Path: "/v1/mcp-servers/{mcpServerId}:refresh", NoOutbox: mcpRequestNoEvent, Prepare: mcpPrep(approved)})
	Register(Case{Op: "ListMcpPrimitives", Path: "/v1/mcp-servers/{mcpServerId}/primitives", Prepare: mcpPrimPrep(approved)})
	Register(Case{Op: "GetMcpPrimitiveChange", Path: "/v1/mcp-servers/{mcpServerId}/primitives/{primitiveId}/change", Prepare: mcpPrimPrep(approved)})
	Register(Case{Op: "ApproveMcpPrimitive", Path: "/v1/mcp-servers/{mcpServerId}/primitives/{primitiveId}:approve", Body: `{"note":"matrix"}`,
		Prepare: mcpPrimPrep(approved)})
	Register(Case{Op: "RejectMcpPrimitive", Path: "/v1/mcp-servers/{mcpServerId}/primitives/{primitiveId}:reject", Body: `{"note":"matrix"}`,
		Prepare: mcpPrimPrep(approved)})
	Register(Case{Op: "PutMcpPolicy", Path: "/v1/mcp-servers/{mcpServerId}/policy", Body: `{"rules":[{"name":"delete_*","effect":"block"}]}`,
		Prepare: mcpPrep(approved)})
	Register(Case{Op: "PutMcpServerCredential", Path: "/v1/mcp-servers/{mcpServerId}/credentials/{teamId}", Body: `{"value":"mx-token"}`,
		Prepare: mcpPrep(approved)})

	// Review.
	for _, c := range []struct{ op, action string }{
		{"ApproveMcpServer", "approve"}, {"RejectMcpServer", "reject"}, {"RequestMcpServerChanges", "request-changes"},
	} {
		Register(Case{Op: c.op, Path: "/v1/mcp-servers/{mcpServerId}:" + c.action, Body: `{"note":"matrix"}`, Prepare: mcpPrep(pending)})
	}
	Register(Case{Op: "ListMcpReviews", Path: "/v1/mcp-reviews", Prepare: mcpPrep(pending)})

	// The ledger and query capture.
	Register(Case{Op: "ListMcpEvents", Path: "/v1/mcp-events"})
	Register(Case{Op: "GetMcpUsageReport", Path: "/v1/reports/mcp"})
	Register(Case{Op: "GetMcpQueryCapture", Path: "/v1/mcp-settings/query-capture"})
	Register(Case{Op: "StartMcpQueryCapture", Path: "/v1/mcp-settings/query-capture:start", Body: `{"days":1,"reason":"matrix"}`,
		Prepare: func(t *testing.T, w *World) map[string]string { w.seedMcpCapture(t, false); return nil }})
	Register(Case{Op: "StopMcpQueryCapture", Path: "/v1/mcp-settings/query-capture:stop",
		Prepare: func(t *testing.T, w *World) map[string]string { w.seedMcpCapture(t, true); return nil }})

	// Team servers and switches.
	Register(Case{Op: "ListTeamMcpServers", Path: "/v1/teams/{teamId}/mcp-servers", Prepare: mcpPrep(published)})
	Register(Case{Op: "GetTeamMcpServer", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}", Prepare: mcpPrep(published)})
	Register(Case{Op: "PutTeamMcpServer", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}", Body: `{"enabled":true,"mode":"full"}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.mcpTeamReady(t)
			return w.seedMcpServer(t, published)
		}})
	Register(Case{Op: "CreateTeamMcpServer", Path: "/v1/teams/{teamId}/mcp-servers", Body: `{"slug":"mx-{n}","displayName":"Matrix",` + remote + `}`,
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.mcpTeamReady(t)
			return map[string]string{"n": strings.ToLower(unique())}
		}})
	Register(Case{Op: "UpdateTeamMcpServer", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}", Body: `{"purpose":"matrix"}`, Headers: ifm,
		Prepare: mcpPrep(teamDraft)})
	Register(Case{Op: "DeleteTeamMcpServer", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}", Headers: ifm, Prepare: mcpPrep(teamDraft)})
	Register(Case{Op: "SubmitMcpServerForReview", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}:submit", Prepare: mcpPrep(teamDraft)})
	Register(Case{Op: "ApproveTeamMcpServer", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}:approve", Body: `{"note":"matrix"}`,
		Prepare: mcpPrep(teamPending)})
	Register(Case{Op: "RejectTeamMcpServer", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}:reject", Body: `{"note":"matrix"}`,
		Prepare: mcpPrep(teamPending)})
	Register(Case{Op: "RefreshTeamMcpInventory", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}:refresh", NoOutbox: mcpRequestNoEvent,
		Prepare: mcpPrep(teamApproved)})
	Register(Case{Op: "ListTeamMcpPrimitives", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}/primitives", Prepare: mcpPrimPrep(teamApproved)})
	Register(Case{Op: "ApproveTeamMcpPrimitive", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}/primitives/{primitiveId}:approve",
		Body: `{"note":"matrix"}`, Prepare: mcpPrimPrep(teamApproved)})
	Register(Case{Op: "RejectTeamMcpPrimitive", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}/primitives/{primitiveId}:reject",
		Body: `{"note":"matrix"}`, Prepare: mcpPrimPrep(teamApproved)})
	Register(Case{Op: "PutTeamMcpPolicy", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}/policy", Body: `{"rules":[{"name":"delete_*","effect":"block"}]}`,
		Prepare: mcpPrep(teamApproved)})
	Register(Case{Op: "PutTeamMcpServerCredential", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}/credential", Body: `{"value":"mx-token"}`,
		Prepare: mcpPrep(teamApproved)})
	Register(Case{Op: "DisconnectMcpOAuth", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}/oauth:disconnect",
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.mcpTeamReady(t)
			p := w.seedMcpServer(t, mcpServerSeed{team: true, status: "approved", auth: "oauth_user"})
			w.exec(t, `INSERT INTO afs.mcp_credentials (id, server_id, scope, team_id, kind, secret_arn, subject, status)
VALUES ($1, $2, $3, $3, 'oauth_user', 'arn:aws:secretsmanager:us-east-1:000000000000:secret:' || afs.mcp_credential_path($3, $4) || '-AbCdEf', 'svc-mx', 'set')`,
				pgtest.NewID("mcr"), p["mcpServerId"], w.TeamA.ID, p["slug"])
			return p
		}})
	Register(Case{Op: "TestMcpFederation", Path: "/v1/teams/{teamId}/mcp-servers/{mcpServerId}/federation:test", NoOutbox: mcpRequestNoEvent,
		Prepare: func(t *testing.T, w *World) map[string]string {
			w.mcpTeamReady(t)
			p := w.seedMcpServer(t, mcpServerSeed{team: true, status: "approved"})
			w.exec(t, `UPDATE afs.mcp_servers SET auth_kind = 'workload_federation',
  federation = '{"issuer": "https://oidc.eks.example/id/mx", "audience": "api://mx-tools", "tokenEndpoint": "https://sts.mx.example/token"}' WHERE id = $1`,
				p["mcpServerId"])
			return p
		}})

	// Agents, pools and the ledger of team A.
	Register(Case{Op: "GetEffectiveAgentMcp", Path: "/v1/teams/{teamId}/agents/{agentId}/mcp", Prepare: agentPrep})
	Register(Case{Op: "GetEffectivePoolMcp", Path: "/v1/teams/{teamId}/pools/{poolId}/mcp", Prepare: poolPrep})
	Register(Case{Op: "ListTeamMcpEvents", Path: "/v1/teams/{teamId}/mcp-events"})
	Register(Case{Op: "GetTeamMcpUsageReport", Path: "/v1/teams/{teamId}/reports/mcp"})

	// Federated identities.
	for _, k := range []struct{ kind, op, path string }{
		{"agent", "Agent", "/v1/teams/{teamId}/agents/{agentId}/federated-identities"},
		{"pool", "Pool", "/v1/teams/{teamId}/pools/{poolId}/federated-identities"},
	} {
		base := agentPrep
		if k.kind == "pool" {
			base = poolPrep
		}
		withFed := func(t *testing.T, w *World) map[string]string { return w.seedFederated(t, k.kind, base(t, w)) }
		Register(Case{Op: "List" + k.op + "FederatedIdentities", Path: k.path, Prepare: withFed})
		Register(Case{Op: "Put" + k.op + "FederatedIdentity", Path: k.path + "/mx-new", Body: `{"audience":"api://mx-tools"}`,
			Prepare: func(t *testing.T, w *World) map[string]string { w.allowAudience(t); return base(t, w) }})
		Register(Case{Op: "Delete" + k.op + "FederatedIdentity", Path: k.path + "/mx-tools", Headers: ifm, Prepare: withFed})
	}
}

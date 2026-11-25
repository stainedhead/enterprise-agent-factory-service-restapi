package main

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

const testConsoleURL = "https://console.afs.example"

// testMcpOAuth configures the OAuth connect flow against the matrix's fake authorization server.
func testMcpOAuth(w *matrix.World) mcpOAuthConfig {
	return mcpOAuthConfig{ClientID: "https://afs.example/oauth/client.json", RedirectURI: "https://api.afs.example/v1/mcp-oauth/callback",
		ConsoleURL: testConsoleURL, HTTP: w.OIDCClient()}
}

// MCP gateway PRD §9 case 5 and API contract CD-48: a team admin connects a service account. The start answers an
// authorization URL with PKCE S256 and the resource; the unauthenticated callback claims the state once, checks the
// issuer, redeems the code with the derived verifier, stores the refresh token in Secrets Manager (an ARN only in the
// database) and redirects to the console. A replayed or forged state, or a mixed-up issuer, stores nothing.
func TestMcpOAuthConnectEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, withCluster(settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6, AWSRegion: "us-east-1", ClusterOIDCIssuer: w.Cluster.Server.URL,
		McpOAuth: testMcpOAuth(w)}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	g := secopsRig{w, c.Handler}
	as := matrix.FakeOAuthAS()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.Admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	exec(`UPDATE afs.teams SET gateway_deployment = 'per_namespace', may_add_mcp_servers = true WHERE id = $1`, w.TeamA.ID)
	srv, slug := pgtest.NewID("mcp"), "oauth-"+strings.ToLower(pgtest.NewID("x")[4:12])
	exec(`INSERT INTO afs.mcp_servers (id, team_id, slug, display_name, owner, purpose, data_classification, transport, location, url, auth_kind,
  review_status, risk_tier) VALUES ($1, $2, $3, 'SaaS', 'Platform', 'Tickets', 'internal', 'streamable_http', 'external', $4, 'oauth_user', 'approved', 'low')`,
		srv, w.TeamA.ID, slug, as.Server.URL+"/mcp")
	admin := w.Tokens[matrix.TeamAdmin]
	start := func() (string, url.Values) {
		t.Helper()
		r := g.do(t, "POST", "/v1/teams/"+w.TeamA.ID+"/mcp-servers/"+srv+"/oauth:start", admin, "")
		if r.code != 200 {
			t.Fatalf("start: %d %s", r.code, r.body)
		}
		var out struct{ AuthorizationURL, State, ExpiresAt string }
		if err := json.Unmarshal([]byte(r.body), &out); err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(out.AuthorizationURL)
		if !strings.HasPrefix(out.AuthorizationURL, as.Issuer()+"/authorize?") || u.Query().Get("code_challenge_method") != "S256" ||
			u.Query().Get("resource") != as.Server.URL+"/mcp" || u.Query().Get("state") != out.State {
			t.Fatalf("authorization URL = %s", out.AuthorizationURL)
		}
		return out.State, u.Query()
	}
	callback := func(q string) (int, string) {
		t.Helper()
		r := g.do(t, "GET", "/v1/mcp-oauth/callback?"+q, "", "")
		return r.code, r.hdr.Get("Location")
	}
	credentials := func() int {
		return g.count(t, `SELECT count(*) FROM afs.mcp_credentials WHERE server_id = $1`, srv)
	}

	if r := g.do(t, "POST", "/v1/teams/"+w.TeamA.ID+"/mcp-servers/"+srv+"/oauth:start", w.Tokens[matrix.Viewer], ""); r.code != 403 {
		t.Errorf("viewer start = %d", r.code)
	}

	// A mixed-up issuer (RFC 9207) burns the state and stores nothing.
	state, _ := start()
	code, loc := callback(url.Values{"code": {"c0"}, "state": {state}, "iss": {"https://evil.example"}}.Encode())
	if code != 303 || !strings.Contains(loc, "reason=issuer_mismatch") || credentials() != 0 {
		t.Fatalf("issuer mix-up: %d %s", code, loc)
	}

	state, q := start()
	code, loc = callback(url.Values{"code": {"c1"}, "state": {state}, "iss": {as.Issuer()}}.Encode())
	if code != 303 || loc != testConsoleURL+"/teams/"+w.TeamA.ID+"/mcp-servers/"+srv+"?mcpOAuth=connected" {
		t.Fatalf("callback: %d %s", code, loc)
	}
	f := as.Form()
	if f["code"] != "c1" || f["code_verifier"] == "" || f["client_id"] != "https://afs.example/oauth/client.json" || f["resource"] != as.Server.URL+"/mcp" {
		t.Errorf("token request = %v", f)
	}
	if q.Get("code_challenge") == f["code_verifier"] {
		t.Error("the challenge is the verifier")
	}
	var kind, arn, subject, status string
	if err := w.Admin.QueryRow(t.Context(), `SELECT kind, secret_arn, subject, status FROM afs.mcp_credentials WHERE server_id = $1`, srv).
		Scan(&kind, &arn, &subject, &status); err != nil || kind != "oauth_user" || status != "set" || !strings.Contains(arn, ":secret:") || subject == "" {
		t.Fatalf("credential = %s %s %s %s %v", kind, arn, subject, status, err)
	}
	if strings.Contains(arn, "rt-matrix") {
		t.Error("the refresh token reached the database")
	}
	if n := g.count(t, `SELECT count(*) FROM afs.audit_log WHERE action = 'CompleteMcpOAuthConnect' AND team_id = $1`, w.TeamA.ID); n != 1 {
		t.Errorf("callback audit rows = %d", n)
	}
	if n := g.count(t, `SELECT count(*) FROM afs.outbox_events WHERE event_type = 'mcp_server.credential_set' AND team_id = $1`, w.TeamA.ID); n != 1 {
		t.Errorf("credential_set events = %d", n)
	}

	// The state is single use; a forged one is refused the same way.
	for _, s := range []string{state, "forged"} {
		code, loc = callback(url.Values{"code": {"c2"}, "state": {s}}.Encode())
		if code != 303 || loc != testConsoleURL+"/mcp-servers?mcpOAuth=failed&reason=state_invalid" {
			t.Errorf("state %q: %d %s", s, code, loc)
		}
	}
	if r := g.do(t, "GET", "/v1/teams/"+w.TeamA.ID+"/mcp-servers/"+srv, admin, ""); !strings.Contains(r.body, `"credentialState":"set"`) {
		t.Errorf("team view after connect: %s", r.body)
	}
}

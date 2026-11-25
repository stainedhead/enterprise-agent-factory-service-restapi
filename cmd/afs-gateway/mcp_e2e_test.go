package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/gatewayhttp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpgateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpwire"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

func TestSealingKeys(t *testing.T) {
	k := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	if ks, err := sealingKeys(k + "," + k); err != nil || len(ks) != 2 {
		t.Errorf("two keys = %d %v", len(ks), err)
	}
	for _, bad := range []string{"short", k + "," + k + "," + k, "!!!"} {
		if _, err := sealingKeys(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if ks, err := sealingKeys(""); err != nil || ks != nil {
		t.Error("no key")
	}
}

// MCP gateway PRD §9 cases 1 and 2 on the team gateway's listener: an agent calls an approved tool of its team's
// server through /mcp/{server} and through the aggregated endpoint; the call reaches the upstream without the
// agent's token, is recorded in the ledger, and a held primitive stays invisible. With mcp.enabled off every MCP
// path is 404. The refresh lists the upstream and holds what is new.
func TestMcpRouteEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	admin := db.Admin(t)
	ctx := t.Context()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := admin.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	var gotAuth []string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization")+r.Header.Get(gatewayhttp.TokenHeader))
		var m mcpwire.Message
		_ = json.NewDecoder(r.Body).Decode(&m)
		w.Header().Set("Content-Type", "application/json")
		var res any
		switch m.Method {
		case "tools/call":
			res = map[string]any{"content": []map[string]any{{"type": "text", "text": "3 open issues"}}}
		case "tools/list":
			res = map[string]any{"tools": []any{toolDefinition("list_issues"), toolDefinition("delete_issue")}}
		default:
			_ = json.NewEncoder(w).Encode(mcpwire.NewError(m.ID, mcpwire.CodeMethodNotFound, "no", nil))
			return
		}
		b, _ := json.Marshal(res)
		_ = json.NewEncoder(w).Encode(mcpwire.Message{JSONRPC: "2.0", ID: m.ID, Result: b})
	}))
	t.Cleanup(up.Close)
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: up.Certificate().Raw}))

	team := db.SeedTeam(t)
	agent := pgtest.NewID("agt")
	db.SeedOrchestrationRuntime(t)
	exec(`UPDATE afs.teams SET gateway_deployment = 'per_namespace', may_add_mcp_servers = true WHERE id = $1`, team.ID)
	exec(`INSERT INTO afs.agents (id, team_id, name, template, image, model_id, mcp_mode)
VALUES ($1, $2, 'reviewer', 'hermes', 'ghcr.io/x/hermes@sha256:`+strings.Repeat("a", 64)+`', $3, 'full')`, agent, team.ID, pgtest.OrchestrationModel)
	srv, slug := pgtest.NewID("mcp"), "iss-"+strings.ToLower(pgtest.NewID("x")[4:10])
	exec(`INSERT INTO afs.mcp_servers (id, team_id, slug, display_name, owner, purpose, data_classification, transport, location, url, ca_bundle,
  auth_kind, review_status, risk_tier, protocol_version) VALUES ($1, $2, $3, 'Issues', 'Platform', 'Tickets', 'internal', 'streamable_http', 'corporate', $4, $5,
  'none', 'approved', 'low', '2026-07-28')`, srv, team.ID, slug, up.URL+"/mcp", ca)
	exec(`INSERT INTO afs.team_mcp_servers (team_id, server_id, enabled) VALUES ($1, $2, true)`, team.ID, srv)
	def := toolDefinition("list_issues")
	digest, _ := mcp.Digest(def)
	canon, _ := mcp.Canonicalize(def)
	exec(`INSERT INTO afs.mcp_primitives (id, server_id, kind, name, annotations, definition, digest, status)
VALUES ($1, $2, 'tool', 'list_issues', '{"readOnlyHint": true}', $3, $4, 'approved')`, pgtest.NewID("mpr"), srv, canon, digest)
	setEnabled := func(on bool) {
		exec(`UPDATE afs.service_settings SET value = to_jsonb($1::boolean) WHERE key = 'mcp.enabled'`, on)
	}
	setEnabled(true)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.WithoutCancel(ctx), `UPDATE afs.service_settings SET value = 'false' WHERE key = 'mcp.enabled'`)
	})

	pool, err := postgres.OpenPool(ctx, postgres.Config{URL: db.URLs["afs_gateway"], Service: "afs-gateway", SystemActor: true, TeamIDs: team.ID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	s := settings{TeamNamespace: "afs-t-" + team.Slug, TeamID: team.ID, CacheTTL: time.Second, CredentialsDir: t.TempDir(),
		McpSealingKeys: [][]byte{bytes.Repeat([]byte{7}, 32)}}
	identify := func(r *http.Request) (mcpgateway.Caller, error) {
		return mcpgateway.Caller{TeamID: team.ID, SubjectKind: "agent", SubjectID: agent}, nil
	}
	app, err := wireMcp(ctx, slog.New(slog.DiscardHandler), s, pool, identify, nil, "us-east-1")
	if err != nil || app == nil {
		t.Fatalf("wireMcp = %v %v", app, err)
	}
	app.Ledger.Run(ctx)
	mux := gatewayhttp.NewMux(http.NotFoundHandler())
	app.Family.Mount(mux)

	post := func(path, method, name, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(mcpwire.HeaderProtocolVersion, mcpwire.Version20260728)
		req.Header.Set(mcpwire.HeaderMethod, method)
		if name != "" {
			req.Header.Set(mcpwire.HeaderName, name)
		}
		req.Header.Set(gatewayhttp.TokenHeader, "agent-token")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		b, _ := io.ReadAll(rec.Body)
		return rec.Code, string(b)
	}
	code, body := post("/mcp/"+slug, "tools/call", "list_issues", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_issues","arguments":{}}}`)
	if code != 200 || !strings.Contains(body, "3 open issues") {
		t.Fatalf("per-server call: %d %s", code, body)
	}
	code, body = post("/mcp", "tools/call", slug+"__list_issues", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+slug+`__list_issues"}}`)
	if code != 200 || !strings.Contains(body, "3 open issues") {
		t.Fatalf("aggregated call: %d %s", code, body)
	}
	for _, a := range gotAuth {
		if a != "" {
			t.Errorf("the agent's credential reached the upstream: %q", a)
		}
	}
	_, body = post("/mcp", "tools/list", "", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if !strings.Contains(body, slug+"__list_issues") || strings.Contains(body, "delete_issue") || !strings.Contains(body, "afs_search") {
		t.Errorf("full-mode list: %s", body)
	}

	// A requested refresh holds the new delete_issue; it stays invisible.
	exec(`UPDATE afs.mcp_servers SET refresh_requested_at = now() WHERE id = $1`, srv)
	if n := app.Refresher.Once(ctx); n != 1 {
		t.Errorf("refreshed %d servers", n)
	}
	var held int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM afs.mcp_primitives WHERE server_id = $1 AND name = 'delete_issue' AND status = 'pending_review'`, srv).Scan(&held); err != nil || held != 1 {
		t.Errorf("held after refresh = %d %v", held, err)
	}
	app.InvalidateAll()
	code, body = post("/mcp/"+slug, "tools/call", "delete_issue", `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"delete_issue"}}`)
	if code != 200 || !strings.Contains(body, "-32602") {
		t.Errorf("a held tool was callable: %d %s", code, body)
	}

	app.Ledger.Close()
	var rows int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM afs.mcp_events WHERE team_id = $1 AND agent_id = $2`, team.ID, agent).Scan(&rows); err != nil || rows < 4 {
		t.Errorf("ledger rows = %d %v", rows, err)
	}
	var refused int
	_ = admin.QueryRow(ctx, `SELECT count(*) FROM afs.mcp_events WHERE team_id = $1 AND outcome = 'refused' AND reason = 'primitive_pending_review'`, team.ID).Scan(&refused)
	if refused != 1 {
		t.Errorf("refused rows = %d", refused)
	}

	setEnabled(false)
	app.InvalidateAll()
	if code, _ := post("/mcp", "tools/list", "", `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`); code != http.StatusNotFound {
		t.Errorf("mcp.enabled off: %d", code)
	}
	if a, err := wireMcp(ctx, nil, settings{}, pool, identify, nil, ""); a != nil || err != nil {
		t.Error("a shared gateway serves MCP")
	}
}

func toolDefinition(name string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"name": name, "description": "Issues: " + name + ".", "inputSchema": map[string]any{"type": "object"},
		"annotations": map[string]any{"readOnlyHint": name == "list_issues", "openWorldHint": false}})
	return b
}

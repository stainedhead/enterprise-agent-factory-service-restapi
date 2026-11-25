package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// API contract §4.14: CreateAgent and UpdateAgent take mcp {access, serverIds, mode, blocks}; the response shows
// it with the effective servers and the AFS_MCP_* names, and a server the team has off is 400
// mcp_server_not_enabled_for_team.
func TestAgentMcpEndToEnd(t *testing.T) {
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := w.Admin.Exec(t.Context(), q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	exec(`UPDATE afs.teams SET gateway_deployment = 'per_namespace' WHERE id = $1`, w.TeamA.ID)
	on, off := pgtest.NewID("mcp"), pgtest.NewID("mcp")
	for _, id := range []string{on, off} {
		exec(`INSERT INTO afs.mcp_servers (id, team_id, slug, display_name, owner, purpose, data_classification, transport, location, url, auth_kind,
  review_status, risk_tier) VALUES ($1, $2, $3, 'Issues', 'Platform', 'Tickets', 'internal', 'streamable_http', 'corporate',
  'https://issues.corp.example/mcp', 'none', 'approved', 'low')`, id, w.TeamA.ID, "e2e-"+strings.ToLower(id[4:14]))
	}
	exec(`INSERT INTO afs.team_mcp_servers (team_id, server_id, enabled) VALUES ($1, $2, true), ($1, $3, false)`, w.TeamA.ID, on, off)
	adm := w.Tokens[matrix.TeamAdmin]
	model, tpl := w.SeedAgentCatalog(t)
	base := "/v1/teams/" + w.TeamA.ID + "/agents"
	image := "registry.example.com/agents/coder@sha256:" + strings.Repeat("4f", 32)
	body := func(mcp string) string {
		return `{"name":"mcp-` + strings.ToLower(pgtest.NewID("x")[2:8]) + `","template":"` + tpl + `","image":"` + image + `","modelId":"` + model +
			`","mcp":` + mcp + `}`
	}
	if code, out, _ := agentCall(t, c.Handler, http.MethodPost, base, adm, body(`{"access":"selected","serverIds":["`+off+`"]}`)); code != 400 ||
		!strings.Contains(out, "mcp_server_not_enabled_for_team") {
		t.Fatalf("a server that is off: %d %s", code, out)
	}
	code, out, hdr := agentCall(t, c.Handler, http.MethodPost, base, adm, body(`{"access":"selected","serverIds":["`+on+`"],
  "blocks":[{"serverId":"`+on+`","name":"delete_*","effect":"block"}]}`))
	if code != 201 || !strings.Contains(out, `"mcp":{"access":"selected","serverIds":["`+on+`"],"mode":"search"`) ||
		!strings.Contains(out, `"effectiveServers":[]`) || !strings.Contains(out, `"env":["AFS_MCP_URL","AFS_MCP_CONFIG"]`) {
		t.Fatalf("create: %d %s", code, out)
	}
	id := out[strings.Index(out, `"id":"`)+6:]
	id = id[:strings.Index(id, `"`)]
	code, out, _ = agentCall(t, c.Handler, http.MethodPatch, base+"/"+id, adm, `{"mcp":{"mode":"full"}}`, "If-Match", hdr.Get("ETag"))
	if code != 200 || !strings.Contains(out, `"mode":"full"`) || !strings.Contains(out, `"AFS_MCP_SERVERS"`) || !strings.Contains(out, "delete_*") {
		t.Fatalf("update: %d %s", code, out)
	}
	var n int
	if err := w.Admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE resource_id = $1 AND after ? 'mcp'`, id).Scan(&n); err != nil || n != 2 {
		t.Errorf("audit with mcp = %d %v", n, err)
	}
}

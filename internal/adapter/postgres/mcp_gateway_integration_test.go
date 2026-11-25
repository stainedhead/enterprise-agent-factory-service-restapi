package postgres_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpgateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpinventory"
)

func toolDef(name, desc string, readOnly bool) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"name": name, "description": desc, "inputSchema": map[string]any{"type": "object"},
		"annotations": map[string]any{"readOnlyHint": readOnly, "openWorldHint": false}})
	return b
}

// The team gateway's MCP state as afs_gateway sees it (gateway contract §13.4 to §13.8): a refresh holds new
// primitives, admission lists only approved ones, a changed definition is held while the approved one is served,
// credential expiry emits one event, and the ledger is written in a batch.
func TestMcpGatewayStore(t *testing.T) {
	r := newMcpRig(t)
	ctx := t.Context()
	pool := gatewayPoolFor(t, r.db, r.team.ID)
	g := postgres.NewMcpGatewayStore(pool)

	srv, slug := pgtest.NewID("mcp"), "gw-"+strings.ToLower(pgtest.NewID("x")[4:12])
	r.exec(`INSERT INTO afs.mcp_servers (id, team_id, slug, display_name, owner, purpose, data_classification, transport, location, url, auth_kind,
  review_status, risk_tier, refresh_requested_at) VALUES ($1, $2, $3, 'Issues', 'Platform', 'Tickets', 'internal', 'streamable_http', 'corporate',
  'https://issues.corp.example/mcp', 'static', 'approved', 'low', now())`, srv, r.team.ID, slug)
	r.exec(`INSERT INTO afs.team_mcp_servers (team_id, server_id, enabled) VALUES ($1, $2, true)`, r.team.ID, srv)

	tg, err := g.Target(ctx, srv)
	if err != nil || tg.Slug != slug || tg.URL != "https://issues.corp.example/mcp" || tg.AuthKind != "static" || tg.TeamID != r.team.ID {
		t.Fatalf("target = %+v %v", tg, err)
	}
	if _, err := g.Target(ctx, pgtest.NewID("mcp")); err == nil {
		t.Error("an unknown server has a target")
	}
	due, err := g.McpRefreshTargets(ctx, r.team.ID)
	if err != nil || len(due) != 1 || !due[0].Requested || !due[0].LastSeen.Before(time.Now().Add(-time.Hour)) {
		t.Fatalf("refresh targets = %+v %v", due, err)
	}

	// First refresh: both tools are new and held.
	listed := map[mcp.Kind][]json.RawMessage{mcp.KindTool: {toolDef("list_issues", "List issues.", true), toolDef("close_issue", "Close one.", true)}}
	plan, err := mcpinventory.Build(mcpinventory.Server{ID: srv, Slug: slug, DriftPolicy: "hold"}, nil, listed, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.ApplyMcpPlan(ctx, tg, plan, "2026-07-28"); err != nil {
		t.Fatal(err)
	}
	if n := r.count(`SELECT count(*) FROM afs.mcp_primitives WHERE server_id = $1 AND status = 'pending_review'`, srv); n != 2 {
		t.Errorf("held primitives = %d", n)
	}
	if n := r.count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'mcp_server.primitive_changed' AND aggregate_id = $1`, srv); n != 1 {
		t.Errorf("primitive_changed events = %d, want one per refresh", n)
	}
	if n := r.count(`SELECT count(*) FROM afs.mcp_servers WHERE id = $1 AND refresh_requested_at IS NULL AND protocol_version = '2026-07-28'`, srv); n != 1 {
		t.Error("the refresh request was not cleared")
	}

	caller := mcpgateway.Caller{TeamID: r.team.ID, SubjectKind: "agent", SubjectID: r.agent}
	snap, err := g.Snapshot(ctx, caller)
	if err != nil || len(snap.Servers) != 1 || len(snap.Entries) != 2 || snap.Mode != "search" {
		t.Fatalf("snapshot = %+v %v", snap, err)
	}
	for _, e := range snap.Entries {
		if e.Decision.Allowed || e.Reason != "primitive_pending_review" {
			t.Errorf("a held primitive is admitted: %+v", e)
		}
	}
	v1 := snap.Version

	r.exec(`UPDATE afs.mcp_primitives SET status = 'approved' WHERE server_id = $1 AND name = 'list_issues'`, srv)
	snap, _ = g.Snapshot(ctx, caller)
	allowed := 0
	for _, e := range snap.Entries {
		if e.Decision.Allowed {
			allowed++
			if e.Primitive.Name != "list_issues" || !strings.Contains(string(e.Definition), "List issues.") || !e.Primitive.Annotations.ReadOnly() {
				t.Errorf("entry = %+v", e)
			}
		}
	}
	if allowed != 1 || snap.Version == v1 {
		t.Errorf("allowed = %d, version %s → %s", allowed, v1, snap.Version)
	}
	if _, err := g.Snapshot(ctx, mcpgateway.Caller{SubjectKind: "person"}); err == nil {
		t.Error("a caller that is no agent or pool")
	}

	// Second refresh: list_issues changed incompatibly → held, the approved definition still served.
	stored, err := g.McpStored(ctx, srv)
	if err != nil || len(stored) != 2 {
		t.Fatalf("stored = %+v %v", stored, err)
	}
	listed = map[mcp.Kind][]json.RawMessage{mcp.KindTool: {toolDef("list_issues", "List issues.", false), toolDef("close_issue", "Close one.", true)}}
	plan, _ = mcpinventory.Build(mcpinventory.Server{ID: srv, Slug: slug, DriftPolicy: "hold"}, stored, listed, false)
	if err := g.ApplyMcpPlan(ctx, tg, plan, ""); err != nil {
		t.Fatal(err)
	}
	var def, pending []byte
	var status string
	if err := r.db.Admin(t).QueryRow(ctx, `SELECT definition, pending_definition, status FROM afs.mcp_primitives WHERE server_id = $1 AND name = 'list_issues'`, srv).
		Scan(&def, &pending, &status); err != nil || status != "pending_review" || !strings.Contains(string(def), `"readOnlyHint": true`) || pending == nil {
		t.Errorf("held change = %s %s %s %v", status, def, pending, err)
	}

	// Removal on the third refresh.
	stored, _ = g.McpStored(ctx, srv)
	plan, _ = mcpinventory.Build(mcpinventory.Server{ID: srv, Slug: slug}, stored, map[mcp.Kind][]json.RawMessage{mcp.KindTool: {toolDef("close_issue", "Close one.", true)}}, false)
	if err := g.ApplyMcpPlan(ctx, tg, plan, ""); err != nil {
		t.Fatal(err)
	}
	if n := r.count(`SELECT count(*) FROM afs.mcp_primitives WHERE server_id = $1 AND removed_at IS NOT NULL`, srv); n != 1 {
		t.Errorf("removed = %d", n)
	}

	// A compatible change under auto_approve_compatible is approved by the gateway and served at once.
	r.exec(`UPDATE afs.mcp_servers SET drift_policy = 'auto_approve_compatible' WHERE id = $1`, srv)
	r.exec(`UPDATE afs.mcp_primitives SET status = 'approved' WHERE server_id = $1 AND name = 'close_issue'`, srv)
	stored, _ = g.McpStored(ctx, srv)
	plan, _ = mcpinventory.Build(mcpinventory.Server{ID: srv, Slug: slug, DriftPolicy: "auto_approve_compatible"}, stored,
		map[mcp.Kind][]json.RawMessage{mcp.KindTool: {toolDef("close_issue", "Close one issue by id.", true)}}, false)
	if len(plan.Items) == 0 || plan.Items[0].Action != mcpinventory.ActionAutoApprove {
		t.Fatalf("plan = %+v", plan.Items)
	}
	if err := g.ApplyMcpPlan(ctx, tg, plan, ""); err != nil {
		t.Fatal(err)
	}
	if n := r.count(`SELECT count(*) FROM afs.mcp_primitives WHERE server_id = $1 AND name = 'close_issue' AND status = 'approved'
  AND decided_by = 'system:auto_approve' AND definition->>'description' = 'Close one issue by id.'`, srv); n != 1 {
		var st, by, d string
		_ = r.db.Admin(t).QueryRow(ctx, `SELECT status, coalesce(decided_by, ''), definition::text FROM afs.mcp_primitives WHERE server_id = $1 AND name = 'close_issue'`, srv).Scan(&st, &by, &d)
		t.Errorf("the compatible change was not approved automatically: %s %s %s", st, by, d)
	}

	// Credential status: set → expired emits auth_expired once.
	var path string
	if err := r.db.Admin(t).QueryRow(ctx, `SELECT afs.mcp_credential_path($1, $2)`, r.team.ID, slug).Scan(&path); err != nil {
		t.Fatal(err)
	}
	r.exec(`INSERT INTO afs.mcp_credentials (id, server_id, scope, team_id, kind, secret_arn, status)
VALUES ($1, $2, $3, $3, 'static', $4, 'set')`, pgtest.NewID("mcr"), srv, r.team.ID, "arn:aws:secretsmanager:us-east-1:000000000000:secret:"+path+"-AbCdEf")
	for range 2 {
		if err := g.MarkMcpCredential(ctx, srv, r.team.ID, "expired"); err != nil {
			t.Fatal(err)
		}
	}
	if n := r.count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'mcp_server.auth_expired' AND aggregate_id = $1`, srv); n != 1 {
		t.Errorf("auth_expired events = %d", n)
	}
	if err := g.MarkMcpCredential(ctx, srv, r.team.ID, "set"); err != nil {
		t.Fatal(err)
	}
	if n := r.count(`SELECT count(*) FROM afs.outbox_events WHERE event_type = 'mcp_server.auth_restored' AND aggregate_id = $1`, srv); n != 1 {
		t.Errorf("auth_restored events = %d", n)
	}
	if err := g.ReportMcpFederation(ctx, srv, r.team.ID, "ready", ""); err != nil {
		t.Error(err)
	}

	// Settings.
	if on, err := g.McpEnabled(ctx); err != nil || on {
		t.Errorf("mcp.enabled = %v %v", on, err)
	}
	if l, max, err := g.McpLimits(ctx); err != nil || l.MaxRequestBytes != 1<<20 || l.Timeout != time.Minute || max != 25 {
		t.Errorf("limits = %+v %d %v", l, max, err)
	}
	if d, err := g.McpRefreshInterval(ctx); err != nil || d != 15*time.Minute {
		t.Errorf("interval = %v %v", d, err)
	}

	// The ledger.
	led := postgres.NewMcpLedger(pool, nil)
	led.Run(ctx)
	hit := true
	led.Record(mcpgateway.Event{At: time.Now(), RequestID: "req_l1", Caller: caller, ServerID: srv, ServerKind: "team", Method: "tools/call",
		Name: slug + "__list_issues", Digest: "sha256:" + strings.Repeat("a", 64), Outcome: "ok", HTTPStatus: 200, GatewayMS: 3, UpstreamMS: 2,
		RequestBytes: 40, ResponseBytes: 90, Protocol: "2026-07-28"})
	led.Record(mcpgateway.Event{At: time.Now(), RequestID: "req_l2", Caller: caller, ServerKind: "afs", Method: "tools/call", Name: "afs_search",
		Outcome: "ok", SearchResults: []string{slug + "__list_issues"}, SearchHit: &hit, QueryText: "issues", Digest: "not-a-digest"})
	led.Close()
	if led.Written() != 2 || led.Dropped() != 0 {
		t.Fatalf("ledger written %d dropped %d", led.Written(), led.Dropped())
	}
	if n := r.count(`SELECT count(*) FROM afs.mcp_events WHERE team_id = $1 AND query_text IS NULL AND agent_id = $2`, r.team.ID, r.agent); n != 2 {
		t.Errorf("ledger rows = %d (query text kept only during a capture)", n)
	}
}

func gatewayPoolFor(t *testing.T, db *pgtest.DB, teamID string) *pgxpool.Pool {
	t.Helper()
	p, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_gateway"], Service: "afs-gateway", SystemActor: true, TeamIDs: teamID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func (r *mcpRig) count(q string, args ...any) int {
	r.t.Helper()
	var n int
	if err := r.db.Admin(r.t).QueryRow(r.t.Context(), q, args...).Scan(&n); err != nil {
		r.t.Fatalf("%v\n%s", err, q)
	}
	return n
}

// Every service connection assumes its group role, so the schema's current_user guards apply to the LOGIN users
// that are only members of it (bootstrap/roles.sql).
func TestServicesAssumeTheirRole(t *testing.T) {
	db := pgtest.Get(t)
	for role, svc := range map[string]string{"afs_app": "afs-api", "afs_gateway": "afs-gateway", "afs_reconciler": "afs-reconciler"} {
		p, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs[role], Service: svc, SystemActor: true})
		if err != nil {
			t.Fatal(err)
		}
		var cur string
		if err := p.QueryRow(t.Context(), `SELECT current_user`).Scan(&cur); err != nil || cur != role {
			t.Errorf("%s runs as %q %v", svc, cur, err)
		}
		p.Close()
	}
}

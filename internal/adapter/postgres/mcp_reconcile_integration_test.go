package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

// MCP-RA-6 against the schema: the reconciler reads a team's switched-on servers with the credential reference,
// federation audience, held primitives and hosted spec; an agent's effective servers and mode; nothing while
// mcp.enabled is off; and writes the servers' conditions.
func TestReconcilerMcpAgainstTheSchema(t *testing.T) {
	db := pgtest.Get(t)
	s := seedFullAgent(t, db)
	admin := db.Admin(t)
	ctx := t.Context()
	tm := s.team
	mustExec(t, admin, `UPDATE afs.teams SET gateway_deployment = 'per_namespace' WHERE id = $1`, tm.ID)
	setMcp := func(on bool) {
		mustExec(t, admin, `UPDATE afs.service_settings SET value = to_jsonb($1::boolean) WHERE key = 'mcp.enabled'`, on)
	}
	setMcp(true)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.WithoutCancel(ctx), `UPDATE afs.service_settings SET value = 'false' WHERE key = 'mcp.enabled'`)
	})
	remote, hosted := pgtest.NewID("mcp"), pgtest.NewID("mcp")
	rslug, hslug := "rc-"+strings.ToLower(remote[4:12]), "rh-"+strings.ToLower(hosted[4:12])
	mustExec(t, admin, `INSERT INTO afs.mcp_servers (id, team_id, slug, display_name, owner, purpose, data_classification, transport, location, url,
  auth_kind, review_status, risk_tier) VALUES ($1, $2, $3, 'Issues', 'Platform', 'Tickets', 'internal', 'streamable_http', 'corporate',
  'https://issues.corp.example/mcp', 'static', 'approved', 'low')`, remote, tm.ID, rslug)
	mustExec(t, admin, `INSERT INTO afs.mcp_servers (id, team_id, slug, display_name, owner, purpose, data_classification, transport, location,
  auth_kind, review_status, risk_tier) VALUES ($1, $2, $3, 'Files', 'Platform', 'Files', 'internal', 'hosted', 'in_namespace', 'none', 'approved', 'low')`,
		hosted, tm.ID, hslug)
	mustExec(t, admin, `INSERT INTO afs.mcp_hosted_specs (server_id, image, args, env_names, egress_hosts, resources)
VALUES ($1, 'registry.example.com/files@sha256:`+strings.Repeat("d", 64)+`', '{/app/server}', '{TOKEN}', '{api.files.example}', '{"cpu":"250m"}')`, hosted)
	mustExec(t, admin, `INSERT INTO afs.team_mcp_servers (team_id, server_id, enabled, mode) VALUES ($1, $2, true, 'read_only'), ($1, $3, true, 'full')`,
		tm.ID, remote, hosted)
	mustExec(t, admin, `INSERT INTO afs.mcp_credentials (id, server_id, scope, team_id, kind, secret_arn, status)
VALUES ($1, $2, $3, $3, 'static', 'arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/afs-t-' || $4 || '/_gateway/mcp/' || $5 || '-AbCdEf', 'expired')`,
		pgtest.NewID("mcr"), remote, tm.ID, tm.Slug, rslug)
	for _, srv := range []string{remote, hosted} {
		mustExec(t, admin, `INSERT INTO afs.mcp_primitives (id, server_id, kind, name, annotations, definition, digest, status)
VALUES ($1, $2, 'tool', 'list', '{"readOnlyHint": true}', '{}',
  'sha256:`+strings.Repeat("e", 64)+`', 'approved')`, pgtest.NewID("mpr"), srv)
	}
	mustExec(t, admin, `INSERT INTO afs.mcp_primitives (id, server_id, kind, name, definition, digest, status) VALUES ($1, $2, 'tool', 'drop', '{}',
  'sha256:`+strings.Repeat("f", 64)+`', 'pending_review')`, pgtest.NewID("mpr"), remote)
	mustExec(t, admin, `UPDATE afs.agents SET mcp_mode = 'full' WHERE id = $1`, s.agentID)

	runner := reconcilerPool(t, db)
	d, err := postgres.NewTeamSource(runner).LoadTeam(ctx, tm.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := d.Gateway.Mcp
	if !m.Enabled || len(m.Servers) != 2 {
		t.Fatalf("mcp = %+v", m)
	}
	var r, h = m.Servers[0], m.Servers[1]
	if r.Slug != rslug || r.CredentialState != "expired" || r.CredentialARN == "" || r.DriftPending != 1 || r.Hosted != nil ||
		h.Hosted == nil || h.Hosted.Image == "" || h.Hosted.Resources["cpu"] != "250m" || h.Hosted.EnvNames[0] != "TOKEN" {
		t.Errorf("servers = %+v / %+v", r, h)
	}
	a, err := postgres.NewAgentSource(runner).LoadAgent(ctx, s.agentID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Mcp.Mode != "full" || len(a.Mcp.Servers) != 2 || a.Mcp.Servers[0].Slug != rslug || !a.Mcp.Servers[0].ReadOnly || a.Mcp.Servers[0].Kind != "team" {
		t.Errorf("agent mcp = %+v", a.Mcp)
	}

	w := postgres.NewRouteStatusWriter(runner, postgres.NewOutboxWriter(), billingIDs{}, &fakes.Clock{T: time.Now().UTC()})
	if err := w.ReportMcp(ctx, tm.ID, d.Gateway.McpConditions()); err != nil {
		t.Fatal(err)
	}
	var phase string
	if err := admin.QueryRow(ctx, `SELECT phase FROM afs.resource_status WHERE resource_kind = 'team_mcp_server' AND resource_id = $1`,
		tm.ID+"/"+remote).Scan(&phase); err != nil || phase != "Degraded" {
		t.Errorf("remote phase = %q %v", phase, err)
	}
	if err := w.ReportMcp(ctx, tm.ID, nil); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = admin.QueryRow(ctx, `SELECT count(*) FROM afs.resource_status WHERE resource_kind = 'team_mcp_server' AND team_id = $1`, tm.ID).Scan(&n)
	if n != 0 {
		t.Errorf("rows left = %d", n)
	}

	setMcp(false)
	if d, err = postgres.NewTeamSource(runner).LoadTeam(ctx, tm.ID); err != nil || d.Gateway.Mcp.Enabled {
		t.Errorf("off team = %+v %v", d.Gateway.Mcp, err)
	}
	if a, err = postgres.NewAgentSource(runner).LoadAgent(ctx, s.agentID); err != nil || len(a.Mcp.Servers) != 0 {
		t.Errorf("off agent = %+v %v", a.Mcp, err)
	}
}

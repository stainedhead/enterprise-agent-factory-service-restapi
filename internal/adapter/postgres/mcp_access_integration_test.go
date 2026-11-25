package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpaccess"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// An agent's mcp member (API contract §4.14) as afs_app stores it: selected servers must be on for the team, the
// agent's own blocks land as agent-level rules, the effective set follows afs.mcp_admission, and switching back
// to access all drops the server rows. Another team cannot write it.
func TestMcpAccessRepo(t *testing.T) {
	r := newMcpRig(t)
	ctx := t.Context()
	runner := postgres.NewTxRunner(appPool(t, r.db))
	st := postgres.NewMcpAccessRepo()
	on, off := pgtest.NewID("mcp"), pgtest.NewID("mcp")
	for _, id := range []string{on, off} {
		r.exec(`INSERT INTO afs.mcp_servers (id, team_id, slug, display_name, owner, purpose, data_classification, transport, location, url, auth_kind,
  review_status, risk_tier) VALUES ($1, $2, $3, 'Issues', 'Platform', 'Tickets', 'internal', 'streamable_http', 'corporate',
  'https://issues.corp.example/mcp', 'none', 'approved', 'low')`, id, r.team.ID, "ac-"+strings.ToLower(id[4:14]))
		r.exec(`INSERT INTO afs.mcp_primitives (id, server_id, kind, name, definition, digest, status) VALUES ($1, $2, 'tool', 'list_issues', '{}',
  'sha256:`+strings.Repeat("b", 64)+`', 'approved')`, pgtest.NewID("mpr"), id)
	}
	r.exec(`INSERT INTO afs.team_mcp_servers (team_id, server_id, enabled) VALUES ($1, $2, true), ($1, $3, false)`, r.team.ID, on, off)

	write := func(c interface{ Session() port.Session }, p domain.McpAccessPatch) error {
		return runner.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
			_, _, err := mcpaccess.Write(ctx, tx, st, billingIDs{}, r.team.ID, mcpaccess.KindAgent, r.agent, p)
			return err
		})
	}
	fill := func() *domain.McpAccessState {
		var out *domain.McpAccessState
		if err := runner.ReadTx(ctx, r.teamAdm.Session(), func(ctx context.Context, tx port.Tx) error {
			m, err := mcpaccess.Fill(ctx, tx, st, r.team.ID, mcpaccess.KindAgent, []string{r.agent})
			out = m[r.agent]
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}

	if m := fill(); m == nil || m.Access != "all" || m.Mode != domain.McpAgentModeSearch || len(m.Effective) != 1 || m.Effective[0] != on ||
		len(m.Env) != 2 {
		t.Fatalf("default = %+v", m)
	}
	sel, full := "selected", domain.McpAgentModeFull
	if err := write(r.teamAdm, domain.McpAccessPatch{Access: &sel, ServerIDs: &[]string{off}}); err == nil ||
		!strings.Contains(err.Error(), domain.ReasonMcpServerNotEnabledForTeam) {
		t.Errorf("a server that is off = %v", err)
	}
	blocks := []domain.McpAgentBlock{{ServerID: on, Name: "list_*", Effect: "block"}}
	if err := write(r.teamAdm, domain.McpAccessPatch{Access: &sel, ServerIDs: &[]string{on}, Mode: &full, Blocks: &blocks}); err != nil {
		t.Fatal(err)
	}
	m := fill()
	if m.Access != "selected" || len(m.ServerIDs) != 1 || m.Mode != full || len(m.Blocks) != 1 || m.Blocks[0].Name != "list_*" ||
		len(m.Effective) != 0 || len(m.Env) != 3 {
		t.Errorf("selected = %+v", m)
	}
	if n := r.count(`SELECT count(*) FROM afs.mcp_policies WHERE level = 'agent' AND agent_id = $1`, r.agent); n != 1 {
		t.Errorf("agent rules = %d", n)
	}
	// A repeated body does not raise the version again.
	var v1, v2 int
	_ = r.db.Admin(t).QueryRow(ctx, `SELECT version FROM afs.agents WHERE id = $1`, r.agent).Scan(&v1)
	if err := write(r.teamAdm, domain.McpAccessPatch{Access: &sel}); err != nil {
		t.Fatal(err)
	}
	_ = r.db.Admin(t).QueryRow(ctx, `SELECT version FROM afs.agents WHERE id = $1`, r.agent).Scan(&v2)
	if v1 != v2 {
		t.Errorf("version %d -> %d", v1, v2)
	}
	all, none := "all", []domain.McpAgentBlock{}
	if err := write(r.teamAdm, domain.McpAccessPatch{Access: &all, Blocks: &none}); err != nil {
		t.Fatal(err)
	}
	if m := fill(); m.Access != "all" || len(m.ServerIDs) != 0 || len(m.Blocks) != 0 || m.Mode != full || len(m.Effective) != 1 {
		t.Errorf("back to all = %+v", m)
	}
	if n := r.count(`SELECT count(*) FROM afs.agent_mcp_servers WHERE agent_id = $1`, r.agent); n != 0 {
		t.Errorf("server rows left = %d", n)
	}
	// Another team's admin sees no such agent, so nothing is written.
	if err := write(r.outs, domain.McpAccessPatch{Access: &sel, ServerIDs: &[]string{on}}); err == nil {
		t.Error("another team wrote the access")
	}
	if n := r.count(`SELECT count(*) FROM afs.agent_mcp_servers WHERE agent_id = $1`, r.agent); n != 0 {
		t.Errorf("cross-team rows = %d", n)
	}
}

// An mcp-category external system reads governedBy once an approved MCP server covers its host.
func TestExternalSystemGovernedBy(t *testing.T) {
	r := newMcpRig(t)
	ctx := t.Context()
	runner := postgres.NewTxRunner(appPool(t, r.db))
	host := "gov-" + strings.ToLower(pgtest.NewID("x")[4:12]) + ".corp.example"
	sys := pgtest.NewID("ext")
	r.exec(`INSERT INTO afs.external_systems (id, slug, name, category, endpoints) VALUES ($1, $2, 'Issues', 'mcp', ARRAY[$3])`,
		sys, "gov-"+strings.ToLower(sys[4:12]), host)
	read := func() domain.ExternalSystemAccess {
		var a domain.ExternalSystemAccess
		if err := runner.ReadTx(ctx, r.teamAdm.Session(), func(ctx context.Context, tx port.Tx) error {
			var err error
			a, err = postgres.NewExtSystemRepo().GetTeam(ctx, tx, r.team.ID, sys, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return a
	}
	if a := read(); a.GovernedBy != "" {
		t.Errorf("before = %q", a.GovernedBy)
	}
	srv := pgtest.NewID("mcp")
	r.exec(`INSERT INTO afs.mcp_servers (id, slug, display_name, owner, purpose, data_classification, transport, location, url, auth_kind,
  review_status, risk_tier) VALUES ($1, $2, 'Issues', 'Platform', 'Tickets', 'internal', 'streamable_http', 'corporate', $3, 'none', 'approved', 'low')`,
		srv, "gov-"+strings.ToLower(srv[4:12]), "https://"+host+"/mcp")
	if a := read(); a.GovernedBy != srv {
		t.Errorf("after = %q, want %s", a.GovernedBy, srv)
	}
}

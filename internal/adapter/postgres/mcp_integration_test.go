package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
)

// mcpSecrets is a SecretWriter fake that answers ARNs at the requested path (the database checks the path).
type mcpSecrets struct {
	mu      sync.Mutex
	created []string
}

func (s *mcpSecrets) Create(_ context.Context, name string, _ domain.SecretValue, _ string, _ map[string]string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created = append(s.created, name)
	return "arn:aws:secretsmanager:us-east-1:000000000000:secret:" + name + "-AbCdEf", nil
}
func (*mcpSecrets) PutValue(context.Context, string, domain.SecretValue, string) error { return nil }
func (*mcpSecrets) ForceDelete(context.Context, string) error                          { return nil }
func (*mcpSecrets) ScheduleDeletion(context.Context, string, int) error                { return nil }

type mcpRig struct {
	t                         *testing.T
	db                        *pgtest.DB
	svc                       *mcpcatalog.Service
	team, other               pgtest.Team
	agent                     string
	admin, sec, teamAdm, outs acting.Caller
}

func newMcpRig(t *testing.T) *mcpRig {
	t.Helper()
	db := pgtest.Get(t)
	runner := postgres.NewTxRunner(appPool(t, db))
	r := &mcpRig{t: t, db: db, team: db.SeedTeam(t), other: db.SeedTeam(t), agent: pgtest.NewID("agt")}
	r.svc = &mcpcatalog.Service{Deps: acting.Deps{Tx: runner, Audit: postgres.NewAuditWriter(runner), Outbox: postgres.NewOutboxWriter(),
		Idem: postgres.NewIdempotencyStore(), Clock: realClock{}, IDs: billingIDs{}}, Repo: postgres.NewMcpRepo(), Secrets: &mcpSecrets{},
		ClusterIssuer: "https://oidc.eks.example/id/test"}
	r.exec(`UPDATE afs.teams SET gateway_deployment = 'per_namespace', may_add_mcp_servers = true WHERE id = $1`, r.team.ID)
	r.exec(`UPDATE afs.service_settings SET value = '["api://tools"]' WHERE key = 'federation.allowed_audiences'`)
	admin := db.Admin(t)
	t.Cleanup(func() { // the database is shared: put back the defaults of the settings this test changes
		_, _ = admin.Exec(context.WithoutCancel(t.Context()), `UPDATE afs.service_settings SET value = CASE key
  WHEN 'federation.allowed_audiences' THEN '[]'::jsonb ELSE '"team_all"'::jsonb END
  WHERE key IN ('federation.allowed_audiences', 'mcp.team_server_approval')`)
	})
	db.SeedOrchestrationRuntime(t)
	r.exec(`INSERT INTO afs.agents (id, team_id, name, template, image, model_id)
VALUES ($1, $2, 'reviewer', 'hermes', 'ghcr.io/x/hermes@sha256:`+strings.Repeat("a", 64)+`', $3)`, r.agent, r.team.ID, pgtest.OrchestrationModel)
	user := func(id string) authz.Principal { return authz.Principal{ID: id, Type: authz.PrincipalUser} }
	r.admin = acting.Caller{Principal: authz.Principal{ID: user("usr_mcp_admin").ID, Type: authz.PrincipalUser, ServiceAdmin: true}, RequestID: "req_mcp1"}
	r.sec = acting.Caller{Principal: authz.Principal{ID: "usr_mcp_sec", Type: authz.PrincipalUser, SecurityOperator: true}, RequestID: "req_mcp2"}
	ta := user("usr_mcp_team")
	ta.Bindings = []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: r.team.ID}}
	r.teamAdm = acting.Caller{Principal: ta, RequestID: "req_mcp3"}
	oa := user("usr_mcp_other")
	oa.Bindings = []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: r.other.ID}}
	r.outs = acting.Caller{Principal: oa, RequestID: "req_mcp4"}
	return r
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

func (r *mcpRig) exec(q string, args ...any) {
	r.t.Helper()
	if _, err := r.db.Admin(r.t).Exec(r.t.Context(), q, args...); err != nil {
		r.t.Fatalf("%v\n%s", err, q)
	}
}

func mcpRemote(slug string) mcpcatalog.ServerInput {
	s := func(v string) *string { return &v }
	return mcpcatalog.ServerInput{Slug: s(slug), DisplayName: s("Issues"), Owner: s("Platform"), Purpose: s("Track issues"),
		DataClassification: s("internal"), Transport: s("streamable_http"), Location: s("corporate"),
		URL: s("https://issues.corp.example/mcp"), AuthKind: s("static")}
}

func wantReason(t *testing.T, err error, kind domain.ErrorKind, reason string) {
	t.Helper()
	var de *domain.Error
	ok := errors.As(err, &de)
	if domain.KindOf(err) != kind || (reason != "" && (!ok || de.Reason != reason)) {
		t.Fatalf("err = %v, want %s %s", err, kind, reason)
	}
}

func TestMcpCatalogEndToEnd(t *testing.T) {
	r := newMcpRig(t)
	ctx := t.Context()
	slug := "iss" + strings.ToLower(pgtest.NewID("x")[4:10])

	// Catalog: create, read back every column, list, refuse a taken slug.
	created, err := r.svc.Create(ctx, r.admin, mcpRemote(slug), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := created.View.Server.ID
	got, err := r.svc.Get(ctx, r.admin, id)
	if err != nil || got.Server.Slug != slug || got.Server.ReviewStatus != domain.McpDraft || got.Server.Version != 1 ||
		got.Server.CredentialMode != domain.McpCredentialPerTeam || got.Server.URL != "https://issues.corp.example/mcp" {
		t.Fatalf("get = %+v %v", got.Server, err)
	}
	list, _, err := r.svc.List(ctx, r.sec, mcpcatalog.ServerFilter{Scope: "enterprise"}, acting.Page{})
	if err != nil || len(list) == 0 {
		t.Fatalf("list = %v %v", list, err)
	}
	_, err = r.svc.Create(ctx, r.admin, mcpRemote(slug), nil)
	wantReason(t, err, domain.KindConflict, domain.ReasonSlugTaken)

	// Review: a security operator approves the draft (through pending_review); a service admin publishes.
	if _, _, err := r.svc.Publish(ctx, r.admin, id, nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("publish a draft = %v", err)
	}
	if _, _, err := r.svc.Review(ctx, r.sec, id, domain.McpApproved, mcpcatalog.ReviewInput{Note: "reviewed"}, nil); err != nil {
		t.Fatal(err)
	}
	pub, _, err := r.svc.Publish(ctx, r.admin, id, nil)
	if err != nil || !pub.Server.Published {
		t.Fatalf("publish = %+v %v", pub.Server, err)
	}

	// Inventory: the gateway records a tool pending review; the security operator approves it.
	prim := pgtest.NewID("mpr")
	r.exec(`INSERT INTO afs.mcp_primitives (id, server_id, kind, name, annotations, definition, digest, status)
VALUES ($1, $2, 'tool', 'search', '{"readOnlyHint": true}', '{"name": "search"}', 'sha256:`+strings.Repeat("b", 64)+`', 'pending_review')`, prim, id)
	prims, _, err := r.svc.ListPrimitives(ctx, r.sec, id, mcpcatalog.PrimitiveFilter{}, acting.Page{})
	if err != nil || len(prims) != 1 || prims[0].Primitive.Status != domain.McpPrimitivePending {
		t.Fatalf("primitives = %+v %v", prims, err)
	}
	dec, _, err := r.svc.DecidePrimitive(ctx, r.sec, id, prim, "fine", true, nil)
	if err != nil || dec.Primitive.Status != domain.McpPrimitiveApproved || dec.Primitive.ApprovedDigest == "" {
		t.Fatalf("decide = %+v %v", dec.Primitive, err)
	}
	pol, err := r.svc.PutPolicy(ctx, r.admin, id, mcpcatalog.PolicyInput{Rules: []domain.McpPolicyRule{{Name: "delete_*", Effect: "block"}}}, nil)
	if err != nil || len(pol.Rules) != 1 {
		t.Fatalf("policy = %+v %v", pol, err)
	}

	// The team: sees it off, switches it on, sets its credential, and its agent is admitted to the tool.
	rows, _, err := r.svc.ListTeam(ctx, r.teamAdm, r.team.ID, "", acting.Page{})
	if err != nil || len(rows) != 1 || rows[0].Row.Switch != nil {
		t.Fatalf("team list = %+v %v", rows, err)
	}
	sw, err := r.svc.PutSwitch(ctx, r.teamAdm, r.team.ID, id, mcpcatalog.SwitchInput{Enabled: true, Mode: domain.McpTeamModeFull}, nil)
	if err != nil || sw.Row.Switch == nil || !sw.Row.Switch.Enabled {
		t.Fatalf("switch = %+v %v", sw, err)
	}
	cred, err := r.svc.PutTeamCredential(ctx, r.teamAdm, r.team.ID, id, mcpcatalog.CredentialInput{Value: "token"}, nil)
	if err != nil || cred.State != domain.McpCredSet {
		t.Fatalf("credential = %+v %v", cred, err)
	}
	eff, err := r.svc.GetEffective(ctx, r.teamAdm, r.team.ID, "agent", r.agent)
	if err != nil || len(eff.Servers) != 1 {
		t.Fatalf("effective = %+v %v", eff, err)
	}
	if _, err := r.svc.Get(ctx, r.outs, id); domain.KindOf(err) != domain.KindAccessDenied {
		t.Errorf("outsider reads the service catalog = %v", err)
	}
	if err := r.svc.Delete(ctx, r.admin, id, pub.Server.Version); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("delete in use = %v", err)
	}

	// A team server: created, submitted, approved by the team under team_all; invisible to another team.
	ts, err := r.svc.CreateTeam(ctx, r.teamAdm, r.team.ID, mcpRemote("own"+slug[3:]), nil)
	if err != nil {
		t.Fatal(err)
	}
	tid := ts.View.Server.ID
	if _, _, err := r.svc.Submit(ctx, r.teamAdm, r.team.ID, tid, nil); err != nil {
		t.Fatal(err)
	}
	if v, _, err := r.svc.TeamReview(ctx, r.teamAdm, r.team.ID, tid, true, mcpcatalog.ReviewInput{Note: "ours"}, nil); err != nil ||
		v.Server.ReviewStatus != domain.McpApproved {
		t.Fatalf("team review = %v", err)
	}
	if _, err := r.svc.GetTeam(ctx, r.outs, r.other.ID, tid); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("other team sees the team server = %v", err)
	}
	if _, err := r.svc.CreateTeam(ctx, r.outs, r.other.ID, mcpRemote("theirs"), nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Errorf("team without may_add_mcp_servers = %v", err)
	}

	// Federated identity for the agent: defaults from the name; a reserved audience is refused by the database.
	fed, err := r.svc.PutFederated(ctx, r.teamAdm, r.team.ID, "agent", r.agent, domain.FederatedIdentity{Name: "tools", Audience: "api://tools"}, nil)
	if err != nil || fed.Identity.EnvVar != "AFS_FEDERATED_TOKEN_FILE_TOOLS" || !strings.HasPrefix(fed.Subject, "system:serviceaccount:") {
		t.Fatalf("federated = %+v %v", fed, err)
	}
	if _, err := r.svc.PutFederated(ctx, r.teamAdm, r.team.ID, "agent", r.agent, domain.FederatedIdentity{Name: "aws", Audience: "sts.amazonaws.com"}, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("reserved audience = %v", err)
	}
	if err := r.svc.DeleteFederated(ctx, r.teamAdm, r.team.ID, "agent", r.agent, "tools", fed.Identity.Version); err != nil {
		t.Errorf("delete federated = %v", err)
	}

	// Query capture: one window at a time.
	if _, err := r.svc.StartCapture(ctx, r.admin, 1, "incident 42", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.StartCapture(ctx, r.admin, 1, "again", nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("second capture = %v", err)
	}
	if c, err := r.svc.StopCapture(ctx, r.admin); err != nil || c.StoppedAt == nil {
		t.Errorf("stop = %+v %v", c, err)
	}

	// Ledger: the gateway's rows, read by the team and by the security operator; the usage report.
	for i, outcome := range []string{"ok", "ok", "refused"} {
		r.exec(`INSERT INTO afs.mcp_events (at, request_id, team_id, agent_id, server_id, server_kind, method, name, outcome, gateway_ms)
VALUES (now() - make_interval(secs => $1), 'req_'||$1, $2, $3, $4, 'enterprise', 'tools/call', 'search', $5, 12)`, i, r.team.ID, r.agent, id, outcome)
	}
	evs, next, err := r.svc.ListTeamEvents(ctx, r.teamAdm, r.team.ID, domain.McpEventFilter{}, acting.Page{Limit: 2})
	if err != nil || len(evs) != 2 || next == "" {
		t.Fatalf("team events = %d %q %v", len(evs), next, err)
	}
	rest, _, err := r.svc.ListTeamEvents(ctx, r.teamAdm, r.team.ID, domain.McpEventFilter{}, acting.Page{Limit: 2, Cursor: mcpcatalog.EventCursor(evs[1])})
	if err != nil || len(rest) != 1 || rest[0].Outcome != "refused" {
		t.Fatalf("second page = %+v %v", rest, err)
	}
	all, _, err := r.svc.ListEvents(ctx, r.sec, domain.McpEventFilter{TeamID: r.team.ID, Outcome: "refused"}, acting.Page{})
	if err != nil || len(all) != 1 {
		t.Fatalf("service events = %d %v", len(all), err)
	}
	usage, _, err := r.svc.TeamUsage(ctx, r.teamAdm, r.team.ID, domain.McpUsageQuery{})
	if err != nil || len(usage) != 1 || usage[0].Calls != 3 || usage[0].Refusals != 1 || usage[0].Label != slug {
		t.Fatalf("usage = %+v %v", usage, err)
	}
	adopt, _, err := r.svc.Usage(ctx, r.admin, domain.McpUsageQuery{View: "adoption", GroupBy: "team"})
	if err != nil || len(adopt) == 0 || adopt[0].Teams == nil {
		t.Fatalf("adoption = %+v %v", adopt, err)
	}
	unused, _, err := r.svc.TeamUsage(ctx, r.teamAdm, r.team.ID, domain.McpUsageQuery{View: "unused", WindowDays: 30})
	if err != nil || len(unused) != 0 {
		t.Errorf("unused = %+v %v", unused, err)
	}

	// Settings reach the catalog: the approval policy names security for elevated team servers.
	r.exec(`UPDATE afs.service_settings SET value = '"security_all"' WHERE key = 'mcp.team_server_approval'`)
	ts2, err := r.svc.CreateTeam(ctx, r.teamAdm, r.team.ID, mcpRemote("two"+slug[3:]), nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _, err := r.svc.Submit(ctx, r.teamAdm, r.team.ID, ts2.View.Server.ID, nil); err != nil || v.Approver != "security" {
		t.Errorf("approver = %q %v", v.Approver, err)
	}
	if _, _, err := r.svc.TeamReview(ctx, r.teamAdm, r.team.ID, ts2.View.Server.ID, true, mcpcatalog.ReviewInput{Note: "x"}, nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Errorf("team approves under security_all = %v", err)
	}
	if v, _, err := r.svc.TeamReview(ctx, r.sec, r.team.ID, ts2.View.Server.ID, true, mcpcatalog.ReviewInput{Note: "x"}, nil); err == nil {
		t.Errorf("security operator on a team route = %+v", v.Server.ReviewStatus)
	}
	if v, _, err := r.svc.Review(ctx, r.sec, ts2.View.Server.ID, domain.McpApproved, mcpcatalog.ReviewInput{Note: "security ok"}, nil); err != nil ||
		v.Server.ReviewStatus != domain.McpApproved {
		t.Errorf("security approves the team server = %v", err)
	}
	var queue json.RawMessage
	if err := r.db.Admin(t).QueryRow(ctx, `SELECT coalesce(json_agg(event_type), '[]') FROM afs.outbox_events WHERE aggregate_id IN ($1, $2)`, id, prim).Scan(&queue); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"mcp_server.created", "mcp_server.approved", "mcp_server.published", "mcp_server.primitive_approved"} {
		if !strings.Contains(string(queue), ev) {
			t.Errorf("outbox for %s lacks %s: %s", id, ev, queue)
		}
	}
}

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/matrix"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
)

// secopsRig is the real wiring on a real database with the matrix world's credentials.
type secopsRig struct {
	w *matrix.World
	h http.Handler
}

func newSecopsRig(t *testing.T) secopsRig {
	t.Helper()
	db := pgtest.Get(t)
	w := matrix.NewWorld(t, db)
	c, err := wire(t.Context(), quiet, withCluster(settings{DatabaseURL: db.URLs["afs_app"], OwnerURL: db.URLs["afs_owner"], APIReplicas: 1,
		OIDCHTTP: w.OIDCClient(), RatePerSecond: 1e6, RateBurst: 1e6, AWSRegion: "us-east-1", ClusterOIDCIssuer: w.Cluster.Server.URL}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return secopsRig{w, c.Handler}
}

func (g secopsRig) do(t *testing.T, method, path, token, body string, hdr ...string) result {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r.RemoteAddr = "198.51.100.99:4000"
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, r)
	b, _ := io.ReadAll(rec.Body)
	return result{rec.Code, string(b), rec.Header()}
}

func (g secopsRig) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := g.w.Admin.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// RA-US-9.3, RA-US-15.5 (part), RA-US-11.1 (part): a service admin creates a service-level key, the
// secret is shown once and never stored, the key is a security operator and nothing else, and deleting it
// ends its access.
func TestServiceAPIKeyEndToEnd(t *testing.T) {
	g := newSecopsRig(t)
	svc := g.w.Tokens[matrix.ServiceAdmin]
	exp := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	body := `{"name":"scanner-` + g.w.TeamA.Slug + `","role":"security_operator","expiresAt":"` + exp + `"}`

	r := g.do(t, "POST", "/v1/admin/api-keys", svc, body, "Idempotency-Key", "svc-key-1")
	if r.code != 201 {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	var created struct{ ID, Name, Role, KeyPrefix, SecretKey string }
	if err := json.Unmarshal([]byte(r.body), &created); err != nil || !strings.HasPrefix(created.SecretKey, "afsk_") || created.Role != "security_operator" {
		t.Fatalf("created: %+v %v", created, err)
	}
	// Replay: metadata without the secret, flagged as a replay; nothing more is stored.
	rows := g.count(t, `SELECT count(*) FROM afs.api_keys WHERE team_id IS NULL`)
	r = g.do(t, "POST", "/v1/admin/api-keys", svc, body, "Idempotency-Key", "svc-key-1")
	if r.code != 201 || r.hdr.Get("Idempotent-Replayed") != "true" || strings.Contains(r.body, "secretKey") || !strings.Contains(r.body, created.ID) {
		t.Fatalf("replay: %d %q %s", r.code, r.hdr.Get("Idempotent-Replayed"), r.body)
	}
	if g.count(t, `SELECT count(*) FROM afs.api_keys WHERE team_id IS NULL`) != rows {
		t.Fatal("a replay stored another key")
	}
	if n := g.count(t, `SELECT count(*) FROM afs.idempotency_keys WHERE response_body::text LIKE '%secretKey%'`); n != 0 {
		t.Fatalf("%d stored responses contain secretKey", n)
	}
	if n := g.count(t, `SELECT count(*) FROM afs.audit_log WHERE action = 'CreateServiceApiKey' AND (after::text LIKE '%' || $1 || '%')`, created.SecretKey); n != 0 {
		t.Fatal("the secret is in an audit record")
	}
	// A duplicate name and a team role are refused.
	if r = g.do(t, "POST", "/v1/admin/api-keys", svc, body); r.code != 409 {
		t.Fatalf("duplicate name: %d %s", r.code, r.body)
	}
	if r = g.do(t, "POST", "/v1/admin/api-keys", svc, strings.Replace(strings.Replace(body, "security_operator", "team_admin", 1), "scanner-", "other-", 1)); r.code != 400 {
		t.Fatalf("team role: %d %s", r.code, r.body)
	}
	// The list shows the key without its secret.
	if r = g.do(t, "GET", "/v1/admin/api-keys", svc, ""); r.code != 200 || !strings.Contains(r.body, created.ID) || strings.Contains(r.body, created.SecretKey) {
		t.Fatalf("list: %d %s", r.code, r.body)
	}

	key := created.SecretKey
	var me meBody
	if r = g.do(t, "GET", "/v1/me", key, ""); r.code != 200 || json.Unmarshal([]byte(r.body), &me) != nil {
		t.Fatalf("GetMe: %d %s", r.code, r.body)
	}
	if !me.SecurityOperator || me.ServiceAdmin || me.Principal.Type != "api_key" || len(me.Teams) != 0 ||
		!slices.Equal(me.ServicePermissions, []string{"teams:read-directory", "work-orders:read", "work-orders:write", "swarm-templates:read"}) {
		t.Fatalf("GetMe for a service key: %+v", me)
	}
	if r = g.do(t, "GET", "/v1/team-directory", key, ""); r.code != 200 || !strings.Contains(r.body, g.w.TeamA.Slug) || !strings.Contains(r.body, g.w.TeamB.ID) {
		t.Fatalf("directory as a service key: %d %s", r.code, r.body)
	}
	// Nothing it should not reach: the key list, a team route, a security operator binding.
	for _, p := range []string{"/v1/admin/api-keys", "/v1/admin/security-operators", "/v1/teams/" + g.w.TeamA.ID, "/v1/teams/" + g.w.TeamA.ID + "/api-keys"} {
		if r = g.do(t, "GET", p, key, ""); r.code != 403 {
			t.Errorf("service key GET %s: %d, want 403", p, r.code)
		}
	}

	if r = g.do(t, "DELETE", "/v1/admin/api-keys/"+created.ID, svc, ""); r.code != 204 {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if r = g.do(t, "DELETE", "/v1/admin/api-keys/"+created.ID, svc, ""); r.code != 404 {
		t.Fatalf("delete again: %d", r.code)
	}
	if r = g.do(t, "GET", "/v1/me", key, ""); r.code != 401 {
		t.Fatalf("a deleted key authenticates: %d", r.code)
	}
}

// RA-US-11.10: only service admins grant the security operator role.
func TestOnlyServiceAdminsManageSecurityOperators(t *testing.T) {
	g := newSecopsRig(t)
	body := `{"subjectType":"group","groupName":"secops-` + g.w.TeamA.Slug + `"}`
	before := g.count(t, `SELECT count(*) FROM afs.role_bindings WHERE role = 'security_operator'`)
	for _, c := range []matrix.Caller{matrix.TeamAdmin, matrix.TeamKeyAdmin, matrix.Viewer, matrix.SecurityOperator, matrix.ServiceKey, matrix.Unbound} {
		if r := g.do(t, "POST", "/v1/admin/security-operators", g.w.Tokens[c], body); r.code != 403 {
			t.Errorf("%s adding a security operator: %d %s", c, r.code, r.body)
		}
	}
	// A team-admin cannot do it through the team's member route either.
	team := "/v1/teams/" + g.w.TeamA.ID + "/members"
	if r := g.do(t, "POST", team, g.w.Tokens[matrix.TeamAdmin], `{"role":"security_operator","subjectType":"group","groupName":"x"}`); r.code != 400 && r.code != 403 {
		t.Errorf("member route with the security_operator role: %d %s", r.code, r.body)
	}
	if g.count(t, `SELECT count(*) FROM afs.role_bindings WHERE role = 'security_operator'`) != before {
		t.Fatal("a refused call wrote a binding")
	}

	svc := g.w.Tokens[matrix.ServiceAdmin]
	r := g.do(t, "POST", "/v1/admin/security-operators", svc, body)
	var op struct {
		ID, SubjectType, GroupName string
		Version                    int64
	}
	if r.code != 201 || json.Unmarshal([]byte(r.body), &op) != nil || op.GroupName == "" || r.hdr.Get("ETag") == "" {
		t.Fatalf("add: %d %s", r.code, r.body)
	}
	etag := r.hdr.Get("ETag")
	if r = g.do(t, "POST", "/v1/admin/security-operators", svc, body); r.code != 409 {
		t.Fatalf("duplicate: %d %s", r.code, r.body)
	}
	if r = g.do(t, "GET", "/v1/admin/security-operators", svc, ""); r.code != 200 || !strings.Contains(r.body, op.ID) {
		t.Fatalf("list: %d %s", r.code, r.body)
	}
	if r = g.do(t, "DELETE", "/v1/admin/security-operators/"+op.ID, svc, ""); r.code != 428 {
		t.Fatalf("delete without If-Match: %d", r.code)
	}
	if r = g.do(t, "DELETE", "/v1/admin/security-operators/"+op.ID, svc, "", "If-Match", `"99"`); r.code != 412 {
		t.Fatalf("stale If-Match: %d", r.code)
	}
	if r = g.do(t, "DELETE", "/v1/admin/security-operators/"+op.ID, svc, "", "If-Match", etag); r.code != 204 {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if r = g.do(t, "DELETE", "/v1/admin/security-operators/"+op.ID, svc, "", "If-Match", etag); r.code != 404 {
		t.Fatalf("delete again: %d", r.code)
	}
	// The team-scoped role bindings are not security operators: the id of a member is unknown here.
	member := pgtest.NewID("rb")
	if _, err := g.w.Admin.Exec(t.Context(), `INSERT INTO afs.role_bindings (id, role, team_id, subject_type, group_name) VALUES ($1, 'team_viewer', $2, 'group', $3)`,
		member, g.w.TeamA.ID, "m-"+member); err != nil {
		t.Fatal(err)
	}
	if r = g.do(t, "DELETE", "/v1/admin/security-operators/"+member, svc, "", "If-Match", `"1"`); r.code != 404 {
		t.Fatalf("removing a team member as a security operator: %d", r.code)
	}
}

// secopsOps are the operations a security operator (person or service key) may call: the work order
// operations, swarm template list and get, the team directory and GetMe (API contract §2). Every other
// route, every team-scoped one included, is 403.
var secopsOps = map[string]bool{
	"ListWorkOrders": true, "CreateWorkOrder": true, "GetWorkOrder": true, "UpdateWorkOrder": true, "CancelWorkOrder": true,
	"ListWorkOrderRepos": true, "UpdateWorkOrderRepo": true,
	"ListSwarmTemplates": true, "GetSwarmTemplate": true, "ListTeamDirectory": true, "GetMe": true,
}

// RA-US-1.4, RA-US-15.5, RA-US-3a.2 (part): a security operator and a service key get 403 on every
// team-scoped route, for an existing team and for one that does not exist alike, and on everything
// outside secopsOps.
func TestSecurityOperatorsReachOnlyTheirOperations(t *testing.T) {
	g := newSecopsRig(t)
	const unknownTeam = "team_0ZZZZZZZZZZZZZZZZZZZZZZZZZ"
	const unknownID = "zzzzzzzz"
	fill := func(path, team string) string {
		path = strings.ReplaceAll(path, "{teamId}", team)
		for {
			i := strings.Index(path, "{")
			if i < 0 {
				return path
			}
			j := strings.Index(path[i:], "}")
			path = path[:i] + unknownID + path[i+j+1:]
		}
	}
	for _, caller := range []matrix.Caller{matrix.SecurityOperator, matrix.ServiceKey} {
		token := g.w.Tokens[caller]
		checked := 0
		for _, rt := range httpapi.Routes {
			if rt.Scope == authz.ScopePublic {
				continue
			}
			teams := []string{""}
			if rt.Scope == authz.ScopeTeam {
				teams = []string{g.w.TeamA.ID, g.w.TeamB.ID, unknownTeam}
			}
			for _, team := range teams {
				r := g.do(t, rt.Method, fill(rt.Path, team), token, "", "If-Match", `"1"`, "Idempotency-Key", "secops-probe")
				checked++
				switch {
				case secopsOps[rt.OperationID]:
					if r.code == 401 || r.code == 403 {
						t.Errorf("%s: %s %s: %d, want it reached", caller, rt.Method, rt.Path, r.code)
					}
				case r.code != 403:
					t.Errorf("%s: %s %s (team %q): %d, want 403", caller, rt.Method, rt.Path, team, r.code)
				}
			}
		}
		if checked < 150 {
			t.Fatalf("only %d probes ran", checked)
		}
	}

	// GetMe for an operator: no team, the security operator column of the catalog.
	for _, caller := range []matrix.Caller{matrix.SecurityOperator, matrix.ServiceKey} {
		var me meBody
		if r := g.do(t, "GET", "/v1/me", g.w.Tokens[caller], ""); r.code != 200 || json.Unmarshal([]byte(r.body), &me) != nil {
			t.Fatalf("GetMe as %s: %d %s", caller, r.code, r.body)
		}
		if !me.SecurityOperator || me.ServiceAdmin || len(me.Teams) != 0 || !contains(me.ServicePermissions, "work-orders:write") || contains(me.ServicePermissions, "admin:settings") {
			t.Errorf("GetMe as %s: %+v", caller, me)
		}
	}
	// Every refusal of a security operator is audited with its actor type (RA-US-1.7).
	if n := g.count(t, `SELECT count(*) FROM afs.audit_log WHERE outcome = 'denied' AND actor_type = 'api_key' AND team_id IS NOT NULL`); n == 0 {
		t.Error("denied requests of a service key left no audit record")
	}
}

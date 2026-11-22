package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys/apikeystest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/members"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/members/memberstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams/teamstest"
)

var w2Now = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

type w2Rig struct {
	h      http.Handler
	auth   fakeAuth
	teams  *teamstest.Repo
	audit  *fakes.Audit
	outbox *fakes.Outbox
	denied []Denial
}

type w2Lookup struct{ svc *teams.Service }

func (l w2Lookup) caller(ctx context.Context) acting.Caller {
	p, _ := PrincipalFrom(ctx)
	return acting.Caller{Principal: p}
}

func (l w2Lookup) TeamFlags(ctx context.Context, id string) (authz.TeamFlags, bool, error) {
	f, _, ok, err := l.svc.Flags(ctx, l.caller(ctx), id)
	return f, ok, err
}

func (l w2Lookup) TeamState(ctx context.Context, id string) (domain.TeamState, bool, error) {
	_, st, ok, err := l.svc.Flags(ctx, l.caller(ctx), id)
	return st, ok, err
}

func newW2Rig(t *testing.T) *w2Rig {
	t.Helper()
	r := &w2Rig{auth: fakeAuth{
		"svc":     {ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true},
		"secop":   {ID: "usr_o", Type: authz.PrincipalUser, SecurityOperator: true},
		"unbound": {ID: "usr_u", Type: authz.PrincipalUser},
	}, teams: teamstest.New(), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}}
	tx := &fakes.TxRunner{}
	deps := acting.Deps{Tx: tx, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{}, Clock: &fakes.Clock{T: w2Now}, IDs: &fakes.IDGen{}}
	r.teams.Users["boss@example.com"] = "usr_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamSvc := &teams.Service{Deps: deps, Repo: r.teams, Settings: &fakes.Settings{}}
	pages := NewPageTokens(bytes.Repeat([]byte{7}, 32), func() time.Time { return w2Now })
	look := w2Lookup{teamSvc}
	reg := func(g *Registry) error {
		for _, f := range []Registrar{
			(&TeamsAPI{Svc: teamSvc, Pages: pages, Now: func() time.Time { return w2Now }}).Register,
			(&MembersAPI{Svc: &members.Service{Deps: deps, Repo: memberstest.New()}, Pages: pages}).Register,
			(&APIKeysAPI{Svc: &apikeys.Service{Deps: deps, Repo: apikeystest.New(), Entropy: bytes.NewReader(bytes.Repeat([]byte{9}, 4096))}, Pages: pages}).Register,
			(&Admin1API{Pages: pages}).registerSettings,
			(&Admin1API{Pages: pages}).registerIdPs,
			(&Admin1API{Pages: pages}).registerTags,
			(&AuditAPI{Pages: pages}).Register,
			(&RevisionsAPI{Pages: pages}).Register,
		} {
			if err := f(g); err != nil {
				return err
			}
		}
		return nil
	}
	h, err := New(Config{AllowUnbound: true, Auth: r.auth, Teams: look, TeamState: look, Registrars: []Registrar{reg},
		Hooks: Hooks{Denied: func(_ context.Context, d Denial) { r.denied = append(r.denied, d) }},
		Now:   func() time.Time { return w2Now }})
	if err != nil {
		t.Fatal(err)
	}
	r.h = h
	return r
}

func (r *w2Rig) do(t *testing.T, method, path, token, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, r.h, method, path, token, body, hdr...)
}

const w2CreateBody = `{"slug":"payments","displayName":"Payments","kind":"persistent","costCenter":"CC1","ownerEmail":"o@example.com",
"maxBudgetUsd":"1000","firstAdmin":{"subjectType":"user","email":"boss@example.com"},"tags":[]}`

// createTeam creates a team as the service admin and registers a team-admin and a viewer token for it.
func (r *w2Rig) createTeam(t *testing.T) string {
	t.Helper()
	rec := r.do(t, http.MethodPost, "/v1/teams", "svc", w2CreateBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var b struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	r.auth["admin"] = &authz.Principal{ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: b.ID}}}
	r.auth["viewer"] = &authz.Principal{ID: "usr_v", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: b.ID}}}
	return b.ID
}

func w2Code(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d: %s", rec.Code, want, rec.Body)
	}
}

func TestW2TeamLifecycle(t *testing.T) {
	r := newW2Rig(t)
	id := r.createTeam(t)
	if len(r.audit.Entries) == 0 || len(r.outbox.Events) == 0 {
		t.Error("create wrote no audit or outbox record")
	}

	// reads by every role that may read, and the cross-team 404
	for _, tok := range []string{"admin", "viewer", "svc"} {
		w2Code(t, r.do(t, "GET", "/v1/teams/"+id, tok, ""), 200)
		w2Code(t, r.do(t, "GET", "/v1/teams/"+id+"/status", tok, ""), 200)
	}
	w2Code(t, r.do(t, "GET", "/v1/teams/"+id, "unbound", ""), 404)
	w2Code(t, r.do(t, "GET", "/v1/teams", "unbound", ""), 403)

	// create: team roles are denied, with a denied audit hook call
	before := len(r.denied)
	w2Code(t, r.do(t, "POST", "/v1/teams", "admin", w2CreateBody), 403)
	w2Code(t, r.do(t, "POST", "/v1/teams", "viewer", w2CreateBody), 403)
	w2Code(t, r.do(t, "POST", "/v1/teams", "secop", w2CreateBody), 403)
	if len(r.denied)-before != 3 {
		t.Errorf("denied hook calls = %d", len(r.denied)-before)
	}

	// If-Match: required, stale is 412, current works
	rec := r.do(t, "GET", "/v1/teams/"+id, "admin", "")
	etag := rec.Header().Get("ETag")
	w2Code(t, r.do(t, "PATCH", "/v1/teams/"+id, "admin", `{"displayName":"X"}`), 428)
	w2Code(t, r.do(t, "PATCH", "/v1/teams/"+id, "admin", `{"displayName":"X"}`, "If-Match", `"999"`), 412)
	w2Code(t, r.do(t, "PATCH", "/v1/teams/"+id, "viewer", `{"displayName":"X"}`, "If-Match", etag), 403)
	rec = r.do(t, "PATCH", "/v1/teams/"+id, "admin", `{"displayName":"X"}`, "If-Match", etag)
	w2Code(t, rec, 200)
	if rec.Header().Get("ETag") == etag {
		t.Error("ETag did not change")
	}
	// guardrail field by a team admin is refused by the use case
	w2Code(t, r.do(t, "PATCH", "/v1/teams/"+id, "admin", `{"isolationTier":"dedicated"}`, "If-Match", rec.Header().Get("ETag")), 403)
	w2Code(t, r.do(t, "PATCH", "/v1/teams/"+id, "admin", `{"nope":1}`, "If-Match", rec.Header().Get("ETag")), 400)

	// suspend, guard, resume
	w2Code(t, r.do(t, "POST", "/v1/teams/"+id+":suspend", "viewer", ""), 403)
	w2Code(t, r.do(t, "POST", "/v1/teams/"+id+":suspend", "admin", ""), 200)
	rec = r.do(t, "PATCH", "/v1/teams/"+id, "admin", `{"displayName":"Y"}`, "If-Match", `"1"`)
	w2Code(t, rec, 409)
	if d := errOf(t, rec).Details; d["reason"] != domain.ReasonTeamSuspended {
		t.Errorf("details = %v", d)
	}
	w2Code(t, r.do(t, "PUT", "/v1/teams/"+id+"/tags", "admin", `{"tags":[]}`), 409)
	w2Code(t, r.do(t, "POST", "/v1/teams/"+id+"/api-keys", "admin", `{}`), 409)
	w2Code(t, r.do(t, "GET", "/v1/teams/"+id, "admin", ""), 200)
	w2Code(t, r.do(t, "POST", "/v1/teams/"+id+":resume", "admin", ""), 200)

	// delete: service admin only
	etag = r.do(t, "GET", "/v1/teams/"+id, "svc", "").Header().Get("ETag")
	w2Code(t, r.do(t, "DELETE", "/v1/teams/"+id, "admin", "", "If-Match", etag), 403)
	w2Code(t, r.do(t, "DELETE", "/v1/teams/"+id, "svc", ""), 428)
	rec = r.do(t, "DELETE", "/v1/teams/"+id, "svc", "", "If-Match", etag)
	w2Code(t, rec, 202)
	var tb struct {
		State  string
		Status struct{ Phase string }
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tb)
	if tb.State != "deleting" || tb.Status.Phase != "Deleting" {
		t.Errorf("deleted team = %s", rec.Body)
	}
}

func TestW2CreateTeamValidationAndReplay(t *testing.T) {
	r := newW2Rig(t)
	w2Code(t, r.do(t, "POST", "/v1/teams", "svc", `{"slug":"Bad"}`), 400)
	w2Code(t, r.do(t, "POST", "/v1/teams", "svc", `{"slug":"ok-team","displayName":"x","kind":"persistent"}`), 400)
	first := r.do(t, "POST", "/v1/teams", "svc", w2CreateBody, "Idempotency-Key", "k-1")
	w2Code(t, first, 201)
	again := r.do(t, "POST", "/v1/teams", "svc", w2CreateBody, "Idempotency-Key", "k-1")
	w2Code(t, again, 201)
	if again.Header().Get("Idempotent-Replayed") != "true" || !w2SameJSON(again.Body.Bytes(), first.Body.Bytes()) {
		t.Errorf("replay = %v\n%s\nfirst\n%s", again.Header(), again.Body, first.Body)
	}
	w2Code(t, r.do(t, "POST", "/v1/teams", "svc", strings.Replace(w2CreateBody, "Payments", "Other", 1), "Idempotency-Key", "k-1"), 409)
	w2Code(t, r.do(t, "POST", "/v1/teams", "svc", w2CreateBody, "Idempotency-Key", "bad key"), 400)
	w2Code(t, r.do(t, "GET", "/v1/teams?state=nope", "svc", ""), 400)
	w2Code(t, r.do(t, "GET", "/v1/teams?kind=nope", "svc", ""), 400)
	w2Code(t, r.do(t, "GET", "/v1/teams?maxResults=0", "svc", ""), 400)
}

func TestW2ModelsAndTags(t *testing.T) {
	r := newW2Rig(t)
	id := r.createTeam(t)
	r.teams.Catalog["m1"] = teams.ModelInfo{ID: "m1", Enabled: true}
	w2Code(t, r.do(t, "GET", "/v1/teams/"+id+"/models", "viewer", ""), 200)
	w2Code(t, r.do(t, "PUT", "/v1/teams/"+id+"/models", "admin", `{"modelIds":["m1"]}`), 403)
	w2Code(t, r.do(t, "PUT", "/v1/teams/"+id+"/models", "svc", `{}`), 400)
	rec := r.do(t, "PUT", "/v1/teams/"+id+"/models", "svc", `{"modelIds":["m1"]}`)
	w2Code(t, rec, 200)
	if rec.Header().Get("ETag") == "" {
		t.Error("no ETag")
	}
	w2Code(t, r.do(t, "PUT", "/v1/teams/"+id+"/tags", "viewer", `{"tags":[]}`), 403)
	w2Code(t, r.do(t, "PUT", "/v1/teams/"+id+"/tags", "admin", `{}`), 400)
	w2Code(t, r.do(t, "PUT", "/v1/teams/"+id+"/tags", "admin", `{"tags":[{"key":"project","value":"x"}]}`), 200)
	rec = r.do(t, "GET", "/v1/teams/"+id+"/tags", "viewer", "")
	w2Code(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"project"`) || !strings.Contains(rec.Body.String(), "requestMetadataDroppedKeys") {
		t.Errorf("tags = %s", rec.Body)
	}
}

func TestW2MembersAndKeys(t *testing.T) {
	r := newW2Rig(t)
	id := r.createTeam(t)
	body := `{"role":"team_viewer","subjectType":"group","groupName":"eng"}`
	w2Code(t, r.do(t, "POST", "/v1/teams/"+id+"/members", "viewer", body), 403)
	w2Code(t, r.do(t, "POST", "/v1/teams/"+id+"/members", "unbound", body), 404)
	rec := r.do(t, "POST", "/v1/teams/"+id+"/members", "admin", body)
	w2Code(t, rec, 201)
	var m struct {
		ID      string
		Version int64
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	w2Code(t, r.do(t, "GET", "/v1/teams/"+id+"/members", "viewer", ""), 200)
	w2Code(t, r.do(t, "PATCH", "/v1/teams/"+id+"/members/"+m.ID, "admin", `{"role":"team_admin"}`), 428)
	w2Code(t, r.do(t, "PATCH", "/v1/teams/"+id+"/members/"+m.ID, "admin", `{"role":"security_operator"}`, "If-Match", `"1"`), 400)
	w2Code(t, r.do(t, "PATCH", "/v1/teams/"+id+"/members/"+m.ID, "viewer", `{"role":"team_admin"}`, "If-Match", `"1"`), 403)
	w2Code(t, r.do(t, "PATCH", "/v1/teams/"+id+"/members/"+m.ID, "admin", `{"role":"team_admin"}`, "If-Match", `"1"`), 200)
	w2Code(t, r.do(t, "DELETE", "/v1/teams/"+id+"/members/"+m.ID, "admin", "", "If-Match", `"1"`), 412)
	w2Code(t, r.do(t, "DELETE", "/v1/teams/"+id+"/members/"+m.ID, "viewer", "", "If-Match", `"2"`), 403)
	w2Code(t, r.do(t, "DELETE", "/v1/teams/"+id+"/members/"+m.ID, "admin", "", "If-Match", `"2"`), 204)

	// API keys: viewers may not even list (D50)
	w2Code(t, r.do(t, "GET", "/v1/teams/"+id+"/api-keys", "viewer", ""), 403)
	exp := w2Now.Add(24 * time.Hour).Format(time.RFC3339)
	kb := `{"name":"ci","role":"team_viewer","expiresAt":"` + exp + `"}`
	w2Code(t, r.do(t, "POST", "/v1/teams/"+id+"/api-keys", "viewer", kb), 403)
	w2Code(t, r.do(t, "POST", "/v1/teams/"+id+"/api-keys", "admin", `{"name":"ci","role":"team_viewer","expiresAt":"nope"}`), 400)
	first := r.do(t, "POST", "/v1/teams/"+id+"/api-keys", "admin", kb, "Idempotency-Key", "key-1")
	w2Code(t, first, 201)
	var k struct {
		ID        string
		SecretKey string
	}
	_ = json.Unmarshal(first.Body.Bytes(), &k)
	if !strings.HasPrefix(k.SecretKey, "afsk_") {
		t.Fatalf("no secretKey: %s", first.Body)
	}
	replay := r.do(t, "POST", "/v1/teams/"+id+"/api-keys", "admin", kb, "Idempotency-Key", "key-1")
	w2Code(t, replay, 201)
	if replay.Header().Get("Idempotent-Replayed") != "true" || strings.Contains(replay.Body.String(), "secretKey") {
		t.Errorf("replay leaks or lacks header: %v %s", replay.Header(), replay.Body)
	}
	list := r.do(t, "GET", "/v1/teams/"+id+"/api-keys", "admin", "")
	w2Code(t, list, 200)
	if strings.Contains(list.Body.String(), k.SecretKey) || strings.Contains(list.Body.String(), "secretKey") {
		t.Errorf("list leaks the secret: %s", list.Body)
	}
	w2Code(t, r.do(t, "DELETE", "/v1/teams/"+id+"/api-keys/"+k.ID, "viewer", ""), 403)
	w2Code(t, r.do(t, "DELETE", "/v1/teams/"+id+"/api-keys/"+k.ID, "admin", ""), 204)
	w2Code(t, r.do(t, "DELETE", "/v1/teams/"+id+"/api-keys/"+k.ID, "admin", ""), 404)
}

// Every service-admin route refuses team roles before reaching a use case.
func TestW2AdminRoutesDenyNonServiceAdmins(t *testing.T) {
	r := newW2Rig(t)
	id := r.createTeam(t)
	routes := []struct{ method, path, body string }{
		{"GET", "/v1/admin/settings", ""},
		{"PUT", "/v1/admin/settings/teams.ephemeral_max_days", `{"value":30}`},
		{"GET", "/v1/admin/identity-providers", ""},
		{"POST", "/v1/admin/identity-providers", `{}`},
		{"GET", "/v1/admin/identity-providers/idp_x", ""},
		{"PATCH", "/v1/admin/identity-providers/idp_x", `{}`},
		{"DELETE", "/v1/admin/identity-providers/idp_x", ""},
		{"GET", "/v1/admin/service-tags", ""},
		{"PUT", "/v1/admin/service-tags/k", `{"mode":"fixed","value":"v"}`},
		{"DELETE", "/v1/admin/service-tags/k", ""},
		{"GET", "/v1/admin/audit-events", ""},
	}
	for _, rt := range routes {
		for _, tok := range []string{"admin", "viewer", "secop"} {
			rec := r.do(t, rt.method, rt.path, tok, rt.body, "If-Match", `"1"`)
			if rec.Code != 403 {
				t.Errorf("%s %s as %s = %d, want 403", rt.method, rt.path, tok, rec.Code)
			}
		}
	}
	// team-scoped reads of audit and revisions: viewers are denied or allowed per the matrix, and
	// an unbound caller is always 404.
	for _, p := range []string{"/audit-events", "/revisions", "/revisions/1"} {
		if rec := r.do(t, "GET", "/v1/teams/"+id+p, "unbound", ""); rec.Code != 404 {
			t.Errorf("unbound GET %s = %d", p, rec.Code)
		}
	}
	w2Code(t, r.do(t, "GET", "/v1/teams/"+id+"/revisions/abc", "svc", ""), 400)
}

// w2SameJSON compares two JSON documents ignoring key order (the store keeps bodies as jsonb).
func w2SameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

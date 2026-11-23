package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/api"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

const (
	teamA = "team_01JD3Z5Y8K2M4N6P8Q0R2S4T6V"
	teamB = "team_01JD3Z5Y8K2M4N6P8Q0R2S4T6W"
)

// fakeAuth maps a bearer token to a principal.
type fakeAuth map[string]*authz.Principal

func (f fakeAuth) Authenticate(r *http.Request) (*authz.Principal, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return nil, nil
	}
	p, ok := f[strings.TrimPrefix(h, "Bearer ")]
	if !ok {
		return nil, domain.Unauthenticated()
	}
	return p, nil
}

type fakeTeams map[string]authz.TeamFlags

func (f fakeTeams) TeamFlags(_ context.Context, id string) (authz.TeamFlags, bool, error) {
	fl, ok := f[id]
	return fl, ok, nil
}

var principals = fakeAuth{
	"admin-a":  {ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: teamA}}},
	"viewer-a": {ID: "usr_v", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: teamA}}},
	"svc":      {ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true},
	"secop":    {ID: "usr_o", Type: authz.PrincipalUser, SecurityOperator: true},
	"ghost-a":  {ID: "usr_g", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: "team_01JD3Z5Y8K2M4N6P8Q0R2S4T6X"}}},
}

type recorder struct {
	denied   []Denial
	notFound []string
}

func newServer(t *testing.T, regs ...Registrar) (http.Handler, *recorder) {
	t.Helper()
	rec := &recorder{}
	if regs == nil {
		regs = []Registrar{none}
	}
	h, err := New(Config{
		AllowUnbound: true,
		Auth:         principals,
		Teams:        fakeTeams{teamA: {}, teamB: {}},
		Hooks: Hooks{
			Denied:            func(_ context.Context, d Denial) { rec.denied = append(rec.denied, d) },
			CrossTeamNotFound: func(_ context.Context, op string) { rec.notFound = append(rec.notFound, op) },
		},
		Registrars: regs,
		Now:        func() time.Time { return time.Unix(1_800_000_000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, rec
}

func do(t *testing.T, h http.Handler, method, path, token string, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errOf(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var b errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	if b.Details == nil || b.RequestID == "" || b.Code == "" {
		t.Fatalf("envelope incomplete: %s", rec.Body.String())
	}
	return b
}

// M0-D1: every route declares a permission or is explicitly public or any-signed-in.
func TestEveryRouteDeclaresAPermission(t *testing.T) {
	if len(Routes) != 194 {
		t.Fatalf("routes = %d, want the 194 contract operations", len(Routes))
	}
	for _, rt := range Routes {
		explicit := rt.Scope == authz.ScopePublic || rt.Scope == authz.ScopeAny
		if rt.Permission == "" && len(rt.AnyOf) == 0 && !explicit {
			t.Errorf("%s has no permission and is not public", rt.OperationID)
		}
		if rt.Scope == authz.ScopeTeam && !strings.Contains(rt.Path, "{teamId}") {
			t.Errorf("%s is team-scoped without {teamId} in %s", rt.OperationID, rt.Path)
		}
		if rt.Permission != "" && !rt.Permission.Known() {
			t.Errorf("%s: unknown permission %s", rt.OperationID, rt.Permission)
		}
	}
}

func TestEveryOperationIsRoutedToAPlaceholder(t *testing.T) {
	h, _ := newServer(t)
	for _, rt := range Routes {
		if rt.OperationID == "GetHealth" || rt.OperationID == "GetReadiness" {
			continue
		}
		path := strings.NewReplacer("{teamId}", teamA, "{kind}", "teams").Replace(rt.Path)
		for strings.Contains(path, "{") {
			i, j := strings.Index(path, "{"), strings.Index(path, "}")
			path = path[:i] + "x1" + path[j+1:]
		}
		rec := do(t, h, rt.Method, path, "svc", "")
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s %s %s: %d %s", rt.OperationID, rt.Method, path, rec.Code, rec.Body.String())
			continue
		}
		b := errOf(t, rec)
		if b.Code != domain.KindInternal || b.Details["reason"] != domain.ReasonNotImplemented || !strings.Contains(b.Message, rt.OperationID) {
			t.Errorf("%s: placeholder body %+v", rt.OperationID, b)
		}
	}
}

func TestVerbDispatch(t *testing.T) {
	var got []string
	h, _ := newServer(t, func(g *Registry) error {
		for _, op := range []string{"GetTeam", "SuspendTeam", "ResumeTeam"} {
			if err := g.Handle(op, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = append(got, op+":"+r.PathValue("teamId"))
				w.WriteHeader(http.StatusNoContent)
			})); err != nil {
				return err
			}
		}
		return nil
	})
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/teams/" + teamA}, {"POST", "/v1/teams/" + teamA + ":suspend"}, {"POST", "/v1/teams/" + teamA + ":resume"},
	} {
		if rec := do(t, h, c.method, c.path, "svc", ""); rec.Code != http.StatusNoContent {
			t.Fatalf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
	want := []string{"GetTeam:" + teamA, "SuspendTeam:" + teamA, "ResumeTeam:" + teamA}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v", got)
	}
	for _, path := range []string{"/v1/teams/" + teamA + ":explode", "/v1/teams/:suspend", "/v1/nope"} {
		if rec := do(t, h, "POST", path, "svc", ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
}

// A colon in the last segment is part of the id unless it introduces a registered verb: Bedrock model ids
// end in ":0", and a team id followed by ":suspend" is still the verb.
func TestColonIDIsNotAVerb(t *testing.T) {
	var gotModel, gotTeam string
	h, _ := newServer(t, func(g *Registry) error {
		if err := g.Handle("UpdateModel", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotModel = r.PathValue("modelId")
			w.WriteHeader(http.StatusNoContent)
		})); err != nil {
			return err
		}
		return g.Handle("SuspendTeam", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotTeam = r.PathValue("teamId")
			w.WriteHeader(http.StatusNoContent)
		}))
	})
	const model = "anthropic.claude-3-5-sonnet-20241022-v2:0"
	if rec := do(t, h, "PATCH", "/v1/admin/models/"+model, "svc", `{}`); rec.Code != http.StatusNoContent || gotModel != model {
		t.Fatalf("model id with a colon: %d, got %q", rec.Code, gotModel)
	}
	if rec := do(t, h, "POST", "/v1/teams/"+teamA+":suspend", "svc", ""); rec.Code != http.StatusNoContent || gotTeam != teamA {
		t.Fatalf("real verb: %d, got %q", rec.Code, gotTeam)
	}
}

func TestRegistry(t *testing.T) {
	ok := http.NotFoundHandler()
	if _, err := New(Config{Registrars: []Registrar{func(g *Registry) error { return g.Handle("NoSuchOp", ok) }}}); err == nil {
		t.Fatal("unknown op bound")
	}
	if _, err := New(Config{Registrars: []Registrar{func(g *Registry) error { return g.Handle("GetHealth", ok) }}}); err == nil {
		t.Fatal("double bind")
	}
	g := &Registry{ops: map[string]*operation{"A": {}, "B": {}}, bound: map[string]bool{}}
	_ = g.Handle("B", ok)
	_ = g.Handle("A", ok)
	if strings.Join(g.Bound(), ",") != "A,B" {
		t.Fatal(g.Bound())
	}
	if h := NewHandler(nil); h == nil {
		t.Fatal("nil handler")
	}
}

// M0-D2.
func TestRequestIDAndContractVersion(t *testing.T) {
	h, _ := newServer(t)
	for _, c := range []struct {
		name, sent string
		echoed     bool
	}{
		{"client id echoed", "abc-123", true},
		{"too long replaced", strings.Repeat("x", 65), false},
		{"non-printable replaced", "a\x01b", false},
		{"missing generated", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			// 401, 404 and success all carry both headers.
			for _, path := range []string{"/v1/me", "/v1/nope", "/healthz"} {
				rec := do(t, h, "GET", path, "", "", "X-Request-Id", c.sent)
				rid := rec.Header().Get("X-Request-Id")
				if (rid == c.sent) != c.echoed || rid == "" {
					t.Errorf("%s: X-Request-Id %q for sent %q", path, rid, c.sent)
				}
				if !c.echoed && !strings.HasPrefix(rid, "req_") {
					t.Errorf("generated id %q", rid)
				}
				if rec.Header().Get("X-Afs-Contract-Version") != api.ContractVersion {
					t.Errorf("%s: no contract version", path)
				}
				if rec.Code >= 400 && errOf(t, rec).RequestID != rid {
					t.Errorf("%s: body request id differs", path)
				}
			}
		})
	}
}

func TestStrictQueryAndBody(t *testing.T) {
	var decoded map[string]any
	h, _ := newServer(t, func(g *Registry) error {
		for _, op := range []string{"UpdateTeam", "ListAgents"} {
			if err := g.Handle(op, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPatch {
					var v struct {
						DisplayName string `json:"displayName"`
						TeamID      string `json:"teamId"`
					}
					if err := Decode(r, &v); err != nil {
						WriteError(w, r, err)
						return
					}
					decoded = map[string]any{"displayName": v.DisplayName}
				}
				w.WriteHeader(http.StatusOK)
			})); err != nil {
				return err
			}
		}
		return nil
	})
	path := "/v1/teams/" + teamA
	if rec := do(t, h, "GET", path+"/agents?maxResults=5", "admin-a", ""); rec.Code != http.StatusOK {
		t.Fatalf("declared query refused: %d %s", rec.Code, rec.Body.String())
	}
	rec := do(t, h, "GET", path+"/agents?bogus=1&maxResults=5", "admin-a", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "bogus") {
		t.Fatalf("unknown query: %d %s", rec.Code, rec.Body.String())
	}
	cases := []struct {
		name, body string
		code       int
	}{
		{"ok", `{"displayName":"Payments"}`, 200},
		{"same team in body", `{"displayName":"P","teamId":"` + teamA + `"}`, 200},
		{"other team in body", `{"displayName":"P","teamId":"` + teamB + `"}`, 400},
		{"unknown field", `{"nope":1}`, 400},
		{"wrong type", `{"displayName":1}`, 400},
		{"trailing", `{"displayName":"P"} {}`, 400},
		{"too large", `{"displayName":"` + strings.Repeat("x", DefaultBodyLimit) + `"}`, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, h, "PATCH", path, "admin-a", c.body)
			if rec.Code != c.code {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
		})
	}
	if decoded["displayName"] != "P" {
		t.Fatalf("decoded %v", decoded)
	}
}

func TestPanicRecovery(t *testing.T) {
	h, _ := newServer(t, func(g *Registry) error {
		return g.Handle("GetMe", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	})
	rec := do(t, h, "GET", "/v1/me", "admin-a", "")
	if rec.Code != http.StatusInternalServerError || errOf(t, rec).Message != "internal error" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestErrorMapping(t *testing.T) {
	conflict := domain.Conflict(domain.ReasonIdempotencyInProgress, "busy", map[string]any{"missingTags": []string{"env"}})
	conflict.RetryAfterSeconds = 2
	cases := []struct {
		err  error
		code int
	}{
		{domain.Validation(domain.FieldError{Field: "name", Reason: "bad"}), 400},
		{domain.Unauthenticated(), 401},
		{domain.AccessDenied("teams:write"), 403},
		{domain.NotFound("team"), 404},
		{conflict, 409},
		{domain.PreconditionFailed(), 412},
		{domain.PreconditionRequired(), 428},
		{&domain.Error{Kind: domain.KindThrottled, RetryAfterSeconds: 1}, 429},
		{domain.Internal(errors.New("secret detail")), 500},
		{domain.Unavailable("database", errors.New("down")), 503},
		{&domain.Error{Kind: domain.KindBudgetExceeded}, 500},
		{errors.New("plain"), 500},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		WriteError(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/", nil), c.err)
		if rec.Code != c.code {
			t.Errorf("%v: %d want %d", c.err, rec.Code, c.code)
		}
		if strings.Contains(rec.Body.String(), "secret detail") {
			t.Error("internal cause leaked")
		}
	}
	rec := httptest.NewRecorder()
	WriteError(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/", nil), conflict)
	var b map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	d := b["details"].(map[string]any)
	if d["reason"] != domain.ReasonIdempotencyInProgress || d["missingTags"] == nil || rec.Header().Get("Retry-After") != "2" {
		t.Fatalf("details %v", b)
	}
	rec = httptest.NewRecorder()
	WriteError(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/", nil), domain.AccessDenied("teams:write"))
	if !strings.Contains(rec.Body.String(), `"requiredPermission":"teams:write"`) {
		t.Fatal(rec.Body.String())
	}
	rec = httptest.NewRecorder()
	WriteError(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/", nil), domain.Validation(domain.FieldError{Field: "name", Reason: "bad"}))
	if !strings.Contains(rec.Body.String(), `"fields":[{"field":"name","reason":"bad"}]`) {
		t.Fatal(rec.Body.String())
	}
}

// M0-D3: PRD §3 in order.
func TestAuthorizationRule(t *testing.T) {
	h, rec := newServer(t, func(g *Registry) error {
		for _, op := range []string{"GetTeam", "UpdateTeam", "ListTeamDirectory", "GetMe", "ListTeams"} {
			if err := g.Handle(op, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, ok := PrincipalFrom(r.Context()); !ok {
					t.Error("no principal in context")
				}
				w.WriteHeader(http.StatusOK)
			})); err != nil {
				return err
			}
		}
		return nil
	})
	unknownTeam := "team_01JD3Z5Y8K2M4N6P8Q0R2S4T6X"
	cases := []struct {
		name, token, method, path string
		code                      int
		perm                      string
	}{
		{"no credential", "", "GET", "/v1/teams/" + teamA, 401, ""},
		{"bad credential", "nope", "GET", "/v1/teams/" + teamA, 401, ""},
		{"public needs nothing", "", "GET", "/healthz", 200, ""},
		{"bound viewer reads", "viewer-a", "GET", "/v1/teams/" + teamA, 200, ""},
		{"unbound gets 404", "viewer-a", "GET", "/v1/teams/" + teamB, 404, ""},
		{"bound to unknown team gets 404", "ghost-a", "GET", "/v1/teams/" + unknownTeam, 404, ""},
		{"security operator on team route", "secop", "GET", "/v1/teams/" + teamA, 403, ""},
		{"security operator on unknown team", "secop", "GET", "/v1/teams/" + unknownTeam, 403, ""},
		{"viewer lacks write", "viewer-a", "PATCH", "/v1/teams/" + teamA, 403, "teams:write"},
		{"unbound write is 404 not 403", "viewer-a", "PATCH", "/v1/teams/" + teamB, 404, ""},
		{"service admin anywhere", "svc", "PATCH", "/v1/teams/" + teamB, 200, ""},
		{"service route denied", "admin-a", "GET", "/v1/team-directory", 403, "teams:read-directory"},
		{"service route for operator", "secop", "GET", "/v1/team-directory", 200, ""},
		{"any route signed in", "viewer-a", "GET", "/v1/me", 200, ""},
		{"any route anonymous", "", "GET", "/v1/me", 401, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := do(t, h, c.method, c.path, c.token, "")
			if r.Code != c.code {
				t.Fatalf("%d %s", r.Code, r.Body.String())
			}
			if c.code >= 400 {
				b := errOf(t, r)
				if c.perm != "" && b.Details["requiredPermission"] != c.perm {
					t.Fatalf("details %v", b.Details)
				}
			}
		})
	}
	if len(rec.denied) != 4 || len(rec.notFound) != 3 {
		t.Fatalf("hooks: denied %d notFound %d", len(rec.denied), len(rec.notFound))
	}
	if d := rec.denied[2]; d.OperationID != "UpdateTeam" || d.TeamID != teamA || d.RequiredPermission != "teams:write" {
		t.Fatalf("denial %+v", d)
	}
}

type errTeams struct{}

func (errTeams) TeamFlags(context.Context, string) (authz.TeamFlags, bool, error) {
	return authz.TeamFlags{}, false, domain.Unavailable("database", nil)
}

type errAuth struct{ err error }

func (e errAuth) Authenticate(*http.Request) (*authz.Principal, error) { return nil, e.err }

func TestAuthorizationDependencyFailures(t *testing.T) {
	h, err := New(Config{AllowUnbound: true, Auth: principals, Teams: errTeams{}, Registrars: []Registrar{none}})
	if err != nil {
		t.Fatal(err)
	}
	if r := do(t, h, "GET", "/v1/teams/"+teamA, "admin-a", ""); r.Code != 503 {
		t.Fatalf("team lookup down: %d", r.Code)
	}
	for _, c := range []struct {
		err  error
		code int
	}{{domain.Unavailable("idp", nil), 503}, {errors.New("weird"), 401}} {
		h, _ := New(Config{AllowUnbound: true, Auth: errAuth{c.err}, Registrars: []Registrar{none}})
		if r := do(t, h, "GET", "/v1/me", "x", ""); r.Code != c.code {
			t.Errorf("%v: %d", c.err, r.Code)
		}
	}
	h, _ = New(Config{AllowUnbound: true, Registrars: []Registrar{none}})
	if r := do(t, h, "GET", "/v1/me", "x", ""); r.Code != 401 {
		t.Fatalf("no authenticator: %d", r.Code)
	}
}

// M0-D4.
func TestHealthReadinessAndOpenAPI(t *testing.T) {
	h, err := New(Config{AllowUnbound: true, Ready: map[string]ReadinessCheck{
		"database": func(*http.Request) error { return nil },
		"schema":   func(*http.Request) error { return errors.New("have 000015, want 000016") },
	}, Registrars: []Registrar{none}})
	if err != nil {
		t.Fatal(err)
	}
	r := do(t, h, "GET", "/readyz", "", "")
	var hs healthStatus
	_ = json.Unmarshal(r.Body.Bytes(), &hs)
	if r.Code != 503 || hs.Status != "not ready" || hs.Checks["database"] != "ok" || hs.Checks["schema"] != "unavailable" || strings.Contains(r.Body.String(), "000015") {
		t.Fatalf("%d %s", r.Code, r.Body.String())
	}
	r = do(t, h, "GET", "/v1/openapi.yaml", "", "")
	if r.Code != 200 || !bytes.Equal(r.Body.Bytes(), api.OpenAPI) || r.Header().Get("Content-Type") != "application/yaml" {
		t.Fatalf("openapi: %d", r.Code)
	}
	if r := do(t, h, "GET", "/healthz?x=1", "", ""); r.Code != 400 {
		t.Fatalf("health strict query: %d", r.Code)
	}
}

// M0-D5.
func TestPageTokens(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pt := NewPageTokens([]byte("0123456789abcdef0123456789abcdef"), func() time.Time { return now })
	tok := pt.Encode("ListAgents", "usr_a", "agt_5")
	if pt.Encode("ListAgents", "usr_a", "") != "" {
		t.Fatal("last page has a token")
	}
	if c, err := pt.Decode("ListAgents", "usr_a", tok); err != nil || c != "agt_5" {
		t.Fatalf("%q %v", c, err)
	}
	for name, f := range map[string]func() error{
		"other op":     func() error { _, err := pt.Decode("ListTeams", "usr_a", tok); return err },
		"other caller": func() error { _, err := pt.Decode("ListAgents", "usr_b", tok); return err },
		"tampered":     func() error { _, err := pt.Decode("ListAgents", "usr_a", "x"+tok); return err },
		"garbage":      func() error { _, err := pt.Decode("ListAgents", "usr_a", "nodot"); return err },
		"other key": func() error {
			_, err := NewPageTokens([]byte("another-key-another-key-another!!"), nil).Decode("ListAgents", "usr_a", tok)
			return err
		},
		"expired": func() error {
			later := NewPageTokens([]byte("0123456789abcdef0123456789abcdef"), func() time.Time { return now.Add(25 * time.Hour) })
			_, err := later.Decode("ListAgents", "usr_a", tok)
			return err
		},
	} {
		if err := f(); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s: %v", name, err)
		}
	}
	req := func(q string) *http.Request {
		return httptest.NewRequestWithContext(t.Context(), "GET", "/v1/x?"+q, nil)
	}
	if p, err := pt.ParsePage(req(""), "ListAgents", "usr_a"); err != nil || p.Limit != 50 || p.Cursor != "" {
		t.Fatal(p, err)
	}
	if p, err := pt.ParsePage(req("maxResults=100&nextToken="+tok), "ListAgents", "usr_a"); err != nil || p.Limit != 100 || p.Cursor != "agt_5" {
		t.Fatal(p, err)
	}
	for _, q := range []string{"maxResults=0", "maxResults=101", "maxResults=x", "nextToken=bad"} {
		if _, err := pt.ParsePage(req(q), "ListAgents", "usr_a"); err == nil {
			t.Errorf("%s accepted", q)
		}
	}
}

func TestCSV(t *testing.T) {
	var csvOp string
	for _, rt := range Routes {
		if rt.CSV {
			csvOp = rt.OperationID
			break
		}
	}
	var wants []bool
	h, _ := newServer(t, func(g *Registry) error {
		for _, op := range []string{csvOp, "GetMe"} {
			if err := g.Handle(op, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wants = append(wants, WantsCSV(r))
				_ = WriteCSV(w, []string{"a", "b"}, [][]string{{"=cmd()", "-1.5"}, {"-x", "@y"}, {"", "plain"}}, "tok")
			})); err != nil {
				return err
			}
		}
		return nil
	})
	var path, method string
	for _, rt := range Routes {
		if rt.OperationID == csvOp {
			path, method = strings.ReplaceAll(rt.Path, "{teamId}", teamA), rt.Method
		}
	}
	for strings.Contains(path, "{") {
		i, j := strings.Index(path, "{"), strings.Index(path, "}")
		path = path[:i] + "teams" + path[j+1:]
	}
	r := do(t, h, method, path, "svc", "", "Accept", "application/json, text/csv;q=0.9")
	if r.Header().Get("X-Afs-Next-Token") != "tok" || !strings.HasPrefix(r.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("%s %s: %d %v", method, path, r.Code, r.Header())
	}
	if got := r.Body.String(); got != "a,b\n'=cmd(),-1.5\n'-x,'@y\n,plain\n" {
		t.Fatalf("csv %q", got)
	}
	_ = do(t, h, "GET", "/v1/me", "svc", "", "Accept", "text/csv")
	if len(wants) != 2 || !wants[0] || wants[1] {
		t.Fatalf("WantsCSV %v", wants)
	}
	if WantsCSV(httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)) {
		t.Fatal("no route")
	}
}

func TestSplitVerb(t *testing.T) {
	for _, c := range []struct{ in, base, verb, wc string }{
		{"/v1/teams/{teamId}:suspend", "/v1/teams/{teamId}", "suspend", "teamId"},
		{"/v1/teams/{teamId}", "/v1/teams/{teamId}", "", "teamId"},
		{"/v1/teams", "/v1/teams", "", ""},
		{"/v1/admin/exports:import", "/v1/admin/exports:import", "", ""},
	} {
		b, v, w := splitVerb(c.in)
		if b != c.base || v != c.verb || w != c.wc {
			t.Errorf("%s: %s %s %s", c.in, b, v, w)
		}
	}
	if _, ok := RouteFrom(httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)); ok {
		t.Fatal("route outside the router")
	}
}

// M1-L2: the router tells the metrics middleware which operation a request matched.
func TestOnRouteReportsTheMatchedOperation(t *testing.T) {
	var got []string
	h, err := New(Config{
		AllowUnbound: true,
		Auth:         principals, Teams: fakeTeams{teamA: {}}, Registrars: []Registrar{none},
		OnRoute: func(_ context.Context, op string) { got = append(got, op) },
		Now:     func() time.Time { return time.Unix(1_800_000_000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	do(t, h, "GET", "/v1/me", "", "")
	do(t, h, "GET", "/v1/nope", "", "")
	if len(got) != 1 || got[0] != "GetMe" {
		t.Errorf("OnRoute calls = %v, want [GetMe] (an unmatched path reports nothing)", got)
	}
}

// M3-X1: the report operations accept tag.<key> filters (M3-H2) next to the verb split of M2: a
// ":verb" path keeps its base pattern, and an unknown query parameter is still refused everywhere else.
func TestTagFiltersAreOnlyForReportOperations(t *testing.T) {
	report := &operation{Route: Route{OperationID: "GetTeamReport"}, query: map[string]bool{"from": true}}
	other := &operation{Route: Route{OperationID: "ListAgents"}, query: map[string]bool{"maxResults": true}}
	q := func(raw string) *http.Request {
		return httptest.NewRequestWithContext(t.Context(), "GET", "/v1/x?"+raw, nil)
	}
	if err := checkQuery(report, q("from=2026-11-01T00:00:00Z&tag.costCenter=CC1")); err != nil {
		t.Errorf("tag filter on a report: %v", err)
	}
	if err := checkQuery(report, q("tag.=x")); err == nil {
		t.Error("an empty tag key must be refused")
	}
	if err := checkQuery(report, q("bogus=1")); err == nil {
		t.Error("an unknown parameter on a report must be refused")
	}
	if err := checkQuery(other, q("tag.costCenter=CC1")); err == nil {
		t.Error("a tag filter on a non-report operation must be refused")
	}
	if b, v, _ := splitVerb("/v1/teams/{teamId}/agents/{agentId}:pause"); b != "/v1/teams/{teamId}/agents/{agentId}" || v != "pause" {
		t.Errorf("verb split = %s %s", b, v)
	}
}

// FR-R15 AC2, AC3: New refuses a contract operation nobody bound, naming it, unless AllowUnbound is
// set; no database is needed to prove it.
func TestNewRefusesUnboundOperations(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	one := []Registrar{func(g *Registry) error { return g.Handle("ListTeams", ok) }}
	_, err := New(Config{Registrars: one})
	if err == nil || !strings.Contains(err.Error(), "unbound") || !strings.Contains(err.Error(), "CreateTeam") {
		t.Fatalf("err = %v, want an error naming an unbound operation such as CreateTeam", err)
	}
	if _, err := New(Config{Registrars: one, AllowUnbound: true}); err != nil {
		t.Fatalf("AllowUnbound: %v", err)
	}
	if _, err := New(Config{}); err == nil {
		t.Fatal("no registrars at all must also be refused")
	}
}

func none(*Registry) error { return nil }

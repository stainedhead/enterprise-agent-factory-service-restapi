package middleware_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/api"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/middleware"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamB = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
)

type creds map[string]*authz.Principal

func (c creds) Authenticate(_ context.Context, tok string) (*authz.Principal, error) {
	if p, ok := c[tok]; ok {
		return p, nil
	}
	return nil, domain.Unauthenticated()
}

type env struct {
	h       http.Handler
	runner  *fakes.TxRunner
	audit   *fakes.Audit
	auditor *middleware.DeniedAuditor

	mu      sync.Mutex
	created int // rows "inserted" by CreateApiKey, committed
	// createFn runs inside the CreateApiKey transaction.
	createFn func(tx port.Tx) error
	// version served by GetTeam / UpdateTeam.
	version int64
	logs    *strings.Builder
}

func routeInfo(r *http.Request) (middleware.RouteInfo, bool) {
	rt, ok := httpapi.RouteFrom(r)
	if !ok {
		return middleware.RouteInfo{}, false
	}
	return middleware.RouteInfo{OperationID: rt.OperationID, Method: rt.Method, Public: rt.Scope == authz.ScopePublic, Idempotent: rt.Idempotent, Mutation: rt.Mutation}, true
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{runner: &fakes.TxRunner{}, audit: &fakes.Audit{}, version: 3, logs: &strings.Builder{}}
	log := slogTo(e.logs)
	limiter := middleware.NewLimiter(middleware.LimiterConfig{Now: func() time.Time { return time.Unix(1_800_000_000, 0) }})
	principals := creds{
		"admin-a":  {ID: "usr_admin_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: teamA}}},
		"admin-a2": {ID: "usr_admin_a2", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: teamA}}},
		"viewer-a": {ID: "usr_viewer_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: teamA}}},
		"outsider": {ID: "usr_out", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: teamB}}},
	}
	e.auditor = &middleware.DeniedAuditor{Audit: e.audit, Session: auth.SessionOf, RequestID: httpapi.RequestIDFrom, Log: log}
	required, err := middleware.IfMatchRules(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}

	register := func(g *httpapi.Registry) error {
		must := func(op string, h http.HandlerFunc) {
			if err := g.Handle(op, h); err != nil {
				t.Fatal(err)
			}
		}
		must("CreateApiKey", func(w http.ResponseWriter, r *http.Request) {
			p, _ := httpapi.PrincipalFrom(r.Context())
			err := e.runner.InTx(r.Context(), auth.SessionOf(p, httpapi.RequestIDFrom(r.Context())), func(_ context.Context, tx port.Tx) error {
				if e.createFn != nil {
					if err := e.createFn(tx); err != nil {
						return err
					}
				}
				e.mu.Lock()
				defer e.mu.Unlock()
				e.created++
				return nil
			})
			if err != nil {
				httpapi.WriteError(w, r, err)
				return
			}
			e.mu.Lock()
			n := e.created
			e.mu.Unlock()
			httpapi.WriteJSON(w, http.StatusCreated, map[string]any{"id": "key_" + strconv.Itoa(n), "name": "ci", "secretKey": "afsk_" + strconv.Itoa(n) + "_secret"})
		})
		must("AddMember", func(w http.ResponseWriter, _ *http.Request) { // never opens its transaction through the runner
			httpapi.WriteJSON(w, http.StatusCreated, map[string]any{"id": "rb_1"})
		})
		must("GetTeam", func(w http.ResponseWriter, _ *http.Request) {
			httpapi.WriteJSON(w, http.StatusOK, map[string]any{"id": teamA, "version": e.version})
		})
		must("UpdateTeam", func(w http.ResponseWriter, r *http.Request) {
			if err := middleware.CheckIfMatch(r, domain.Version(e.version)); err != nil {
				httpapi.WriteError(w, r, err)
				return
			}
			e.version++
			httpapi.WriteJSON(w, http.StatusOK, map[string]any{"id": teamA, "version": e.version})
		})
		must("ListTeamModels", func(w http.ResponseWriter, _ *http.Request) { // a 403 produced by the handler
			httpapi.WriteError(w, nil2(), domain.AccessDenied(string(authz.TeamsSetGuardrails)))
		})
		must("ListSignInProviders", func(w http.ResponseWriter, _ *http.Request) {
			httpapi.WriteJSON(w, http.StatusOK, map[string]any{"items": []any{}})
		})
		return nil
	}
	h, err := httpapi.New(httpapi.Config{
		AllowUnbound: true,
		Auth:         &middleware.Authn{Auth: principals, Limiter: limiter},
		Registrars:   []httpapi.Registrar{register},
		Outer:        []httpapi.Middleware{middleware.WithRequestMeta(0)},
		PreAuth:      []httpapi.Middleware{middleware.PublicLimit(limiter, routeInfo, httpapi.WriteError)},
		PostAuth: []httpapi.Middleware{
			e.auditor.Stage(routeInfo, httpapi.PrincipalFrom),
			middleware.RequireIfMatch(required, routeInfo, httpapi.WriteError),
			middleware.AutoETag(),
		},
		Hooks: httpapi.Hooks{
			Denied: func(ctx context.Context, d httpapi.Denial) {
				e.auditor.Record(ctx, middleware.Denied{Principal: d.Principal, OperationID: d.OperationID, TeamID: d.TeamID, Required: d.RequiredPermission})
			},
			CrossTeamNotFound: e.auditor.CrossTeamNotFound,
		},
		Now: func() time.Time { return time.Unix(1_800_000_000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.h = h
	return e
}

func nil2() *http.Request {
	return httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
}

func (e *env) do(t *testing.T, method, path, token, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequestWithContext(t.Context(), method, path, rd)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	r.RemoteAddr = "198.51.100.1:4000"
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	return rec
}

type errEnvelope struct {
	Code    domain.ErrorKind `json:"code"`
	Details map[string]any   `json:"details"`
}

func envelope(t *testing.T, rec *httptest.ResponseRecorder) errEnvelope {
	t.Helper()
	var b errEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("not an error envelope: %q", rec.Body.String())
	}
	return b
}

const keysPath = "/v1/teams/" + teamA + "/api-keys"

// RA-US-1.7, RA-US-6.3: every 403 writes one denied audit record, the request's own transaction writes
// nothing; a cross-team 404 is counted.
func TestDeniedAudit(t *testing.T) {
	e := newEnv(t)
	// Route-level 403: a viewer creating an API key.
	if r := e.do(t, "POST", keysPath, "viewer-a", `{}`, "User-Agent", "afs-test/1"); r.Code != 403 {
		t.Fatalf("%d", r.Code)
	}
	if len(e.audit.Entries) != 1 {
		t.Fatalf("audit entries = %d", len(e.audit.Entries))
	}
	en := e.audit.Entries[0]
	if !en.Own || en.Record.Outcome != port.OutcomeDenied || en.Record.Action != "CreateApiKey" || en.Record.TeamID != teamA ||
		en.Session.Actor != "usr_viewer_a" || en.Session.ActorType != port.ActorUser || en.Session.RequestID == "" ||
		en.Record.UserAgent != "afs-test/1" || en.Record.SourceIP != "198.51.100.1" {
		t.Fatalf("entry = %+v", en)
	}
	if after, _ := en.Record.After.(map[string]any); after["requiredPermission"] != "api-keys:write" {
		t.Fatalf("after = %v", en.Record.After)
	}
	if e.created != 0 || e.runner.Commits != 0 {
		t.Fatal("the denied request wrote")
	}

	// Handler-produced 403 (a use case refusing): audited once, response intact.
	rec := e.do(t, "GET", "/v1/teams/"+teamA+"/models", "admin-a", "")
	if rec.Code != 403 || envelope(t, rec).Details["requiredPermission"] != "teams:set-guardrails" {
		t.Fatalf("handler 403: %d %s", rec.Code, rec.Body)
	}
	if len(e.audit.Entries) != 2 || e.audit.Entries[1].Record.Action != "ListTeamModels" ||
		e.audit.Entries[1].Record.After.(map[string]any)["requiredPermission"] != "teams:set-guardrails" {
		t.Fatalf("entries = %+v", e.audit.Entries)
	}

	// A 404 for an unbound caller is not audited as a denial but is counted.
	if r := e.do(t, "GET", "/v1/teams/"+teamA, "outsider", ""); r.Code != 404 {
		t.Fatalf("%d", r.Code)
	}
	if len(e.audit.Entries) != 2 || e.auditor.CrossTeamCount() != 1 {
		t.Fatalf("entries=%d crossTeam=%d", len(e.audit.Entries), e.auditor.CrossTeamCount())
	}
	// An allowed request writes no denial.
	if r := e.do(t, "GET", "/v1/teams/"+teamA, "admin-a", ""); r.Code != 200 || len(e.audit.Entries) != 2 {
		t.Fatalf("%d entries=%d", r.Code, len(e.audit.Entries))
	}
}

// RA-US-6.1 (part), CT-API-6, CT-API-13.
func TestIfMatchAndETag(t *testing.T) {
	e := newEnv(t)
	get := e.do(t, "GET", "/v1/teams/"+teamA, "admin-a", "")
	if get.Header().Get("ETag") != `"3"` {
		t.Fatalf("ETag = %q", get.Header().Get("ETag"))
	}
	if r := e.do(t, "PATCH", "/v1/teams/"+teamA, "admin-a", `{}`); r.Code != 428 || envelope(t, r).Code != domain.KindPreconditionRequired {
		t.Fatalf("missing If-Match: %d %s", r.Code, r.Body)
	}
	if r := e.do(t, "PATCH", "/v1/teams/"+teamA, "admin-a", `{}`, "If-Match", "abc"); r.Code != 400 {
		t.Fatalf("malformed If-Match: %d", r.Code)
	}
	if r := e.do(t, "PATCH", "/v1/teams/"+teamA, "admin-a", `{}`, "If-Match", `"2"`); r.Code != 412 {
		t.Fatalf("stale: %d %s", r.Code, r.Body)
	}
	ok := e.do(t, "PATCH", "/v1/teams/"+teamA, "admin-a", `{}`, "If-Match", `"3"`)
	if ok.Code != 200 || ok.Header().Get("ETag") != `"4"` {
		t.Fatalf("current: %d ETag %q", ok.Code, ok.Header().Get("ETag"))
	}
	// A caller who may not know the team exists gets 404, not 428.
	if r := e.do(t, "PATCH", "/v1/teams/"+teamA, "outsider", `{}`); r.Code != 404 {
		t.Fatalf("outsider without If-Match: %d", r.Code)
	}
	if r := e.do(t, "PATCH", "/v1/teams/"+teamA, "viewer-a", `{}`); r.Code != 403 {
		t.Fatalf("viewer without If-Match: %d", r.Code)
	}
	// A list has no ETag; an explicit handler ETag wins.
	if l := e.do(t, "GET", "/v1/identity-providers", "", ""); l.Header().Get("ETag") != "" {
		t.Fatal("a list got an ETag")
	}
}

func TestIfMatchRulesFromOpenAPI(t *testing.T) {
	req, err := middleware.IfMatchRules(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"UpdateTeam", "DeleteTeam", "UpdateMember", "RemoveMember", "UpdateAgent"} {
		if !req[op] {
			t.Errorf("%s should require If-Match", op)
		}
	}
	// CQ-2: an API key has no version to send, so its delete does not demand If-Match.
	for _, op := range []string{"GetTeam", "CreateTeam", "ListTeams", "SuspendTeam", "PutTeamModels", "DeleteApiKey"} {
		if req[op] {
			t.Errorf("%s should not require If-Match", op)
		}
	}
	if len(req) < 40 {
		t.Errorf("only %d operations require If-Match", len(req))
	}
}

func TestCheckIfMatch(t *testing.T) {
	cases := []struct {
		hdr  string
		want domain.ErrorKind
	}{
		{"", domain.KindPreconditionRequired},
		{`"x"`, domain.KindValidation},
		{`"2"`, domain.KindPreconditionFailed},
		{`"3"`, ""},
		{`W/"3"`, ""},
		{`3`, ""},
	}
	for _, c := range cases {
		r := httptest.NewRequestWithContext(t.Context(), "PATCH", "/", nil)
		if c.hdr != "" {
			r.Header.Set("If-Match", c.hdr)
		}
		if got := domain.KindOf(middleware.CheckIfMatch(r, 3)); got != c.want {
			t.Errorf("If-Match %q: %q, want %q", c.hdr, got, c.want)
		}
	}
	if v, present, err := middleware.IfMatch(httptest.NewRequestWithContext(t.Context(), "PUT", "/", nil)); v != 0 || present || err != nil {
		t.Fatal("absent header")
	}
	rec := httptest.NewRecorder()
	middleware.SetETag(rec, 9)
	if rec.Header().Get("ETag") != `"9"` {
		t.Fatal("SetETag")
	}
}

// RA-US-15.1 end to end through the router: per principal, public routes per source address.
func TestRateLimitThroughTheRouter(t *testing.T) {
	e := newEnv(t)
	ok, throttled := 0, 0
	var retry string
	for i := 0; i < 60; i++ {
		rec := e.do(t, "GET", "/v1/teams/"+teamA, "admin-a", "")
		switch rec.Code {
		case 200:
			ok++
		case 429:
			throttled++
			retry = rec.Header().Get("Retry-After")
			if envelope(t, rec).Code != domain.KindThrottled || rec.Header().Get("X-Afs-Contract-Version") == "" {
				t.Fatalf("429 body: %s", rec.Body)
			}
		}
	}
	if ok != 40 || throttled != 20 || retry != "1" {
		t.Fatalf("ok=%d throttled=%d Retry-After=%q", ok, throttled, retry)
	}
	if r := e.do(t, "GET", "/v1/teams/"+teamA, "admin-a2", ""); r.Code != 200 {
		t.Fatalf("another principal: %d", r.Code)
	}
	// Public route: limited per source address (shared limiter, so this address is untouched so far).
	pub, pubThrottled := 0, 0
	for i := 0; i < 60; i++ {
		switch e.do(t, "GET", "/v1/identity-providers", "", "").Code {
		case 200:
			pub++
		case 429:
			pubThrottled++
		}
	}
	// 198.51.100.1 also failed no authentication so far; the public bucket is its own key space.
	if pub != 40 || pubThrottled != 20 {
		t.Fatalf("public: %d ok, %d throttled", pub, pubThrottled)
	}
	// Failed authentication on an authenticated route shares the source-address bucket (now empty).
	if r := e.do(t, "GET", "/v1/teams/"+teamA, "bad-token", ""); r.Code != 429 {
		t.Fatalf("failed auth from an exhausted address: %d", r.Code)
	}
}

func TestPostAuthStagesLeaveRefusalsIntact(t *testing.T) {
	e := newEnv(t)
	if r := e.do(t, "GET", "/v1/teams/"+teamA, "", ""); r.Code != 401 || r.Header().Get("X-Request-Id") == "" {
		t.Fatalf("401: %d", r.Code)
	}
}

func slogTo(b *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

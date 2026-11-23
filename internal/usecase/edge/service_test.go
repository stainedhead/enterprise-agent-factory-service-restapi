package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
)

type fakeRoutes struct {
	routes []collab.Route
	set    collab.Settings
	err    error
	serr   error
}

func (f *fakeRoutes) Routes(context.Context) ([]collab.Route, error)    { return f.routes, f.err }
func (f *fakeRoutes) Settings(context.Context) (collab.Settings, error) { return f.set, f.serr }

type fakePrincipals struct {
	people map[string][]Binding
	keys   map[[32]byte]KeyInfo
	err    error
	calls  int
}

func (f *fakePrincipals) Person(_ context.Context, _, sub, _ string, _ []string) ([]Binding, error) {
	f.calls++
	return f.people[sub], f.err
}

func (f *fakePrincipals) Key(_ context.Context, _ string, h [32]byte) (KeyInfo, bool, error) {
	k, ok := f.keys[h]
	return k, ok, f.err
}

type fakeSessions struct {
	sessions map[[32]byte]Session
	handoffs map[[32]byte]Session
	revoked  [][32]byte
	err      error
	gets     int
}

func (f *fakeSessions) HandoffCreate(_ context.Context, h [32]byte, team, host, iss, sub, prov string, groups []string) error {
	if f.err != nil {
		return f.err
	}
	f.handoffs[h] = Session{TeamID: team, Issuer: iss, Subject: sub, ProviderID: prov, Groups: groups, ExpiresAt: time.Unix(0, 0).Add(host2int(host))}
	return nil
}
func host2int(string) time.Duration { return 0 }

func (f *fakeSessions) HandoffConsume(_ context.Context, h [32]byte, _ string, sh [32]byte) (string, time.Time, bool, error) {
	if f.err != nil {
		return "", time.Time{}, false, f.err
	}
	s, ok := f.handoffs[h]
	if !ok {
		return "", time.Time{}, false, nil
	}
	delete(f.handoffs, h)
	exp := t0.Add(8 * time.Hour)
	s.ExpiresAt = exp
	f.sessions[sh] = s
	return s.TeamID, exp, true, nil
}

func (f *fakeSessions) Get(_ context.Context, h [32]byte) (Session, bool, error) {
	f.gets++
	if f.err != nil {
		return Session{}, false, f.err
	}
	s, ok := f.sessions[h]
	return s, ok, nil
}

func (f *fakeSessions) Revoke(_ context.Context, h [32]byte) error {
	f.revoked = append(f.revoked, h)
	delete(f.sessions, h)
	return nil
}

type fakeIdentity struct {
	claims  Claims
	err     error
	authErr error
	last    string
	nonce   string
	prompt  string
}

func (f *fakeIdentity) AuthURL(_ context.Context, _ collab.Route, redirect, state, challenge, nonce, prompt string) (string, error) {
	f.prompt = prompt
	f.nonce = nonce
	if f.authErr != nil {
		return "", f.authErr
	}
	return "https://idp.example/auth?redirect_uri=" + url.QueryEscape(redirect) + "&state=" + url.QueryEscape(state) + "&code_challenge=" + challenge, nil
}

func (f *fakeIdentity) Exchange(_ context.Context, _ collab.Route, _, code, _ string) (Claims, error) {
	f.last = code
	c := f.claims
	if c.Nonce == "" {
		c.Nonce = f.nonce
	}
	return c, f.err
}

func (f *fakeIdentity) VerifyBearer(context.Context, collab.Route, string) (Claims, error) {
	return f.claims, f.err
}

type plainSealer struct{ failSeal, failOpen bool }

func (p plainSealer) Seal(b []byte) (string, error) {
	if p.failSeal {
		return "", errors.New("seal")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (p plainSealer) Open(s string) ([]byte, error) {
	if p.failOpen {
		return nil, errors.New("open")
	}
	return base64.RawURLEncoding.DecodeString(s)
}

type sink struct {
	rows []collab.Row
	err  error
}

func (s *sink) Record(_ context.Context, r []collab.Row) error {
	if s.err != nil {
		return s.err
	}
	s.rows = append(s.rows, r...)
	return nil
}

type alert struct {
	team, pattern, action string
	count                 int
}
type alerts struct{ got []alert }

func (a *alerts) SecretDetected(_ context.Context, team string, _ collab.Surface, _ collab.PrincipalKind, _, pattern, action string, count int) error {
	a.got = append(a.got, alert{team, pattern, action, count})
	return nil
}

type records struct{ got []Event }

func (r *records) Write(e Event) { r.got = append(r.got, e) }

var t0 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

type rig struct {
	svc  *Service
	rt   *fakeRoutes
	pr   *fakePrincipals
	ses  *fakeSessions
	id   *fakeIdentity
	sink *sink
	al   *alerts
	rec  *records
	now  time.Time
	// lastSigninCookie is the binding cookie of the latest signIn2.
	lastSigninCookie string
}

func (r *rig) clock() time.Time { return r.now }

func teamRoute() collab.Route {
	return collab.Route{TeamID: "team_01", Slug: "alpha", Namespace: "team-alpha", Hostname: "alpha.collab.example.com", TeamState: "active",
		WebUIEnabled: true, RESTAPIEnabled: true, WebUIEffective: true, RESTAPIEffective: true, MayExpose: true, Ready: true,
		ServiceName: "paperclip", Port: 3100, ProviderID: "idp_1", IssuerURL: "https://idp.example", ClientID: "cid"}
}

func newRig(t *testing.T, mut func(*collab.Route)) *rig {
	t.Helper()
	r := &rig{now: t0}
	route := teamRoute()
	if mut != nil {
		mut(&route)
	}
	set := collab.DefaultSettings()
	set.PublicBaseDomain = "collab.example.com"
	r.rt = &fakeRoutes{routes: []collab.Route{route}, set: set}
	r.pr = &fakePrincipals{people: map[string][]Binding{"alice": {{Role: "team_admin"}}, "viewer": {{Role: "team_viewer"}},
		"collab": {{Role: "team_collaborator"}}, "root": {{Role: "service_admin"}}, "nobody": nil}, keys: map[[32]byte]KeyInfo{}}
	r.ses = &fakeSessions{sessions: map[[32]byte]Session{}, handoffs: map[[32]byte]Session{}}
	r.id = &fakeIdentity{claims: Claims{Issuer: "https://idp.example", Subject: "alice"}}
	r.sink, r.al, r.rec = &sink{}, &alerts{}, &records{}
	tbl := collab.NewTable(r.clock)
	r.svc = &Service{Table: tbl, Routes: r.rt, Principals: r.pr, Sessions: r.ses, Identity: r.id, Sealer: plainSealer{},
		Access: collab.NewAccessBuffer(), Sink: r.sink, Alerts: r.al, Records: r.rec, Limiter: collab.NewLimiter(20, 40, r.clock),
		Now: r.clock, Rand: bytes.NewReader(bytes.Repeat([]byte{7, 9, 1, 3}, 4096))}
	if err := r.svc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *rig) session(sub string) string {
	raw := bytes.Repeat([]byte{byte(len(sub))}, 32) //nolint:gosec // short test subjects
	r.ses.sessions[sha256.Sum256(raw)] = Session{TeamID: "team_01", Issuer: "i", Subject: sub, ExpiresAt: t0.Add(time.Hour)}
	return base64.RawURLEncoding.EncodeToString(raw)
}

const host = "alpha.collab.example.com"

func get(path string) Request {
	return Request{Method: "GET", Host: host, Path: path, RequestID: "req_1"}
}

func TestRefreshAndFlush(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	r.rt.serr = errors.New("x")
	if r.svc.Refresh(ctx) == nil {
		t.Fatal("settings error expected")
	}
	r.rt.serr, r.rt.err = nil, errors.New("y")
	if r.svc.Refresh(ctx) == nil {
		t.Fatal("routes error expected")
	}
	r.rt.err = nil
	r.svc.Handle(ctx, get("/x"))
	r.sink.err = errors.New("db")
	if r.svc.Flush(ctx) == nil || r.svc.Access.Len() == 0 {
		t.Fatal("failed flush must keep rows")
	}
	r.sink.err = nil
	if err := r.svc.Flush(ctx); err != nil || len(r.sink.rows) == 0 || r.svc.Access.Len() != 0 {
		t.Fatalf("flush: %v %v", err, r.sink.rows)
	}
	if err := r.svc.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHostHandling(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	cases := []struct {
		name   string
		req    Request
		status int
	}{
		{"unknown host", Request{Method: "GET", Host: "evil.example", Path: "/"}, 404},
		{"apex other path", Request{Method: "GET", Host: "collab.example.com", Path: "/"}, 404},
		{"apex callback without state", Request{Method: "GET", Host: "collab.example.com", Path: PathCallback}, 400},
		{"reserved unknown", get("/_afs/other"), 404},
	}
	for _, c := range cases {
		if got := r.svc.Handle(ctx, c.req); got.Status != c.status || got.Action != Respond {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	r.now = r.now.Add(collab.MaxStale + time.Second)
	if got := r.svc.Handle(ctx, get("/")); got.Status != 503 || got.RetryAfter != 30 || got.Reason != "upstream_unavailable" {
		t.Errorf("stale: %+v", got)
	}
}

func TestBothSurfacesOff(t *testing.T) {
	r := newRig(t, func(rt *collab.Route) { rt.WebUIEffective, rt.RESTAPIEffective = false, false })
	if got := r.svc.Handle(context.Background(), get("/")); got.Status != 404 || got.Reason != "" {
		t.Fatalf("%+v", got)
	}
}

func TestCredentialClasses(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	ck := r.session("alice")

	ambiguous := get("/x")
	ambiguous.Cookie, ambiguous.XAfsAuthorization = ck, "Bearer t"
	if got := r.svc.Handle(ctx, ambiguous); got.Status != 400 || got.Reason != "ambiguous_credentials" {
		t.Errorf("ambiguous: %+v", got)
	}
	web := get("/x")
	web.Cookie = ck
	if got := r.svc.Handle(ctx, web); got.Action != Forward || got.Upstream != "http://paperclip.team-alpha.svc.cluster.local:3100" || got.Class != collab.ClassWeb {
		t.Errorf("web: %+v", got)
	}
	nav := get("/board?x=1")
	nav.Navigate = true
	got := r.svc.Handle(ctx, nav)
	if got.Action != Redirect || got.Status != 302 || !strings.HasPrefix(got.Location, PathSignin+"?return=") || got.SetCookie {
		t.Errorf("nav: %+v", got)
	}
	if got := r.svc.Handle(ctx, get("/api/x")); got.Status != 401 || got.Reason != "token_invalid" {
		t.Errorf("api no cred: %+v", got)
	}
	badCookie := get("/x")
	badCookie.Cookie = "!!"
	badCookie.Navigate = true
	if got := r.svc.Handle(ctx, badCookie); got.Action != Redirect {
		t.Errorf("bad cookie nav: %+v", got)
	}
	badCookie.Navigate = false
	if got := r.svc.Handle(ctx, badCookie); got.Status != 401 || got.Reason != "session_expired" {
		t.Errorf("bad cookie api: %+v", got)
	}
	unknownSession := get("/x")
	unknownSession.Cookie = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	if got := r.svc.Handle(ctx, unknownSession); got.Status != 401 {
		t.Errorf("unknown session: %+v", got)
	}
	mal := get("/x")
	mal.XAfsAuthorization = "Basic abc"
	if got := r.svc.Handle(ctx, mal); got.Status != 400 || got.Reason != "malformed_credential" {
		t.Errorf("malformed: %+v", got)
	}
}

func TestAPIKeys(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	h := func(k string) [32]byte { return sha256.Sum256([]byte(k)) }
	r.pr.keys[h("afsk_ok")] = KeyInfo{ID: "key_1", Role: "team_admin", State: "active"}
	r.pr.keys[h("afsk_rev")] = KeyInfo{ID: "key_2", Role: "team_admin", State: "revoked"}
	r.pr.keys[h("afsk_exp")] = KeyInfo{ID: "key_3", Role: "team_admin", State: "expired"}
	for tok, want := range map[string]struct {
		status int
		reason string
	}{"afsk_ok": {200, ""}, "afsk_rev": {401, "key_revoked"}, "afsk_exp": {401, "key_expired"}, "afsk_none": {401, "token_invalid"}} {
		req := get("/api")
		req.XAfsAuthorization = "Bearer " + tok
		got := r.svc.Handle(ctx, req)
		if tok == "afsk_ok" {
			if got.Action != Forward || got.Kind != collab.KindAPIKey || got.Principal != "key_1" {
				t.Errorf("%s: %+v", tok, got)
			}
			continue
		}
		if got.Status != want.status || got.Reason != want.reason {
			t.Errorf("%s: %+v", tok, got)
		}
	}
	r.pr.err = errors.New("db")
	r.svc.keys = newLRU[string, cached[KeyInfo]](10)
	req := get("/api")
	req.XAfsAuthorization = "Bearer afsk_ok"
	if got := r.svc.Handle(ctx, req); got.Status != 401 {
		t.Errorf("key lookup error must fail closed: %+v", got)
	}
}

func TestBearerTokens(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	req := get("/api")
	req.XAfsAuthorization = "bearer jwt"
	if got := r.svc.Handle(ctx, req); got.Action != Forward || got.Principal != "alice" || got.Kind != collab.KindUser {
		t.Errorf("ok: %+v", got)
	}
	r.id.err = &TokenError{Expired: true}
	if got := r.svc.Handle(ctx, req); got.Reason != "token_expired" {
		t.Errorf("expired: %+v", got)
	}
	r.id.err = &TokenError{}
	if got := r.svc.Handle(ctx, req); got.Reason != "token_invalid" {
		t.Errorf("invalid: %+v", got)
	}
	r.id.err = errors.New("network")
	if got := r.svc.Handle(ctx, req); got.Reason != "token_invalid" {
		t.Errorf("other: %+v", got)
	}
}

func TestDecisions(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, sub, method string
		mut               func(*collab.Route)
		status            int
		reason, required  string
	}{
		{"admin read", "alice", "GET", nil, 200, "", ""},
		{"admin write", "alice", "POST", nil, 200, "", ""},
		{"viewer read", "viewer", "GET", nil, 200, "", ""},
		{"viewer write", "viewer", "POST", nil, 403, "permission_required", "collaboration:contribute"},
		{"collaborator web", "collab", "GET", nil, 200, "", ""},
		{"no role", "nobody", "GET", nil, 403, "no_team_role", ""},
		{"not ready", "alice", "GET", func(r *collab.Route) { r.Ready, r.NotReadyReason = false, "starting" }, 503, "starting", ""},
		{"service admin", "root", "GET", nil, 200, "", ""},
	} {
		r := newRig(t, c.mut)
		req := get("/x")
		req.Method = c.method
		req.Origin = "https://" + host
		req.Cookie = r.session(c.sub)
		got := r.svc.Handle(ctx, req)
		if c.status == 200 {
			if got.Action != Forward {
				t.Errorf("%s: %+v", c.name, got)
			}
			continue
		}
		if got.Status != c.status || got.Reason != c.reason || got.Required != c.required {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

func TestServiceAdminEntryFlag(t *testing.T) {
	r := newRig(t, nil)
	req := get("/api")
	req.XAfsAuthorization = "Bearer jwt"
	r.id.claims.Subject = "root"
	got := r.svc.Handle(context.Background(), req)
	if got.Action != Forward {
		t.Fatalf("%+v", got)
	}
	// a service admin with no team binding gets in through service_admin alone
	if !got.ServiceAdminEntry {
		t.Errorf("service-admin entry must be flagged: %+v", got)
	}
	r.id.claims.Subject = "alice"
	if got := r.svc.Handle(context.Background(), req); got.ServiceAdminEntry {
		t.Errorf("team admin is not a service-admin entry")
	}
}

func TestSurfaceRefused(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, func(rt *collab.Route) { rt.RESTAPIEffective = false })
	api := get("/api")
	api.XAfsAuthorization = "Bearer jwt"
	if got := r.svc.Handle(ctx, api); got.Status != 403 || got.Reason != "surface_disabled" {
		t.Errorf("bound caller: %+v", got)
	}
	r.id.claims.Subject = "nobody"
	if got := r.svc.Handle(ctx, api); got.Status != 404 || got.Reason != "" {
		t.Errorf("unbound caller: %+v", got)
	}
	r.id.err = &TokenError{}
	if got := r.svc.Handle(ctx, api); got.Status != 404 {
		t.Errorf("bad token: %+v", got)
	}
}

func TestPrincipalLookupFailure(t *testing.T) {
	r := newRig(t, nil)
	r.pr.err = errors.New("db")
	req := get("/api")
	req.XAfsAuthorization = "Bearer jwt"
	if got := r.svc.Handle(context.Background(), req); got.Status != 503 || got.Reason != "upstream_unavailable" {
		t.Errorf("%+v", got)
	}
}

func TestCaching(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	req := get("/api")
	req.XAfsAuthorization = "Bearer jwt"
	r.svc.Handle(ctx, req)
	r.svc.Handle(ctx, req)
	if r.pr.calls != 1 {
		t.Fatalf("positive decision must be cached, calls=%d", r.pr.calls)
	}
	r.now = r.now.Add(DecisionTTL + time.Second)
	r.svc.Table.Replace(r.rt.routes, r.rt.set)
	r.svc.Handle(ctx, req)
	if r.pr.calls != 2 {
		t.Fatalf("cache must expire after %v, calls=%d", DecisionTTL, r.pr.calls)
	}
	r.id.claims.Subject = "nobody"
	r.svc.Handle(ctx, req)
	r.svc.Handle(ctx, req)
	n := r.pr.calls
	r.now = r.now.Add(NegativeTTL + time.Second)
	r.svc.Table.Replace(r.rt.routes, r.rt.set)
	r.svc.Handle(ctx, req)
	if r.pr.calls != n+1 {
		t.Fatalf("negative result must expire after %v", NegativeTTL)
	}
	// session cache
	ck := r.session("alice")
	web := get("/x")
	web.Cookie = ck
	r.svc.Handle(ctx, web)
	r.svc.Handle(ctx, web)
	if r.ses.gets != 1 {
		t.Errorf("session cache: %d", r.ses.gets)
	}
	r.ses.err = errors.New("db")
	r.now = r.now.Add(DecisionTTL + time.Second)
	r.svc.Table.Replace(r.rt.routes, r.rt.set)
	if got := r.svc.Handle(ctx, web); got.Status != 401 {
		t.Errorf("session store error fails closed: %+v", got)
	}
}

func TestThrottleAndBodyLimits(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	req := get("/api")
	req.XAfsAuthorization = "Bearer jwt"
	throttled := 0
	for i := 0; i < 60; i++ {
		if got := r.svc.Handle(ctx, req); got.Status == 429 {
			throttled++
			if got.RetryAfter < 1 || got.Code != "Throttled" {
				t.Fatalf("%+v", got)
			}
		}
	}
	if throttled == 0 {
		t.Fatal("limiter never tripped")
	}

	r = newRig(t, nil)
	post := get("/api")
	post.Method = "POST"
	post.XAfsAuthorization = "Bearer jwt"
	post.ContentType = "application/json"
	post.ContentLength = 26 << 20
	if got := r.svc.Handle(ctx, post); got.Status != 400 || got.Reason != "body_too_large" {
		t.Errorf("large: %+v", got)
	}
	post.ContentLength = 10
	post.ContentEncoding = "gzip"
	if got := r.svc.Handle(ctx, post); got.Status != 400 || got.Reason != "unsupported_encoding" {
		t.Errorf("gzip: %+v", got)
	}
	post.ContentEncoding = "identity"
	got := r.svc.Handle(ctx, post)
	if got.Action != Forward || !got.ScanBody || got.ScanMode != "block" || got.MaxBody != 25<<20 {
		t.Errorf("scan: %+v", got)
	}
	post.ContentType = "image/png"
	if got := r.svc.Handle(ctx, post); got.ScanBody {
		t.Errorf("binary bodies are not scanned")
	}
	r.rt.set.ContentScan = "off"
	_ = r.svc.Refresh(ctx)
	post.ContentType, post.ContentEncoding = "application/json", "gzip"
	if got := r.svc.Handle(ctx, post); got.Action != Forward || got.ScanBody {
		t.Errorf("scan off: %+v", got)
	}
	viewer := get("/api")
	viewer.Method = "GET"
	viewer.XAfsAuthorization = "Bearer jwt"
	if got := r.svc.Handle(ctx, viewer); got.ScanBody {
		t.Errorf("GET not scanned")
	}
}

func TestOnScan(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	res := Result{Route: teamRoute(), Class: collab.ClassAPI, Kind: collab.KindUser, Principal: "alice", ScanBody: true, ScanMode: "block"}
	if _, blocked := r.svc.OnScan(ctx, res, nil); blocked {
		t.Fatal("no matches, no block")
	}
	m := []collab.Match{{Pattern: "aws_access_key", Count: 2}}
	out, blocked := r.svc.OnScan(ctx, res, m)
	if !blocked || out.Status != 422 || out.Code != "SecretDetected" || len(out.Patterns) != 1 || out.Message == "" {
		t.Fatalf("%+v", out)
	}
	if len(r.al.got) != 1 || r.al.got[0].action != "blocked" || r.al.got[0].count != 2 {
		t.Fatalf("alerts: %+v", r.al.got)
	}
	// within five minutes: counted, not sent
	r.svc.OnScan(ctx, res, m)
	if len(r.al.got) != 1 {
		t.Fatalf("dedup: %+v", r.al.got)
	}
	r.now = r.now.Add(SecretAlertEvery + time.Second)
	r.svc.OnScan(ctx, res, m)
	if len(r.al.got) != 2 || r.al.got[1].count != 4 {
		t.Fatalf("accumulated count: %+v", r.al.got)
	}
	res.ScanMode = "warn"
	res.Principal = "bob"
	if out, blocked := r.svc.OnScan(ctx, res, m); blocked || out.Status == 422 {
		t.Fatalf("warn must not block: %+v", out)
	}
	if r.al.got[len(r.al.got)-1].action != "warned" {
		t.Fatalf("%+v", r.al.got)
	}
	res.Principal = strings.Repeat("p", 80)
	r.svc.OnScan(ctx, res, m)
}

func TestFrameAllowed(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	for sub, want := range map[string]bool{"alice": true, "viewer": false} {
		req := get("/ws")
		req.Upgrade = true
		req.Origin = "https://" + host
		req.Cookie = r.session(sub)
		res := r.svc.Handle(ctx, req)
		if res.Action != Forward || r.svc.FrameAllowed(res) != want || (want && res.ScanMode != "block") || res.ScanBody {
			t.Errorf("%s: %+v", sub, res)
		}
	}
}

func TestRouteTemplate(t *testing.T) {
	for in, want := range map[string]string{"/": "/", "": "/", "/api/issues/42": "/api", "/board": "/board"} {
		if got := RouteTemplate(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestSafeReturn(t *testing.T) {
	for _, c := range []struct{ path, q, want string }{
		{"/board", "a=1", "/board?a=1"}, {"/board", "", "/board"}, {"", "", "/"}, {"//evil.example", "", "/"},
		{"https://evil.example", "", "/"}, {"/_afs/signin", "", "/"}, {"/a\r\nb", "", "/"}, {"/\\evil", "", "/"},
	} {
		if got := safeReturn(c.path, c.q); got != c.want {
			t.Errorf("%q %q: %q", c.path, c.q, got)
		}
	}
	if returnTarget("/x?y=1") != "/x?y=1" || returnTarget("//x") != "/" {
		t.Error("returnTarget")
	}
}

// signIn drives the full flow and returns the team host's cookie.
func signIn(t *testing.T, r *rig, ret string) (Result, Result, Result) {
	t.Helper()
	ctx := context.Background()
	si := get(PathSignin)
	si.Query = url.Values{"return": {ret}}.Encode()
	s1 := r.svc.Handle(ctx, si)
	if s1.Action != Redirect || !strings.HasPrefix(s1.Location, "https://idp.example/auth") {
		t.Fatalf("signin: %+v", s1)
	}
	u, _ := url.Parse(s1.Location)
	if got := u.Query().Get("redirect_uri"); got != "https://collab.example.com/oauth2/callback" {
		t.Fatalf("redirect uri %q", got)
	}
	cb := Request{Method: "GET", Host: "collab.example.com", Path: PathCallback,
		Query: url.Values{"state": {u.Query().Get("state")}, "code": {"authcode"}}.Encode()}
	s2 := r.svc.Handle(ctx, cb)
	if s2.Action != Redirect || !strings.HasPrefix(s2.Location, "https://"+host+PathHandoff+"?") {
		t.Fatalf("callback: %+v", s2)
	}
	hu, _ := url.Parse(s2.Location)
	ho := get(PathHandoff)
	ho.Query = hu.RawQuery
	ho.SigninCookie = s1.SigninCookie
	s3 := r.svc.Handle(ctx, ho)
	return s1, s2, s3
}

// envReq is a handoff request for a database code, sealed with the sign-in cookie "ck".
func envReq(code string) Request {
	env, _ := json.Marshal(handoffEnvelope{Code: code, Bind: b64(hashSlice("ck")), Expires: t0.Add(48 * time.Hour).Unix()})
	sealed, _ := plainSealer{}.Seal(env)
	q := get(PathHandoff)
	q.Query = url.Values{"code": {sealed}}.Encode()
	q.SigninCookie = "ck"
	return q
}

func TestSignInFlow(t *testing.T) {
	r := newRig(t, nil)
	_, _, s3 := signIn(t, r, "/board?tab=2")
	if s3.Action != Redirect || !s3.SetCookie || s3.Location != "/board?tab=2" || s3.CookieMaxAge != int((8*time.Hour).Seconds()) || s3.Cookie == "" {
		t.Fatalf("handoff: %+v", s3)
	}
	if s3.Outcome != collab.OutcomeSignedIn {
		t.Errorf("outcome %q", s3.Outcome)
	}
	// the cookie now works, and the code cannot be replayed
	req := get("/board")
	req.Cookie = s3.Cookie
	if got := r.svc.Handle(context.Background(), req); got.Action != Forward || got.Principal != "alice" {
		t.Fatalf("session use: %+v", got)
	}
	foundSignIn := false
	for _, e := range r.rec.got {
		if e.Event == "sign_in" {
			foundSignIn = true
		}
	}
	if !foundSignIn {
		t.Error("sign_in event missing")
	}
	// sign out
	so := get(PathSignout)
	so.Method, so.Cookie, so.Origin = "POST", s3.Cookie, "https://"+host
	out := r.svc.Handle(context.Background(), so)
	if !out.ClearCookie || out.Location != "/" || len(r.ses.revoked) != 1 {
		t.Fatalf("signout: %+v", out)
	}
	if got := r.svc.Handle(context.Background(), req); got.Status != 401 && got.Action != Redirect {
		t.Fatalf("revoked session: %+v", got)
	}
	so.Cookie = ""
	if out := r.svc.Handle(context.Background(), so); !out.ClearCookie {
		t.Fatalf("signout without cookie: %+v", out)
	}
}

func TestSignInOpenRedirectGuard(t *testing.T) {
	r := newRig(t, nil)
	_, _, s3 := signIn(t, r, "//evil.example/x")
	if s3.Location != "/" {
		t.Fatalf("%q", s3.Location)
	}
}

func TestSignInFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("surface off", func(t *testing.T) {
		r := newRig(t, func(rt *collab.Route) { rt.WebUIEffective = false })
		if got := r.svc.Handle(ctx, get(PathSignin)); got.Status != 404 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("seal", func(t *testing.T) {
		r := newRig(t, nil)
		r.svc.Sealer = plainSealer{failSeal: true}
		if got := r.svc.Handle(ctx, get(PathSignin)); got.Status != 503 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("authurl", func(t *testing.T) {
		r := newRig(t, nil)
		r.id.authErr = errors.New("discovery")
		if got := r.svc.Handle(ctx, get(PathSignin)); got.Status != 503 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("random", func(t *testing.T) {
		r := newRig(t, nil)
		r.svc.Rand = bytes.NewReader(nil)
		if got := r.svc.Handle(ctx, get(PathSignin)); got.Status != 503 {
			t.Fatalf("%+v", got)
		}
		r.svc.Rand = bytes.NewReader(make([]byte, 32))
		if got := r.svc.Handle(ctx, get(PathSignin)); got.Status != 503 {
			t.Fatalf("second read: %+v", got)
		}
	})
	cb := func(state string, extra url.Values) Request {
		q := url.Values{"state": {state}}
		for k, v := range extra {
			q[k] = v
		}
		return Request{Method: "GET", Host: "collab.example.com", Path: PathCallback, Query: q.Encode()}
	}
	valid := func(r *rig) string {
		si := get(PathSignin)
		s1 := r.svc.Handle(ctx, si)
		u, _ := url.Parse(s1.Location)
		return u.Query().Get("state")
	}
	t.Run("bad state", func(t *testing.T) {
		r := newRig(t, nil)
		if got := r.svc.Handle(ctx, cb("%%%", nil)); got.Status != 400 {
			t.Fatalf("%+v", got)
		}
		if got := r.svc.Handle(ctx, cb(base64.RawURLEncoding.EncodeToString([]byte("not json")), nil)); got.Status != 401 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("expired state", func(t *testing.T) {
		r := newRig(t, nil)
		st := valid(r)
		r.now = r.now.Add(StateTTL + time.Minute)
		r.svc.Table.Replace(r.rt.routes, r.rt.set)
		if got := r.svc.Handle(ctx, cb(st, url.Values{"code": {"c"}})); got.Status != 401 || got.Reason != "session_expired" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("idp error", func(t *testing.T) {
		r := newRig(t, nil)
		if got := r.svc.Handle(ctx, cb(valid(r), url.Values{"error": {"access_denied"}})); got.Status != 401 {
			t.Fatalf("%+v", got)
		}
		if got := r.svc.Handle(ctx, cb(valid(r), nil)); got.Status != 401 {
			t.Fatalf("no code: %+v", got)
		}
	})
	t.Run("exchange error and nonce", func(t *testing.T) {
		r := newRig(t, nil)
		st := valid(r)
		r.id.err = errors.New("bad")
		if got := r.svc.Handle(ctx, cb(st, url.Values{"code": {"c"}})); got.Status != 401 {
			t.Fatalf("%+v", got)
		}
		r.id.err = nil
		r.id.claims.Nonce = "other"
		if got := r.svc.Handle(ctx, cb(st, url.Values{"code": {"c"}})); got.Status != 401 {
			t.Fatalf("nonce: %+v", got)
		}
	})
	t.Run("unknown team and host mismatch", func(t *testing.T) {
		r := newRig(t, nil)
		st := valid(r)
		r.rt.routes[0].Hostname = "moved.collab.example.com"
		_ = r.svc.Refresh(ctx)
		if got := r.svc.Handle(ctx, cb(st, url.Values{"code": {"c"}})); got.Status != 404 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("handoff create fails", func(t *testing.T) {
		r := newRig(t, nil)
		st := valid(r)
		r.ses.err = errors.New("db")
		if got := r.svc.Handle(ctx, cb(st, url.Values{"code": {"c"}})); got.Status != 503 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("callback random fails", func(t *testing.T) {
		r := newRig(t, nil)
		st := valid(r)
		r.svc.Rand = bytes.NewReader(nil)
		if got := r.svc.Handle(ctx, cb(st, url.Values{"code": {"c"}})); got.Status != 503 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("handoff", func(t *testing.T) {
		r := newRig(t, nil)
		ho := get(PathHandoff)
		if got := r.svc.Handle(ctx, ho); got.Status != 401 {
			t.Fatalf("no code: %+v", got)
		}
		ho.Query = "code=unknown"
		if got := r.svc.Handle(ctx, ho); got.Status != 401 {
			t.Fatalf("unknown: %+v", got)
		}
		ho = envReq("unknown")
		r.ses.err = errors.New("db")
		if got := r.svc.Handle(ctx, ho); got.Status != 503 {
			t.Fatalf("store: %+v", got)
		}
		r.ses.err = nil
		r.svc.Rand = bytes.NewReader(nil)
		if got := r.svc.Handle(ctx, ho); got.Status != 503 {
			t.Fatalf("rand: %+v", got)
		}
	})
	t.Run("handoff for another team", func(t *testing.T) {
		r := newRig(t, nil)
		code := "abc"
		r.ses.handoffs[sha256.Sum256([]byte(code))] = Session{TeamID: "team_other"}
		ho := envReq(code)
		if got := r.svc.Handle(ctx, ho); got.Status != 401 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("handoff expiring now", func(t *testing.T) {
		r := newRig(t, nil)
		code := "abc"
		r.ses.handoffs[sha256.Sum256([]byte(code))] = Session{TeamID: "team_01"}
		r.now = t0.Add(9 * time.Hour)
		r.svc.Table.Replace(r.rt.routes, r.rt.set)
		ho := envReq(code)
		if got := r.svc.Handle(ctx, ho); got.Status != 401 || got.Reason != "session_expired" {
			t.Fatalf("%+v", got)
		}
	})
}

func TestRandomAndRecords(t *testing.T) {
	r := newRig(t, nil)
	r.svc.Records = nil
	r.svc.Handle(context.Background(), get("/x"))
	long := strings.Repeat("a", 100)
	r.svc.record(Result{Route: teamRoute(), Kind: collab.KindUser, Principal: long}, "allowed", 1)
	r.svc.record(Result{}, "allowed", 1)
	if (&TokenError{Expired: true}).Error() == (&TokenError{}).Error() {
		t.Error("token error text")
	}
	r2 := newRig(t, nil)
	r2.svc.Handle(context.Background(), get("/x"))
	if len(r2.rec.got) == 0 || r2.rec.got[0].Route != "/x" || r2.rec.got[0].PrincipalKind != "anonymous" {
		t.Errorf("%+v", r2.rec.got)
	}
}

func TestRun(t *testing.T) {
	r := newRig(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	changes := make(chan struct{}, 4)
	var mu sync.Mutex
	var errs []string
	done := make(chan struct{})
	r.rt.err = errors.New("db down")
	r.sink.err = errors.New("sink down")
	r.svc.Handle(context.Background(), get("/x"))
	go func() {
		r.svc.Run(ctx, RunConfig{RefreshEvery: 5 * time.Millisecond, FlushEvery: 5 * time.Millisecond, Changes: changes,
			OnError: func(op string, _ error) { mu.Lock(); errs = append(errs, op); mu.Unlock() }})
		close(done)
	}()
	changes <- struct{}{}
	changes <- struct{}{}
	time.Sleep(40 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, e := range errs {
		seen[e] = true
	}
	if !seen["refresh"] || !seen["flush"] {
		t.Fatalf("errors reported: %v", errs)
	}
	// defaults and the final flush
	r2 := newRig(t, nil)
	r2.svc.Handle(context.Background(), get("/x"))
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	r2.svc.Run(ctx2, RunConfig{})
	if len(r2.sink.rows) == 0 {
		t.Error("the final flush must write the counts")
	}
}

func hash2(raw []byte) [32]byte { return sha256.Sum256(raw) }

func TestPrivateEntryAndWrongLevel(t *testing.T) {
	r := newRig(t, nil)
	pub := teamRoute()
	pub.Level, pub.PublicHostname, pub.PrivateHostname = collab.LevelPublic, host, "alpha.internal.example.com"
	pub.WebUIEffective, pub.RESTAPIEffective = false, true // Web UI is private only
	pub.WebUIOtherLevel = true
	priv := pub
	priv.Level, priv.Hostname = collab.LevelPrivate, "alpha.internal.example.com"
	priv.WebUIEffective, priv.RESTAPIEffective, priv.WebUIOtherLevel = true, false, false
	set := collab.DefaultSettings()
	set.PublicBaseDomain, set.PrivateBaseDomain = "collab.example.com", "internal.example.com"
	r.rt.routes, r.rt.set = []collab.Route{pub, priv}, set
	if err := r.svc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// The Web UI answers 404 on the public hostname, even to a caller with a binding.
	req := Request{Method: "GET", Host: host, Path: "/", Cookie: r.session("alice"), Navigate: true, RequestID: "r1", SecFetchSite: "same-origin"}
	if got := r.svc.Handle(ctx, req); got.Status != 404 {
		t.Errorf("web ui on the public host: %+v", got)
	}
	// It is served on the private hostname, and the sign-in goes through the private apex.
	req.Host = "alpha.internal.example.com"
	if got := r.svc.Handle(ctx, req); got.Action != Forward {
		t.Errorf("web ui on the private host: %+v", got)
	}
	if got := r.svc.callbackURL(collab.LevelPrivate); got != "https://internal.example.com"+PathCallback {
		t.Errorf("callback %s", got)
	}
	if got := r.svc.Handle(ctx, Request{Method: "GET", Host: "internal.example.com", Path: "/"}); got.Status != 404 || got.Action != Respond {
		t.Errorf("private apex: %+v", got)
	}
	// The access record carries the level.
	r.svc.record(Result{Route: priv, Class: collab.ClassWeb}, collab.OutcomeAllowed, 0)
	rows := r.svc.Access.Take()
	found := false
	for _, row := range rows {
		found = found || row.Key.Exposure == "private"
	}
	if !found {
		t.Errorf("rows %+v", rows)
	}
}

package edge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
)

// Finding 1: cross-origin writes and upgrades on a cookie session are refused.
func TestCrossOriginRefused(t *testing.T) {
	ctx := context.Background()
	teamA, teamB := "https://"+host, "https://beta.collab.example.com"
	tests := []struct {
		name         string
		method       string
		origin, sfs  string
		upgrade, sse bool
		refused      bool
	}{
		{"POST from another team host", "POST", teamB, "same-site", false, false, true},
		{"POST same origin", "POST", teamA, "same-origin", false, false, false},
		{"POST origin null", "POST", "null", "", false, false, true},
		{"POST http origin", "POST", "http://" + host, "", false, false, true},
		{"POST no origin same-origin fetch metadata", "POST", "", "same-origin", false, false, false},
		{"POST no origin same-site", "POST", "", "same-site", false, false, true},
		{"POST no origin no metadata", "POST", "", "", false, false, true},
		{"PUT cross", "PUT", teamB, "", false, false, true},
		{"DELETE cross", "DELETE", teamB, "", false, false, true},
		{"websocket from another team host", "GET", teamB, "same-site", true, false, true},
		{"websocket without origin", "GET", "", "same-origin", true, false, true},
		{"websocket same origin", "GET", teamA, "same-origin", true, false, false},
		{"event stream cross origin", "GET", teamB, "", false, true, true},
		{"event stream same origin no Origin header", "GET", "", "same-origin", false, true, false},
		{"plain GET cross is a safe read", "GET", teamB, "cross-site", false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil)
			req := get("/api/x")
			req.Method, req.Origin, req.SecFetchSite, req.Upgrade, req.EventStream = tc.method, tc.origin, tc.sfs, tc.upgrade, tc.sse
			req.Cookie = r.session("alice")
			got := r.svc.Handle(ctx, req)
			if tc.refused != (got.Status == 403 && got.Reason == "cross_origin") {
				t.Fatalf("refused=%v got %d %q", tc.refused, got.Status, got.Reason)
			}
			if !tc.refused && got.Action != Forward {
				t.Fatalf("want forward: %+v", got)
			}
		})
	}
}

func TestCrossOriginExemptions(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	// Header credentials are not ambient: no Origin check.
	req := get("/api/x")
	req.Method, req.XAfsAuthorization = "POST", "Bearer jwt"
	if got := r.svc.Handle(ctx, req); got.Action != Forward {
		t.Fatalf("api POST: %+v", got)
	}
	// Sign-out is a cookie write too.
	so := get(PathSignout)
	so.Method, so.Cookie, so.Origin = "POST", r.session("alice"), "https://beta.collab.example.com"
	if got := r.svc.Handle(ctx, so); got.Reason != "cross_origin" || got.ClearCookie {
		t.Fatalf("cross-origin signout: %+v", got)
	}
}

// Finding 2: an open stream is decided again with fresh data.
func TestRecheck(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	req := get("/ws")
	req.Upgrade, req.Origin, req.Cookie = true, "https://"+host, r.session("alice")
	first := r.svc.Handle(ctx, req)
	if first.Action != Forward || !r.svc.FrameAllowed(first) {
		t.Fatalf("%+v", first)
	}
	if got := r.svc.Recheck(ctx, req); got.Action != Forward {
		t.Fatalf("recheck: %+v", got)
	}
	// binding removed: the cache would still allow it for 60 s, a recheck must not
	r.pr.people["alice"] = nil
	if got := r.svc.Handle(ctx, req); got.Action != Forward {
		t.Fatalf("cached decision expected: %+v", got)
	}
	if got := r.svc.Recheck(ctx, req); got.Action != Respond || got.Status != 403 || got.Reason != "no_team_role" {
		t.Fatalf("binding gone: %+v", got)
	}
	r.pr.people["alice"] = []Binding{{Role: "team_viewer"}}
	if got := r.svc.Recheck(ctx, req); got.Action != Forward || r.svc.FrameAllowed(got) {
		t.Fatalf("downgrade is visible through the new result: %+v", got)
	}
	r.pr.people["alice"] = []Binding{{Role: "team_admin"}}
	// session revoked
	r.ses.sessions = map[[32]byte]Session{}
	if got := r.svc.Recheck(ctx, req); got.Status != 401 {
		t.Fatalf("session gone: %+v", got)
	}
	// surface switched off
	req.Cookie = r.session("alice")
	r.rt.routes[0].WebUIEffective = false
	r.svc.Table.Replace(r.rt.routes, r.rt.set)
	if got := r.svc.Recheck(ctx, req); got.Action != Respond {
		t.Fatalf("surface off: %+v", got)
	}
	// a recheck is not counted and not rate limited
	r.svc.Access.Take()
	before := r.svc.Access.Len()
	r.svc.Recheck(ctx, req)
	if r.svc.Access.Len() != before {
		t.Error("recheck must not count")
	}
}

// Finding 4 (use case side): unreadable media types.
func TestUnsupportedMediaType(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		ct        string
		status    int
		scan, bin bool
	}{
		{"application/json", 0, true, false}, {"application/json; charset=utf-16", 0, true, false}, {"", 0, true, false},
		{"multipart/form-data; boundary=x", 0, true, false}, {"application/x-www-form-urlencoded", 0, true, false},
		{"image/png", 0, false, true}, {"application/pdf", 0, false, true}, {"video/mp4", 0, false, true},
		{"application/octet-stream", 415, false, false}, {"application/zip", 415, false, false}, {"application/x-msgpack", 415, false, false},
	} {
		r := newRig(t, nil)
		req := get("/api/x")
		req.Method, req.XAfsAuthorization, req.ContentType, req.ContentLength = "POST", "Bearer jwt", tc.ct, 10
		got := r.svc.Handle(ctx, req)
		if tc.status != 0 {
			if got.Status != 415 || got.Reason != "unsupported_media_type" || got.Outcome != collab.OutcomeDeniedBad {
				t.Errorf("%q: %+v", tc.ct, got)
			}
			continue
		}
		if got.Action != Forward || got.ScanBody != tc.scan {
			t.Errorf("%q: scan=%v %+v", tc.ct, got.ScanBody, got)
		}
	}
	// warn mode and scan off never refuse; an empty body is never refused
	for _, mode := range []string{"warn", "off"} {
		r := newRig(t, nil)
		r.rt.set.ContentScan = mode
		r.svc.Table.Replace(r.rt.routes, r.rt.set)
		req := get("/api/x")
		req.Method, req.XAfsAuthorization, req.ContentType, req.ContentLength = "POST", "Bearer jwt", "application/octet-stream", 10
		if got := r.svc.Handle(ctx, req); got.Action != Forward {
			t.Errorf("%s: %+v", mode, got)
		}
	}
	r := newRig(t, nil)
	req := get("/api/x")
	req.Method, req.XAfsAuthorization, req.ContentType, req.ContentLength = "POST", "Bearer jwt", "application/octet-stream", 0
	if got := r.svc.Handle(ctx, req); got.Action != Forward {
		t.Errorf("empty body: %+v", got)
	}
	got := r.svc.OnUnreadable(Result{Route: r.rt.routes[0], Kind: collab.KindUser, Principal: "alice"})
	if got.Status != 415 {
		t.Errorf("unreadable: %+v", got)
	}
}

// Finding 7: bounded caches and a per-source limit on unknown credentials.
func TestCachesStayBounded(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	r.svc.CacheSize, r.svc.UnknownPerMinute = 50, 1<<30
	for i := range 2000 {
		req := get("/x")
		req.RemoteAddr = fmt.Sprintf("10.0.%d.%d:1", i/250, i%250)
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		req.Cookie = base64.RawURLEncoding.EncodeToString(raw)
		r.svc.Handle(ctx, req)
		k := get("/x")
		k.XAfsAuthorization = "Bearer afsk_" + fmt.Sprint(i)
		k.RemoteAddr = req.RemoteAddr
		r.svc.Handle(ctx, k)
		p := get("/x")
		p.XAfsAuthorization = "Bearer jwt"
		r.id.claims.Subject = fmt.Sprint("s", i)
		r.svc.Handle(ctx, p)
	}
	r.svc.mu.Lock()
	defer r.svc.mu.Unlock()
	for name, n := range map[string]int{"sess": r.svc.sess.len(), "keys": r.svc.keys.len(), "people": r.svc.people.len(), "unknown": r.svc.unknown.len()} {
		if n > 50 {
			t.Errorf("%s cache holds %d entries, bound is 50", name, n)
		}
	}
}

func TestUnknownCredentialsRateLimited(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	r.svc.UnknownPerMinute = 3
	try := func(i int, ip string) Result {
		req := get("/x")
		req.RemoteAddr = ip + ":9"
		req.XAfsAuthorization = fmt.Sprintf("Bearer afsk_%d", i)
		return r.svc.Handle(ctx, req)
	}
	for i := range 3 {
		if got := try(i, "203.0.113.9"); got.Status != 401 {
			t.Fatalf("%d: %+v", i, got)
		}
	}
	lookups := func() int { r.svc.mu.Lock(); defer r.svc.mu.Unlock(); return r.svc.keys.len() }
	n := lookups()
	if got := try(99, "203.0.113.9"); got.Status != 429 || got.Reason != "" {
		t.Fatalf("fourth unknown lookup: %+v", got)
	}
	if lookups() != n {
		t.Error("a throttled source must not reach the store")
	}
	if got := try(100, "203.0.113.10"); got.Status != 401 {
		t.Errorf("another source is unaffected: %+v", got)
	}
	// the window ends
	r.now = r.now.Add(61 * time.Second)
	r.svc.Table.Replace(r.rt.routes, r.rt.set)
	if got := try(101, "203.0.113.9"); got.Status != 401 {
		t.Errorf("after the window: %+v", got)
	}
	// known credentials never count against the limit
	h := hash("afsk_good")
	r.pr.keys[h] = KeyInfo{ID: "key_1", Role: "team_admin", State: "active"}
	for range 10 {
		req := get("/x")
		req.RemoteAddr, req.XAfsAuthorization = "203.0.113.50:1", "Bearer afsk_good"
		r.now = r.now.Add(DecisionTTL + time.Second)
		r.svc.Table.Replace(r.rt.routes, r.rt.set)
		if got := r.svc.Handle(ctx, req); got.Action != Forward {
			t.Fatalf("known key throttled: %+v", got)
		}
	}
	// a blocked web session lookup is 429 too, and navigation does not redirect-loop
	r.svc.UnknownPerMinute = 1
	for i := range 2 {
		req := get("/x")
		req.RemoteAddr, req.Navigate = "198.51.100.1:1", true
		raw := bytes.Repeat([]byte{byte(i + 1)}, 32)
		req.Cookie = base64.RawURLEncoding.EncodeToString(raw)
		got := r.svc.Handle(ctx, req)
		if i == 1 && got.Status != 429 {
			t.Errorf("web lookup: %+v", got)
		}
	}
}

// Finding 8: a handoff link works only in the browser that started the sign-in.
func TestLoginCSRFBinding(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	s1, s2, s3 := signIn(t, r, "/board")
	if !s1.SetSigninCookie || len(s1.SigninCookie) < 40 {
		t.Fatalf("signin must set the binding cookie: %+v", s1)
	}
	if s3.Action != Redirect || !s3.SetCookie || !s3.ClearSigninCookie {
		t.Fatalf("handoff must set the session and delete the signin cookie: %+v", s3)
	}
	hu, _ := url.Parse(s2.Location)
	// same link, another browser: no cookie, or a different one
	r2 := newRig(t, nil)
	_, s2b, _ := signIn(t, r2, "/")
	hub, _ := url.Parse(s2b.Location)
	for name, cookie := range map[string]string{"no cookie": "", "attacker's cookie": "something-else"} {
		r3 := newRig(t, nil)
		_, s2c, _ := func() (Result, Result, Result) { return signIn2(t, r3) }()
		hc, _ := url.Parse(s2c.Location)
		ho := get(PathHandoff)
		ho.Query, ho.SigninCookie = hc.RawQuery, cookie
		got := r3.svc.Handle(ctx, ho)
		if got.Action != Respond || got.Status != 401 || got.SetCookie {
			t.Errorf("%s: %+v", name, got)
		}
		// the failed replay did not burn the victim's code
		ho.SigninCookie = r3.lastSigninCookie
		if got := r3.svc.Handle(ctx, ho); !got.SetCookie {
			t.Errorf("%s: the real browser could not finish: %+v", name, got)
		}
	}
	_ = hu
	_ = hub
	// a link replayed after use is refused
	ho := get(PathHandoff)
	ho.Query, ho.SigninCookie = hu.RawQuery, s1.SigninCookie
	if got := r.svc.Handle(ctx, ho); got.SetCookie {
		t.Error("handoff code is single use")
	}
	// a tampered code
	ho.Query = "code=" + url.QueryEscape("garbage")
	if got := r.svc.Handle(ctx, ho); got.Status != 401 {
		t.Errorf("tampered: %+v", got)
	}
	// an expired envelope
	e := envReq("abc")
	e.SigninCookie = "ck"
	r.now = r.now.Add(2 * time.Hour * 24)
	r.svc.Table.Replace(r.rt.routes, r.rt.set)
	if got := r.svc.Handle(ctx, e); got.Status != 401 {
		t.Errorf("expired: %+v", got)
	}
}

// signIn2 runs sign-in and callback and remembers the cookie, without the handoff.
func signIn2(t *testing.T, r *rig) (Result, Result, Result) {
	t.Helper()
	ctx := context.Background()
	s1 := r.svc.Handle(ctx, get(PathSignin))
	r.lastSigninCookie = s1.SigninCookie
	u, _ := url.Parse(s1.Location)
	cb := Request{Method: "GET", Host: "collab.example.com", Path: PathCallback,
		Query: url.Values{"state": {u.Query().Get("state")}, "code": {"authcode"}}.Encode()}
	return s1, r.svc.Handle(ctx, cb), Result{}
}

// Finding 9.
func TestSafeReturnControlCharacters(t *testing.T) {
	for _, bad := range []string{"/%09/evil.com", "/\t/evil.com", "/\n/evil.com", "/\x00x", "/\x7f", "/%5Cevil.com", "/\\evil.com",
		"//evil.com", "/%2F/evil.com", "/a\\b", "/a%0d%0ab", "/ok\u0085", "/%00", "/_afs/signin", "http://evil.com", "evil.com", ""} {
		if got := safeReturn(bad, ""); got != "/" {
			t.Errorf("path %q -> %q", bad, got)
		}
		if got := returnTarget(bad); got != "/" {
			t.Errorf("return %q -> %q", bad, got)
		}
	}
	for _, c := range []struct{ path, q, want string }{{"/", "", "/"}, {"/a/b", "x=1", "/a/b?x=1"}, {"/a%20b", "", "/a%20b"}, {"/a//b", "", "/a//b"}} {
		if got := safeReturn(c.path, c.q); got != c.want {
			t.Errorf("%q -> %q", c.path, got)
		}
	}
	if got := safeReturn("/ok", "a=%09b&c=\t"); got != "/" {
		t.Errorf("control character in query: %q", got)
	}
	// through the sign-in redirect itself
	r := newRig(t, nil)
	req := get("/%09/evil.com")
	req.Path = "/\t/evil.com"
	req.Navigate = true
	got := r.svc.Handle(context.Background(), req)
	if got.Action != Redirect || !strings.HasSuffix(got.Location, "return=%2F") {
		t.Errorf("%+v", got)
	}
	_, _, s3 := signIn(t, newRig(t, nil), "/%09/evil.com")
	if s3.Location != "/" {
		t.Errorf("handoff return %q", s3.Location)
	}
}

// Finding 11.
func TestMethodOverrideQuery(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		sub, method, query string
		refused            bool
	}{
		{"viewer", "GET", "_method=DELETE", true}, {"viewer", "GET", "a=1&_METHOD=PUT", true}, {"viewer", "GET", "a=1", false},
		{"alice", "POST", "_method=DELETE", false}, {"alice", "GET", "_method=POST", true},
	} {
		r := newRig(t, nil)
		req := get("/api/x")
		req.Method, req.Query, req.Cookie, req.Origin = tc.method, tc.query, r.session(tc.sub), "https://"+host
		got := r.svc.Handle(ctx, req)
		if tc.refused != (got.Status == 403 && got.Reason == "permission_required") {
			t.Errorf("%+v: %d %q", tc, got.Status, got.Reason)
		}
	}
}

// Finding 12 and the groups hash in the cache key.
func TestGroupOnlySessionReauthenticates(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	r.pr.people["grp"] = []Binding{{Role: "team_admin", Group: true}}
	r.pr.people["mixed"] = []Binding{{Role: "team_admin", Group: true}, {Role: "team_viewer"}}
	r.pr.people["usr"] = []Binding{{Role: "team_admin"}}
	mk := func(sub string, age time.Duration) string {
		raw := bytes.Repeat([]byte{byte((len(sub) + int(sub[0]) + int(age/time.Minute)) % 256)}, 32) //nolint:gosec // test fixture, reduced modulo 256
		r.ses.sessions[hash2(raw)] = Session{TeamID: "team_01", Issuer: "i", Subject: sub,
			ExpiresAt: r.now.Add(8*time.Hour - age), CreatedAt: r.now.Add(-age)}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	nav := func(ck string) Result {
		req := get("/board")
		req.Cookie, req.Navigate = ck, true
		return r.svc.Handle(ctx, req)
	}
	if got := nav(mk("grp", 59*time.Minute)); got.Action != Forward {
		t.Fatalf("young group session: %+v", got)
	}
	got := nav(mk("grp", 61*time.Minute))
	if got.Action != Redirect || !strings.Contains(got.Location, "silent=1") {
		t.Fatalf("old group session must re-authenticate silently: %+v", got)
	}
	api := get("/api/x")
	api.Cookie = mk("grp", 3*time.Hour)
	if got := r.svc.Handle(ctx, api); got.Status != 401 || got.Reason != "session_expired" {
		t.Fatalf("non-navigation: %+v", got)
	}
	for _, sub := range []string{"usr", "mixed"} {
		if got := nav(mk(sub, 5*time.Hour)); got.Action != Forward {
			t.Errorf("%s keeps its session: %+v", sub, got)
		}
	}
	// no CreatedAt: derived from the expiry and the configured lifetime
	raw := bytes.Repeat([]byte{99}, 32)
	r.ses.sessions[hash2(raw)] = Session{TeamID: "team_01", Subject: "grp", ExpiresAt: r.now.Add(time.Hour)}
	if got := nav(base64.RawURLEncoding.EncodeToString(raw)); got.Action != Redirect {
		t.Errorf("derived age: %+v", got)
	}
	// the silent sign-in asks the provider for prompt=none, and falls back to interactive on login_required
	si := get(PathSignin)
	si.Query = "return=%2Fboard&silent=1"
	s1 := r.svc.Handle(ctx, si)
	if r.id.prompt != "none" {
		t.Fatalf("prompt = %q", r.id.prompt)
	}
	u, _ := url.Parse(s1.Location)
	cb := Request{Method: "GET", Host: "collab.example.com", Path: PathCallback,
		Query: url.Values{"state": {u.Query().Get("state")}, "error": {"login_required"}}.Encode()}
	fb := r.svc.Handle(ctx, cb)
	if fb.Action != Redirect || fb.Location != "https://"+host+PathSignin+"?return=%2Fboard" {
		t.Fatalf("fallback: %+v", fb)
	}
	r.svc.Handle(ctx, get(PathSignin))
	if r.id.prompt != "" {
		t.Errorf("interactive sign-in must not send a prompt: %q", r.id.prompt)
	}
	cb.Query = url.Values{"state": {u.Query().Get("state")}, "error": {"access_denied"}}.Encode()
	if got := r.svc.Handle(ctx, cb); got.Status != 401 {
		t.Errorf("other provider errors still fail: %+v", got)
	}
}

func TestBearerCacheKeyIncludesGroups(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, nil)
	req := get("/api/x")
	req.XAfsAuthorization = "Bearer jwt"
	r.id.claims.Groups = []string{"a", "b"}
	r.svc.Handle(ctx, req)
	r.id.claims.Groups = []string{"b", "a"}
	r.svc.Handle(ctx, req)
	if r.pr.calls != 1 {
		t.Fatalf("same groups in another order share an entry: %d", r.pr.calls)
	}
	r.id.claims.Groups = []string{"a"}
	r.svc.Handle(ctx, req)
	if r.pr.calls != 2 {
		t.Fatalf("different groups must not reuse the decision: %d", r.pr.calls)
	}
}

// Finding 13.
func TestHostNormalization(t *testing.T) {
	for in, want := range map[string]string{"Alpha.Collab.Example.com": host, host + ":443": host, host + ".": host, " " + strings.ToUpper(host) + ".:8443": host, "x": "x"} {
		if got := NormalizeHost(in); got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
	ctx := context.Background()
	r := newRig(t, nil)
	req := get("/x")
	req.Host = "ALPHA.collab.example.com.:443"
	req.XAfsAuthorization = "Bearer jwt"
	if got := r.svc.Handle(ctx, req); got.Action != Forward {
		t.Fatalf("%+v", got)
	}
	// sign-in with an odd Host still binds to the route's hostname and the origin check agrees
	si := get(PathSignin)
	si.Host = "Alpha.Collab.Example.Com:443"
	s1 := r.svc.Handle(ctx, si)
	u, _ := url.Parse(s1.Location)
	cb := Request{Method: "GET", Host: "collab.example.com", Path: PathCallback,
		Query: url.Values{"state": {u.Query().Get("state")}, "code": {"c"}}.Encode()}
	s2 := r.svc.Handle(ctx, cb)
	if !strings.HasPrefix(s2.Location, "https://"+host+PathHandoff) {
		t.Fatalf("%+v", s2)
	}
	post := get("/x")
	post.Host, post.Method, post.Origin, post.Cookie = "ALPHA.collab.example.com:443", "POST", "https://"+host, r.session("alice")
	if got := r.svc.Handle(ctx, post); got.Action != Forward {
		t.Fatalf("%+v", got)
	}
}

// Finding 14 (RD-26): a redirect to sign-in is not counted at all.
func TestSigninRedirectNotCounted(t *testing.T) {
	r := newRig(t, nil)
	req := get("/board")
	req.Navigate = true
	if got := r.svc.Handle(context.Background(), req); got.Action != Redirect {
		t.Fatalf("%+v", got)
	}
	if r.svc.Access.Len() != 0 {
		t.Errorf("redirect to sign-in was counted: %d rows", r.svc.Access.Len())
	}
	if err := r.svc.Flush(context.Background()); err != nil || len(r.sink.rows) != 0 {
		t.Errorf("%v %v", err, r.sink.rows)
	}
}

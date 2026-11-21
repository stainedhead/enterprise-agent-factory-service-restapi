package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_800_000_000, 0)} }

func TestLimiterBurstRefillAndRetryAfter(t *testing.T) {
	ck := newClock()
	l := NewLimiter(LimiterConfig{Now: ck.now})
	allowed := 0
	for i := 0; i < 60; i++ {
		if ok, _ := l.Allow("p"); ok {
			allowed++
		}
	}
	if allowed != 40 {
		t.Fatalf("60 requests in one instant: %d allowed, want the burst of 40", allowed)
	}
	if _, wait := l.Allow("p"); wait != 50*time.Millisecond {
		t.Fatalf("wait = %v, want 1/20 s", wait)
	}
	// Another key is unaffected.
	if ok, _ := l.Allow("q"); !ok {
		t.Fatal("a second key was throttled")
	}
	// Half a second refills ten tokens.
	ck.advance(500 * time.Millisecond)
	n := 0
	for i := 0; i < 20; i++ {
		if ok, _ := l.Allow("p"); ok {
			n++
		}
	}
	if n != 10 {
		t.Fatalf("after 500 ms: %d allowed, want 10", n)
	}
}

func TestLimiterReplicasShareTheRate(t *testing.T) {
	ck := newClock()
	l := NewLimiter(LimiterConfig{Replicas: 4, Now: ck.now})
	n := 0
	for i := 0; i < 100; i++ {
		if ok, _ := l.Allow("p"); ok {
			n++
		}
	}
	if n != 10 {
		t.Fatalf("burst on 4 replicas = %d, want 40/4", n)
	}
	// Retry-After is at least one whole second.
	if e := Throttled(10 * time.Millisecond); e.RetryAfterSeconds != 1 || e.Kind != domain.KindThrottled {
		t.Fatalf("%+v", e)
	}
	if e := Throttled(1500 * time.Millisecond); e.RetryAfterSeconds != 2 {
		t.Fatalf("%+v", e)
	}
	// A degenerate share still allows one request.
	one := NewLimiter(LimiterConfig{Replicas: 1000, Now: ck.now})
	if ok, _ := one.Allow("p"); !ok {
		t.Fatal("burst floor of 1 not applied")
	}
}

func TestLimiterEvictsIdleBucketsAndBoundsMemory(t *testing.T) {
	ck := newClock()
	l := NewLimiter(LimiterConfig{Now: ck.now})
	for i := 0; i < 1000; i++ {
		l.Allow("k" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)))
	}
	before := l.Size()
	if before < 100 {
		t.Fatalf("setup: %d buckets", before)
	}
	ck.advance(idleEvicted + sweepEvery)
	l.Allow("fresh")
	if l.Size() != 1 {
		t.Fatalf("idle buckets not evicted: %d left", l.Size())
	}
	// Past the bound, new keys share one bucket instead of growing the map.
	l2 := NewLimiter(LimiterConfig{Now: ck.now})
	l2.mu.Lock()
	for i := 0; i < maxBuckets; i++ {
		l2.buckets[strings.Repeat("a", 1)+string(rune(i))+string(rune(i>>8))+string(rune(i>>16))] = &bucket{tokens: 1, last: ck.t}
	}
	l2.mu.Unlock()
	for i := 0; i < 5; i++ {
		l2.Allow("new-" + string(rune('a'+i)))
	}
	if l2.Size() > maxBuckets+1 {
		t.Fatalf("map grew past the bound: %d", l2.Size())
	}
}

func req(method, path string, hdr ...string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Add(hdr[i], hdr[i+1])
	}
	return r
}

func TestBearer(t *testing.T) {
	cases := []struct {
		name    string
		hdr     []string
		want    string
		wantErr bool
	}{
		{"none", nil, "", false},
		{"bearer", []string{"Authorization", "Bearer abc.def"}, "abc.def", false},
		{"scheme is case-insensitive", []string{"Authorization", "bearer abc"}, "abc", false},
		{"basic", []string{"Authorization", "Basic abc"}, "", true},
		{"no token", []string{"Authorization", "Bearer "}, "", true},
		{"scheme only", []string{"Authorization", "Bearer"}, "", true},
		{"two headers", []string{"Authorization", "Bearer a", "Authorization", "Bearer b"}, "", true},
		{"embedded space", []string{"Authorization", "Bearer a b"}, "", true},
		{"too long", []string{"Authorization", "Bearer " + strings.Repeat("a", maxTokenLength+1)}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Bearer(req("GET", "/", c.hdr...))
			if got != c.want || (err != nil) != c.wantErr {
				t.Fatalf("Bearer = %q, %v", got, err)
			}
			if err != nil && domain.KindOf(err) != domain.KindUnauthenticated {
				t.Fatalf("kind = %s", domain.KindOf(err))
			}
		})
	}
}

type fakeCreds struct {
	byToken map[string]*authz.Principal
	err     error
	calls   int
}

func (f *fakeCreds) Authenticate(_ context.Context, tok string) (*authz.Principal, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if p, ok := f.byToken[tok]; ok {
		return p, nil
	}
	return nil, domain.Unauthenticated()
}

// RA-US-15.1: per principal; another principal unaffected; failed authentication per source address.
func TestAuthnRateLimits(t *testing.T) {
	ck := newClock()
	creds := &fakeCreds{byToken: map[string]*authz.Principal{
		"a": {ID: "usr_a", Type: authz.PrincipalUser}, "b": {ID: "usr_b", Type: authz.PrincipalUser},
	}}
	a := &Authn{Auth: creds, Limiter: NewLimiter(LimiterConfig{Now: ck.now})}
	call := func(tok, ip string) error {
		r := req("GET", "/", "Authorization", "Bearer "+tok)
		r.RemoteAddr = ip + ":1234"
		_, err := a.Authenticate(r)
		return err
	}
	ok, throttled := 0, 0
	for i := 0; i < 60; i++ {
		switch domain.KindOf(call("a", "198.51.100.1")) {
		case "":
			ok++
		case domain.KindThrottled:
			throttled++
		default:
			t.Fatal("unexpected error")
		}
	}
	if ok != 40 || throttled != 20 {
		t.Fatalf("ok=%d throttled=%d, want 40 and 20", ok, throttled)
	}
	if err := call("b", "198.51.100.1"); err != nil {
		t.Fatalf("another principal on the same address was throttled: %v", err)
	}

	// Unauthenticated floods are limited per source address and answer 429, then 401 for others.
	bad, throttled := 0, 0
	for i := 0; i < 60; i++ {
		switch domain.KindOf(call("nope", "203.0.113.9")) {
		case domain.KindUnauthenticated:
			bad++
		case domain.KindThrottled:
			throttled++
		}
	}
	if bad != 40 || throttled != 20 {
		t.Fatalf("bad=%d throttled=%d", bad, throttled)
	}
	if err := call("nope", "203.0.113.10"); domain.KindOf(err) != domain.KindUnauthenticated {
		t.Fatalf("a different source address was throttled: %v", err)
	}
	// No credential at all counts against the source address too.
	r := req("GET", "/")
	r.RemoteAddr = "203.0.113.9:1"
	if _, err := a.Authenticate(r); domain.KindOf(err) != domain.KindThrottled {
		t.Fatalf("anonymous flood: %v", err)
	}
}

func TestAuthnErrors(t *testing.T) {
	const secret = "supersecret-token-value"
	creds := &fakeCreds{err: domain.Unavailable("identity provider keys", errors.New("down: "+secret))}
	a := &Authn{Auth: creds}
	_, err := a.Authenticate(req("GET", "/", "Authorization", "Bearer "+secret))
	if domain.KindOf(err) != domain.KindUnavailable {
		t.Fatalf("Unavailable must pass through, got %v", err)
	}
	// An error never carries the Authorization header it came from (the verifier's own cause aside).
	creds.err = nil
	_, err = a.Authenticate(req("GET", "/", "Authorization", "Bearer "+secret))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("error %v leaks the token", err)
	}
	if p, err := a.Authenticate(req("GET", "/")); p != nil || err != nil {
		t.Fatalf("no credential = (nil, nil), got %v %v", p, err)
	}
	if _, err := a.Authenticate(req("GET", "/", "Authorization", "Basic x")); domain.KindOf(err) != domain.KindUnauthenticated {
		t.Fatalf("basic: %v", err)
	}
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name string
		hops int
		addr string
		xff  []string
		want string
	}{
		{"socket peer", 0, "198.51.100.7:5000", []string{"1.2.3.4"}, "198.51.100.7"},
		{"one proxy appends the client", 1, "10.0.0.1:5000", []string{"9.9.9.9, 203.0.113.5"}, "203.0.113.5"},
		{"client-supplied left entries ignored", 1, "10.0.0.1:5000", []string{"6.6.6.6", "203.0.113.5"}, "203.0.113.5"},
		{"two proxies", 2, "10.0.0.1:5000", []string{"203.0.113.5, 10.0.0.9"}, "203.0.113.5"},
		{"fewer entries than hops", 3, "10.0.0.1:5000", []string{"203.0.113.5"}, "10.0.0.1"},
		{"garbage entry", 1, "10.0.0.1:5000", []string{"not-an-ip"}, "10.0.0.1"},
		{"ipv6", 0, "[2001:db8::1]:443", nil, "2001:db8::1"},
		{"no port", 0, "198.51.100.7", nil, "198.51.100.7"},
	}
	for _, c := range cases {
		r := req("GET", "/")
		r.RemoteAddr = c.addr
		for _, x := range c.xff {
			r.Header.Add("X-Forwarded-For", x)
		}
		if got := ClientIP(r, c.hops); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// RA-US-15.2, CT-API-4.
func TestCORS(t *testing.T) {
	ck := newClock()
	calls := 0
	origins := func(context.Context) ([]string, error) {
		calls++
		return []string{"https://console.example.com"}, nil
	}
	reached := 0
	h := CORS(origins, ck.now, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	}))
	do := func(method string, hdr ...string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req(method, "/v1/teams", hdr...))
		return rec
	}

	rec := do("GET")
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Vary") != "" || calls != 0 {
		t.Fatalf("a request without Origin was touched: %v (settings read %d times)", rec.Header(), calls)
	}
	rec = do("GET", "Origin", "https://evil.example.com")
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Expose-Headers") != "" || reached != 2 {
		t.Fatalf("unlisted origin: %v", rec.Header())
	}
	if rec.Header().Get("Vary") != "Origin" {
		t.Fatalf("Vary = %q", rec.Header().Get("Vary"))
	}
	rec = do("GET", "Origin", "https://console.example.com")
	hdr := rec.Header()
	if hdr.Get("Access-Control-Allow-Origin") != "https://console.example.com" || hdr.Get("Access-Control-Allow-Credentials") != "" ||
		hdr.Get("Access-Control-Expose-Headers") != "ETag, X-Request-Id, Idempotent-Replayed, Retry-After, X-Afs-Contract-Version, X-Afs-Next-Token" {
		t.Fatalf("listed origin: %v", hdr)
	}
	// Preflight: answered here (204), never reaching the router.
	before := reached
	rec = do("OPTIONS", "Origin", "https://console.example.com", "Access-Control-Request-Method", "PATCH", "Access-Control-Request-Headers", "if-match")
	if rec.Code != http.StatusNoContent || reached != before {
		t.Fatalf("preflight: %d, reached router: %v", rec.Code, reached != before)
	}
	hdr = rec.Header()
	if hdr.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type, If-Match, Idempotency-Key, X-Request-Id" ||
		!strings.Contains(hdr.Get("Access-Control-Allow-Methods"), "PATCH") || hdr.Get("Access-Control-Allow-Origin") != "https://console.example.com" ||
		hdr.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("preflight headers: %v", hdr)
	}
	// A preflight from an unlisted origin gets no CORS headers.
	rec = do("OPTIONS", "Origin", "https://evil.example.com", "Access-Control-Request-Method", "GET")
	if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Code == http.StatusNoContent {
		t.Fatalf("unlisted preflight: %d %v", rec.Code, rec.Header())
	}
	if rec = do("GET", "Origin", "null"); rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal(`origin "null" allowed`)
	}
	// The list is cached for 30 s, then read again.
	n := calls
	do("GET", "Origin", "https://console.example.com")
	if calls != n {
		t.Fatal("origins re-read inside the TTL")
	}
	ck.advance(originsTTL + time.Second)
	do("GET", "Origin", "https://console.example.com")
	if calls != n+1 {
		t.Fatalf("origins not re-read after the TTL (%d)", calls-n)
	}
}

func TestCORSDefaultsAndFailures(t *testing.T) {
	ck := newClock()
	empty := CORS(func(context.Context) ([]string, error) { return nil, nil }, ck.now, nil)
	rec := httptest.NewRecorder()
	empty(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req("GET", "/", "Origin", "https://console.example.com"))
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("default (empty) list allowed an origin")
	}
	// The list cannot be read: nothing is allowed, and the last known list survives a later outage.
	fail := true
	flaky := CORS(func(context.Context) ([]string, error) {
		if fail {
			return nil, errors.New("db down")
		}
		return []string{"https://a.example.com"}, nil
	}, ck.now, nil)
	h := flaky(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	get := func() string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req("GET", "/", "Origin", "https://a.example.com"))
		return rec.Header().Get("Access-Control-Allow-Origin")
	}
	if get() != "" {
		t.Fatal("origin allowed with the setting unreadable")
	}
	ck.advance(originsTTL + time.Second)
	fail = false
	if get() == "" {
		t.Fatal("origin not allowed once readable")
	}
	ck.advance(originsTTL + time.Second)
	fail = true
	if get() == "" {
		t.Fatal("last known list dropped during an outage")
	}
}

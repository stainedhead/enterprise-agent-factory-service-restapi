//nolint:bodyclose // the do helper closes every response body
package edgehttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
)

type stub struct {
	mu      sync.Mutex
	res     edge.Result
	got     edge.Request
	block   bool
	scanned int
	frames  bool
}

func (s *stub) Handle(_ context.Context, r edge.Request) edge.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = r
	return s.res
}

func (s *stub) OnScan(_ context.Context, r edge.Result, m []collab.Match) (edge.Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanned += len(m)
	if len(m) > 0 && s.block {
		r.Action, r.Status, r.Code = edge.Respond, 422, "SecretDetected"
		r.Patterns = []string{m[0].Pattern}
		return r, true
	}
	return r, false
}

func (s *stub) FrameAllowed(edge.Result) bool { return s.frames }

type obs struct {
	mu     sync.Mutex
	status []int
}

func (o *obs) Request(_ collab.Class, status int, _ time.Duration) {
	o.mu.Lock()
	o.status = append(o.status, status)
	o.mu.Unlock()
}

func (o *obs) statuses() []int {
	for i := 0; i < 100; i++ {
		o.mu.Lock()
		n := len(o.status)
		o.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]int(nil), o.status...)
}

// rig starts an upstream and an edge whose dialer always reaches the upstream.
type rig struct {
	mu   sync.Mutex
	st   *stub
	up   *httptest.Server
	edge *httptest.Server
	obs  *obs
	seen http.Header
	body []byte
	path string
	host string
}

func newRig(t *testing.T, idle time.Duration, upstream http.HandlerFunc) *rig {
	t.Helper()
	r := &rig{st: &stub{}, obs: &obs{}}
	r.up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		b, _ := io.ReadAll(q.Body)
		r.mu.Lock()
		r.seen, r.path, r.host, r.body = q.Header.Clone(), q.URL.RequestURI(), q.Host, b
		r.mu.Unlock()
		if upstream != nil {
			upstream(w, q)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		w.Header().Set("X-Paperclip", "1")
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(r.up.Close)
	addr := r.up.Listener.Addr().String()
	srv := New(Config{Service: r.st, IdleTimeout: idle, Observer: r.obs,
		Dial: func(ctx context.Context, n, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, n, addr)
		}})
	r.edge = httptest.NewServer(srv.Handler())
	t.Cleanup(r.edge.Close)
	r.st.res = edge.Result{Action: edge.Forward, Upstream: "http://paperclip.team-a.svc.cluster.local:3100", Status: 200, MaxBody: 1 << 20,
		Route: collab.Route{TeamID: "team_01"}}
	return r
}

func do(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(b)
}

func TestForwardHeadersAndResponse(t *testing.T) {
	r := newRig(t, 0, nil)
	req, _ := http.NewRequestWithContext(context.Background(), "GET", r.edge.URL+"/api/issues?x=1&y=%2F", nil)
	req.Host = "alpha.collab.example.com"
	req.Header.Set("Cookie", "__Host-afs-collab=secret; paperclip=abc")
	req.Header.Set("X-Afs-Authorization", "Bearer tok")
	req.Header.Set("Authorization", "Bearer paperclip")
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Amzn-Trace-Id", "Root=1")
	req.Header.Set("X-Real-Ip", "6.6.6.6")
	resp, body := do(t, req)
	if resp.StatusCode != 200 || body != "hello" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if r.Path() != "/api/issues?x=1&y=%2F" || r.Host() != "alpha.collab.example.com" {
		t.Errorf("path %q host %q", r.Path(), r.Host())
	}
	if c := r.Seen().Get("Cookie"); c != "paperclip=abc" {
		t.Errorf("cookie %q", c)
	}
	for _, h := range []string{"X-Afs-Authorization", "X-Amzn-Trace-Id", "X-Real-Ip"} {
		if r.Seen().Get(h) != "" {
			t.Errorf("%s reached the upstream", h)
		}
	}
	if r.Seen().Get("Authorization") != "Bearer paperclip" {
		t.Error("the Paperclip credential must pass through")
	}
	if xf := r.Seen().Get("X-Forwarded-For"); xf == "" || strings.Contains(xf, "6.6.6.6") {
		t.Errorf("X-Forwarded-For %q", xf)
	}
	if r.Seen().Get("X-Forwarded-Proto") != "https" || !strings.HasPrefix(r.Seen().Get("X-Request-Id"), "req_") {
		t.Errorf("%v", r.Seen())
	}
	if resp.Header.Get("Strict-Transport-Security") == "" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		resp.Header.Get("Referrer-Policy") != "strict-origin-when-cross-origin" {
		t.Errorf("%v", resp.Header)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp != "default-src 'self'; frame-ancestors 'none'" {
		t.Errorf("csp %q", csp)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("X-Paperclip") != "1" {
		t.Errorf("CORS must be removed and other headers kept: %v", resp.Header)
	}
	if st := r.obs.statuses(); len(st) != 1 || st[0] != 200 {
		t.Errorf("observer %v", st)
	}
	// the request given to the use case
	if r.st.Got().Host != "alpha.collab.example.com" || r.st.Got().XAfsAuthorization != "Bearer tok" || r.st.Got().Cookie != "secret" || r.st.Got().Path != "/api/issues" {
		t.Errorf("%+v", r.st.Got())
	}
}

func TestOnlyAFSCookieRemoved(t *testing.T) {
	r := newRig(t, 0, nil)
	req, _ := http.NewRequestWithContext(context.Background(), "GET", r.edge.URL+"/", nil)
	req.Header.Set("Cookie", "__Host-afs-collab=secret")
	do(t, req)
	if r.Seen().Get("Cookie") != "" {
		t.Errorf("%q", r.Seen().Get("Cookie"))
	}
}

func TestExistingFrameAncestorsKept(t *testing.T) {
	r := newRig(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Security-Policy", "frame-ancestors https://x")
	})
	resp, _ := do(t, mustGet(r.edge.URL))
	if resp.Header.Get("Content-Security-Policy") != "frame-ancestors https://x" {
		t.Errorf("%q", resp.Header.Get("Content-Security-Policy"))
	}
}

func mustGet(u string) *http.Request {
	req, _ := http.NewRequestWithContext(context.Background(), "GET", u, nil)
	return req
}

func TestRespond(t *testing.T) {
	r := newRig(t, 0, nil)
	r.st.res = edge.Result{Action: edge.Respond, Status: 403, Code: "AccessDenied", Reason: "permission_required", Required: "collaboration:contribute", RetryAfter: 0}
	resp, body := do(t, mustGet(r.edge.URL+"/x"))
	var env struct {
		Code, Message, RequestID string
		Details                  map[string]any
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil || resp.StatusCode != 403 || env.Code != "AccessDenied" || env.Details["reason"] != "permission_required" ||
		env.Details["requiredPermission"] != "collaboration:contribute" || env.RequestID != resp.Header.Get("X-Request-Id") || env.Message == "" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("%v", resp.Header)
	}

	r.st.res = edge.Result{Action: edge.Respond, Status: 503, Code: "Unavailable", Reason: "starting", RetryAfter: 30}
	nav := mustGet(r.edge.URL + "/x")
	nav.Header.Set("Accept", "text/html,application/xhtml+xml")
	nav.Header.Set("Sec-Fetch-Mode", "navigate")
	resp, body = do(t, nav)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") != "30" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(body, resp.Header.Get("X-Request-Id")) || !strings.Contains(body, "starting") {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, body)
	}
	head, _ := http.NewRequestWithContext(context.Background(), "HEAD", r.edge.URL+"/x", nil)
	if resp, body = do(t, head); resp.StatusCode != 503 || body != "" {
		t.Errorf("HEAD: %d %q", resp.StatusCode, body)
	}
	r.st.res = edge.Result{Action: edge.Respond, Status: 404, Code: "NotFound"}
	if _, body = do(t, mustGet(r.edge.URL)); !strings.Contains(body, `"code":"NotFound"`) || strings.Contains(body, "details") {
		t.Errorf("%s", body)
	}
	head2, _ := http.NewRequestWithContext(context.Background(), "HEAD", r.edge.URL, nil)
	head2.Header.Set("Accept", "text/html")
	head2.Header.Set("Sec-Fetch-Mode", "navigate")
	if _, body = do(t, head2); body != "" {
		t.Error("HEAD page has no body")
	}
}

func TestRedirects(t *testing.T) {
	r := newRig(t, 0, nil)
	r.st.res = edge.Result{Action: edge.Redirect, Status: 302, Location: "/board?x=1", SetCookie: true, Cookie: "sid", CookieMaxAge: 600}
	resp, _ := do(t, mustGet(r.edge.URL+"/_afs/handoff"))
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/board?x=1" {
		t.Fatalf("%v", resp.Header)
	}
	ck := resp.Cookies()
	if len(ck) != 1 || ck[0].Name != "__Host-afs-collab" || ck[0].Value != "sid" || !ck[0].Secure || !ck[0].HttpOnly || ck[0].SameSite != http.SameSiteLaxMode || ck[0].Domain != "" || ck[0].MaxAge != 600 {
		t.Fatalf("%+v", ck)
	}
	r.st.res = edge.Result{Action: edge.Redirect, Status: 302, Location: "/", ClearCookie: true}
	resp, _ = do(t, mustGet(r.edge.URL))
	if ck := resp.Cookies(); len(ck) != 1 || ck[0].MaxAge >= 0 {
		t.Fatalf("%+v", ck)
	}
	r.st.res = edge.Result{Action: edge.Redirect, Status: 302, Location: "https://idp.example/auth"}
	resp, _ = do(t, mustGet(r.edge.URL))
	if len(resp.Cookies()) != 0 || resp.Header.Get("Location") != "https://idp.example/auth" {
		t.Fatalf("%v", resp.Header)
	}
}

func TestBodyScan(t *testing.T) {
	const key = "AKIAIOSFODNN7EXAMPLE" //nolint:gosec // the documented AWS example key, not a credential
	r := newRig(t, 0, nil)
	r.st.res.ScanBody, r.st.res.ScanMode = true, "block"
	r.st.block = true
	post := func(body string) (*http.Response, string) {
		req, _ := http.NewRequestWithContext(context.Background(), "POST", r.edge.URL+"/api", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		return do(t, req)
	}
	resp, body := post(`{"k":"` + key + `"}`)
	if resp.StatusCode != 422 || !strings.Contains(body, "aws_access_key") || strings.Contains(body, key) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if len(r.Body()) != 0 {
		t.Error("a blocked body must not reach the upstream")
	}
	r.st.block = false
	if resp, _ = post(`{"k":"` + key + `"}`); resp.StatusCode != 200 || !strings.Contains(string(r.Body()), key) {
		t.Fatalf("warn must forward the body intact: %d %q", resp.StatusCode, r.Body())
	}
	if resp, _ = post(`{"clean":true}`); resp.StatusCode != 200 || string(r.Body()) != `{"clean":true}` {
		t.Fatalf("%d %q", resp.StatusCode, r.Body())
	}
	r.st.res.MaxBody = 10
	if resp, body = post(strings.Repeat("a", 50)); resp.StatusCode != 400 || !strings.Contains(body, "body_too_large") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestBodyLimitWithoutScan(t *testing.T) {
	r := newRig(t, 0, nil)
	r.st.res.MaxBody = 10
	req, _ := http.NewRequestWithContext(context.Background(), "POST", r.edge.URL+"/api", io.NopCloser(strings.NewReader(strings.Repeat("a", 5000))))
	resp, body := do(t, req)
	if resp.StatusCode != 400 || !strings.Contains(body, "body_too_large") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestUpstreamDown(t *testing.T) {
	r := newRig(t, 0, nil)
	r.up.Close()
	resp, body := do(t, mustGet(r.edge.URL))
	if resp.StatusCode != 503 || !strings.Contains(body, "upstream_unavailable") || resp.Header.Get("Retry-After") != "30" {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, body)
	}
	r.st.res.Upstream = "http://%zz"
	if resp, _ = do(t, mustGet(r.edge.URL)); resp.StatusCode != 503 {
		t.Fatalf("bad upstream address: %d", resp.StatusCode)
	}
}

func TestEventStreamIdleTimeout(t *testing.T) {
	r := newRig(t, 80*time.Millisecond, func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-q.Context().Done()
	})
	resp, err := http.DefaultClient.Do(mustGet(r.edge.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	line, _ := br.ReadString('\n')
	if line != "data: one\n" {
		t.Fatalf("event not flushed: %q", line)
	}
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, br); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("an idle stream must be closed")
	}
}

func TestHelpers(t *testing.T) {
	if got := clientIP("1.2.3.4:5"); got != "1.2.3.4" {
		t.Error(got)
	}
	if got := clientIP("weird"); got != "weird" {
		t.Error(got)
	}
	h := http.Header{"Cookie": {"a=1; __Host-afs-collab=x", "b=2"}}
	stripCookie(h)
	if h.Get("Cookie") != "a=1; b=2" {
		t.Errorf("%q", h.Get("Cookie"))
	}
	stripCookie(http.Header{})
	if !isUpgrade(&http.Request{Header: http.Header{"Upgrade": {"WebSocket"}, "Connection": {"keep-alive, Upgrade"}}}) {
		t.Error("upgrade")
	}
	if isUpgrade(&http.Request{Header: http.Header{"Upgrade": {"websocket"}}}) {
		t.Error("needs Connection: Upgrade")
	}
	if messageOf(edge.Result{Code: "Throttled"}) == "" || messageOf(edge.Result{Message: "m"}) != "m" {
		t.Error("messages")
	}
	if _, err := (&idleBody{ReadCloser: io.NopCloser(bytes.NewReader(nil)), d: time.Second, timer: time.NewTimer(time.Hour)}).Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Error(err)
	}
}

func (r *rig) Seen() http.Header  { r.mu.Lock(); defer r.mu.Unlock(); return r.seen }
func (r *rig) Path() string       { r.mu.Lock(); defer r.mu.Unlock(); return r.path }
func (r *rig) Host() string       { r.mu.Lock(); defer r.mu.Unlock(); return r.host }
func (r *rig) Body() []byte       { r.mu.Lock(); defer r.mu.Unlock(); return r.body }
func (s *stub) Got() edge.Request { s.mu.Lock(); defer s.mu.Unlock(); return s.got }

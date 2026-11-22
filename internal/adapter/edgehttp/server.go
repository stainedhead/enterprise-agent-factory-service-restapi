// Package edgehttp is the HTTP adapter of afs-edge: it turns a request into an edge.Request, asks the use
// case what to do, and then forwards, answers or redirects. It holds no policy; every decision is the use
// case's. Request and response bodies are never logged.
package edgehttp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/edgesession"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
)

// DefaultIdleTimeout ends a WebSocket or event stream with no traffic (EDGE-5).
const DefaultIdleTimeout = 10 * time.Minute

// DefaultRecheckEvery is how often an open WebSocket or event stream is authorized again.
const DefaultRecheckEvery = 60 * time.Second

// Decider is what the adapter needs of the use case.
type Decider interface {
	Handle(ctx context.Context, req edge.Request) edge.Result
	OnScan(ctx context.Context, r edge.Result, matches []collab.Match) (edge.Result, bool)
	FrameAllowed(r edge.Result) bool
	// Recheck decides an open stream again with fresh data (finding 2).
	Recheck(ctx context.Context, req edge.Request) edge.Result
	// OnUnreadable answers a body the scanner could not decode (415).
	OnUnreadable(r edge.Result) edge.Result
}

// Observer receives one call per request for metrics.
type Observer interface {
	Request(class collab.Class, status int, d time.Duration)
}

// Config configures a Server.
type Config struct {
	Service Decider
	// Dial opens connections to team upstreams; default net.Dialer with a 5 s timeout.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// IdleTimeout defaults to DefaultIdleTimeout.
	IdleTimeout time.Duration
	// RecheckEvery is the re-authorization interval of open streams; default DefaultRecheckEvery.
	RecheckEvery time.Duration
	// NewTicker is the clock of the re-authorization; default time.NewTicker. Tests inject their own.
	NewTicker func(d time.Duration) (<-chan time.Time, func())
	Log       *slog.Logger
	Observer  Observer
}

// Server is the edge's http.Handler.
type Server struct {
	svc       Decider
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	idle      time.Duration
	recheck   time.Duration
	ticker    func(d time.Duration) (<-chan time.Time, func())
	log       *slog.Logger
	obs       Observer
	transport *http.Transport
}

// New returns a Server.
func New(cfg Config) *Server {
	s := &Server{svc: cfg.Service, dial: cfg.Dial, idle: cfg.IdleTimeout, log: cfg.Log, obs: cfg.Observer}
	if s.dial == nil {
		d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		s.dial = d.DialContext
	}
	if s.idle <= 0 {
		s.idle = DefaultIdleTimeout
	}
	s.recheck, s.ticker = cfg.RecheckEvery, cfg.NewTicker
	if s.recheck <= 0 {
		s.recheck = DefaultRecheckEvery
	}
	if s.ticker == nil {
		s.ticker = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.transport = &http.Transport{DialContext: s.dial, MaxIdleConnsPerHost: 32, IdleConnTimeout: 90 * time.Second,
		DisableCompression: true, ResponseHeaderTimeout: 2 * time.Minute}
	return s
}

// Handler returns the handler.
func (s *Server) Handler() http.Handler { return s }

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "req_" + base64.RawURLEncoding.EncodeToString(b)
}

// navigation is browser navigation: Accept text/html and Sec-Fetch-Mode navigate.
func navigation(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html") && strings.EqualFold(r.Header.Get("Sec-Fetch-Mode"), "navigate")
}

func (s *Server) request(r *http.Request, id string) edge.Request {
	var cookie string
	if c, err := r.Cookie(edge.CookieName); err == nil {
		cookie = c.Value
	}
	var signin string
	if c, err := r.Cookie(edge.SigninCookieName); err == nil {
		signin = c.Value
	}
	return edge.Request{Method: r.Method, Host: r.Host, Path: r.URL.Path, Query: r.URL.RawQuery, Cookie: cookie, SigninCookie: signin,
		Origin: r.Header.Get("Origin"), SecFetchSite: r.Header.Get("Sec-Fetch-Site"),
		EventStream:       strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream"),
		XAfsAuthorization: r.Header.Get("X-Afs-Authorization"), Navigate: navigation(r), ContentLength: r.ContentLength,
		ContentEncoding: r.Header.Get("Content-Encoding"), ContentType: r.Header.Get("Content-Type"), Upgrade: isUpgrade(r),
		RemoteAddr: r.RemoteAddr, RequestID: id}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	started := time.Now()
	id := newRequestID()
	w := &statusWriter{ResponseWriter: rw}
	res := s.svc.Handle(r.Context(), s.request(r, id))
	switch res.Action {
	case edge.Redirect:
		s.redirect(w, res, id)
	case edge.Respond:
		writeError(w, r, res, id, navigation(r))
	default:
		s.forward(w, r, res, id)
	}
	if s.obs != nil {
		s.obs.Request(res.Class, w.status, time.Since(started))
	}
}

func (s *Server) redirect(w http.ResponseWriter, res edge.Result, id string) {
	h := w.Header()
	securityHeaders(h)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Request-Id", id)
	h.Set("Location", res.Location)
	switch {
	case res.SetCookie:
		http.SetCookie(w, edgesession.Cookie(edge.CookieName, res.Cookie, res.CookieMaxAge))
	case res.ClearCookie:
		http.SetCookie(w, edgesession.ClearCookie(edge.CookieName))
	}
	switch {
	case res.SetSigninCookie:
		http.SetCookie(w, edgesession.Cookie(edge.SigninCookieName, res.SigninCookie, edge.SigninCookieTTL))
	case res.ClearSigninCookie:
		http.SetCookie(w, edgesession.ClearCookie(edge.SigninCookieName))
	}
	w.WriteHeader(http.StatusFound)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, res edge.Result, _ int) {
	out := res
	out.Action, out.Status, out.Code, out.Reason, out.RetryAfter = edge.Respond, 503, "Unavailable", "upstream_unavailable", 30
	writeError(w, r, out, w.Header().Get("X-Request-Id"), navigation(r))
}

// stripCookie removes the AFS session cookie from Cookie headers; other cookies belong to Paperclip.
func stripCookie(h http.Header) {
	lines := h.Values("Cookie")
	if len(lines) == 0 {
		return
	}
	var keep []string
	for _, line := range lines {
		for _, c := range strings.Split(line, ";") {
			c = strings.TrimSpace(c)
			if c != "" && !strings.HasPrefix(c, edge.CookieName+"=") && !strings.HasPrefix(c, edge.SigninCookieName+"=") {
				keep = append(keep, c)
			}
		}
	}
	if len(keep) == 0 {
		h.Del("Cookie")
		return
	}
	h.Set("Cookie", strings.Join(keep, "; "))
}

// sanitize applies the header policy to the outbound request (API contract 4.4c): remove the AFS cookie,
// X-Afs-Authorization, X-Amzn-* and client-sent forwarding headers.
func sanitize(h http.Header) {
	stripCookie(h)
	h.Del("X-Afs-Authorization")
	// A method override would turn a request the edge decided as one method into another (finding 11), and
	// a negotiated compression extension would hide WebSocket frames from the scan (finding 3).
	for _, k := range []string{"X-Http-Method-Override", "X-Http-Method", "X-Method-Override", "Sec-Websocket-Extensions"} {
		h.Del(k)
	}
	for k := range h {
		l := strings.ToLower(k)
		if strings.HasPrefix(l, "x-amzn-") || strings.HasPrefix(l, "x-forwarded-") || l == "forwarded" || l == "x-real-ip" {
			delete(h, k)
		}
	}
}

func clientIP(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func (s *Server) forward(w *statusWriter, r *http.Request, res edge.Result, id string) {
	w.Header().Set("X-Request-Id", id)
	if res.ScanBody && r.Body != nil && r.Body != http.NoBody {
		body, err := io.ReadAll(io.LimitReader(r.Body, res.MaxBody+1))
		if err != nil {
			writeError(w, r, edge.Result{Status: 400, Code: "ValidationError", Reason: "malformed_credential", Message: "The request body could not be read."}, id, false)
			return
		}
		if int64(len(body)) > res.MaxBody {
			writeError(w, r, edge.Result{Status: 400, Code: "ValidationError", Reason: "body_too_large"}, id, navigation(r))
			return
		}
		matches, readable := collab.ScanBody(body, r.Header.Get("Content-Type"))
		if !readable {
			writeError(w, r, s.svc.OnUnreadable(res), id, false)
			return
		}
		if out, blocked := s.svc.OnScan(r.Context(), res, matches); blocked {
			writeError(w, r, out, id, false)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	} else if res.MaxBody > 0 && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, res.MaxBody)
	}
	target, err := url.Parse(res.Upstream)
	if err != nil {
		s.fail(w, r, res, 503)
		return
	}
	if isUpgrade(r) {
		out := r.Clone(r.Context())
		out.RequestURI, out.Body, out.Close = "", nil, false
		out.URL = &url.URL{Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
		out.Host = r.Host
		sanitize(out.Header)
		out.Header.Set("X-Forwarded-For", clientIP(r.RemoteAddr))
		out.Header.Set("X-Forwarded-Proto", "https")
		out.Header.Set("X-Request-Id", id)
		s.tunnel(w, r, res, out, s.request(r, id))
		return
	}
	rq := s.request(r, id)
	proxy := &httputil.ReverseProxy{
		Transport:     s.transport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			sanitize(pr.Out.Header)
			pr.Out.Header.Set("X-Forwarded-For", clientIP(pr.In.RemoteAddr))
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			pr.Out.Header.Set("X-Request-Id", id)
		},
		ModifyResponse: func(resp *http.Response) error {
			securityHeaders(resp.Header)
			resp.Header.Set("X-Request-Id", id)
			for k := range resp.Header {
				if strings.HasPrefix(strings.ToLower(k), "access-control-") {
					delete(resp.Header, k)
				}
			}
			scrubSetCookies(resp.Header)
			if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
				resp.Body = s.watchBody(r.Context(), newIdleBody(resp.Body, s.idle), rq, res)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var mb *http.MaxBytesError
			if errors.As(err, &mb) {
				writeError(w, r, edge.Result{Status: 400, Code: "ValidationError", Reason: "body_too_large"}, id, false)
				return
			}
			if r.Context().Err() == nil {
				s.log.Warn("upstream request failed", "afs.team_id", res.Route.TeamID, "err", err)
			}
			s.fail(w, r, res, 503)
		},
	}
	proxy.ServeHTTP(w, r)
}

// idleBody closes an event stream that has produced nothing for d.
type idleBody struct {
	io.ReadCloser
	d     time.Duration
	mu    sync.Mutex
	timer *time.Timer
}

func newIdleBody(rc io.ReadCloser, d time.Duration) *idleBody {
	b := &idleBody{ReadCloser: rc, d: d}
	b.timer = time.AfterFunc(d, func() { _ = rc.Close() })
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	b.timer.Reset(b.d)
	b.mu.Unlock()
	return n, err
}

func (b *idleBody) Close() error {
	b.mu.Lock()
	b.timer.Stop()
	b.mu.Unlock()
	return b.ReadCloser.Close()
}

// scrubSetCookies keeps Paperclip's cookies host-only and away from ours: the Domain attribute is removed
// (a cookie for the whole base domain would reach every other team's host) and any cookie named like an AFS
// cookie is dropped (finding 10).
func scrubSetCookies(h http.Header) {
	vals := h.Values("Set-Cookie")
	if len(vals) == 0 {
		return
	}
	h.Del("Set-Cookie")
	for _, v := range vals {
		parts := strings.Split(v, ";")
		name := strings.ToLower(strings.TrimSpace(strings.SplitN(parts[0], "=", 2)[0]))
		if strings.HasPrefix(name, "__host-afs") {
			continue
		}
		keep := []string{parts[0]}
		for _, p := range parts[1:] {
			if a := strings.ToLower(strings.TrimSpace(p)); a != "domain" && !strings.HasPrefix(a, "domain=") {
				keep = append(keep, p)
			}
		}
		h.Add("Set-Cookie", strings.Join(keep, ";"))
	}
}

// stillAllowed reports whether an open stream may go on: the request is still forwarded, with the same
// right to send and the same scan mode it started with.
func (s *Server) stillAllowed(ctx context.Context, rq edge.Request, res edge.Result) bool {
	out := s.svc.Recheck(ctx, rq)
	return out.Action == edge.Forward && s.svc.FrameAllowed(out) == s.svc.FrameAllowed(res) && out.ScanMode == res.ScanMode
}

// watch runs the periodic re-authorization until stop is closed; kill ends the stream when it fails.
func (s *Server) watch(ctx context.Context, rq edge.Request, res edge.Result, stop <-chan struct{}, kill func()) {
	ticks, stopTicker := s.ticker(s.recheck)
	defer stopTicker()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticks:
			if !s.stillAllowed(ctx, rq, res) {
				kill()
				return
			}
		}
	}
}

// recheckedBody ends an event stream when its re-authorization fails.
type recheckedBody struct {
	io.ReadCloser
	stop chan struct{}
	once sync.Once
}

func (b *recheckedBody) Close() error {
	b.once.Do(func() { close(b.stop) })
	return b.ReadCloser.Close()
}

func (s *Server) watchBody(ctx context.Context, body io.ReadCloser, rq edge.Request, res edge.Result) io.ReadCloser {
	b := &recheckedBody{ReadCloser: body, stop: make(chan struct{})}
	go s.watch(ctx, rq, res, b.stop, func() { _ = body.Close() })
	return b
}

// Package gatewayhttp is the HTTP face of afs-gateway: the Bedrock Runtime and Anthropic Messages wire
// formats of gateway contract §2, the error bodies of §7 and the version header of §9. It parses a
// request, hands the call to the gateway use case with a function that forwards it, and writes the
// answer. It knows nothing about budgets: spend rules live in usecase/gateway.
package gatewayhttp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// VersionHeader carries the gateway contract version on every response (§9).
const VersionHeader = "X-Afs-Gateway-Contract-Version"

// TokenHeader is the optional token a runtime may send (§3).
const TokenHeader = "X-Afs-Gateway-Token" //nolint:gosec // a header name, not a credential

// Upstream forwards a call to Bedrock Runtime (aws/bedrock.Runtime).
type Upstream interface {
	Do(ctx context.Context, c bedrock.Call) (*bedrock.Response, error)
	Open(ctx context.Context, c bedrock.Call) (*bedrock.Stream, error)
}

// Observer receives what the gateway measures. It may be nil.
type Observer interface {
	// Done is called once per model call with the status sent and the latency the gateway added, which
	// excludes the time spent in Bedrock.
	Done(op string, status int, added time.Duration)
	// ReserveFailed is called when a call is refused because the gateway could not reserve spend.
	ReserveFailed(reason string)
}

// Config configures a Server.
type Config struct {
	Service  *gateway.Service
	Upstream Upstream
	// MaxBody is the largest accepted request body; default gateway.MaxBodyBytes (20 MB).
	MaxBody  int64
	Log      *slog.Logger
	Observer Observer
	// Routes forwards model route calls. When set (a team gateway, --team-namespace) the route family of
	// gateway contract §12.2 is mounted.
	Routes RouteUpstream
	// Families are mounted on the listener's Mux after the gateway's own, for example a later MCP family
	// at /mcp. A family may not claim a path another one holds.
	Families []Family
}

// Server is the gateway's http.Handler.
type Server struct {
	svc      *gateway.Service
	up       Upstream
	maxBody  int64
	log      *slog.Logger
	obs      Observer
	routes   RouteUpstream
	families []Family
}

// New returns a Server.
func New(cfg Config) *Server {
	if cfg.MaxBody <= 0 {
		cfg.MaxBody = gateway.MaxBodyBytes
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Server{svc: cfg.Service, up: cfg.Upstream, maxBody: cfg.MaxBody, log: cfg.Log, obs: cfg.Observer, routes: cfg.Routes, families: cfg.Families}
}

// Handler returns the handler to serve: a Mux whose fallback is the Bedrock and Anthropic family of
// gateway contract §2 (it answers the contract's 404 for anything it does not serve), with the route
// family of §12.2 on a team gateway and any extra families of the configuration.
func (s *Server) Handler() http.Handler {
	return s.Mux()
}

// Mux returns the listener's router, so the composition root can mount further families on it.
func (s *Server) Mux() *Mux {
	m := NewMux(s.wrap(s.serveCore))
	if s.routes != nil {
		s.RoutesFamily().Mount(m)
	}
	for _, f := range s.families {
		f.Mount(m)
	}
	return m
}

// wrap gives a family handler the gateway's response headers, request id and panic recovery.
func (s *Server) wrap(h func(rw *respWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rw := &respWriter{ResponseWriter: w, requestID: newRequestID(r)}
		hd := rw.Header()
		hd.Set(VersionHeader, gateway.ContractVersion)
		hd.Set("X-Amzn-Requestid", rw.requestID)
		hd.Set("Cache-Control", "no-store")
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving a model call", "requestId", rw.requestID, "panic", rec, "stack", string(debug.Stack()))
				if !rw.started {
					rw.writeProblem(domain.Internal(errors.New("panic")), 0, false)
				}
			}
		}()
		h(rw, r)
	})
}

// respWriter tracks whether the response has begun, which decides how a late error is reported.
type respWriter struct {
	http.ResponseWriter
	requestID string
	started   bool
	status    int
}

func (rw *respWriter) WriteHeader(code int) {
	if rw.started {
		return
	}
	rw.started, rw.status = true, code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *respWriter) Write(b []byte) (int, error) {
	if !rw.started {
		rw.WriteHeader(http.StatusOK)
	}
	return rw.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer for Flush.
func (rw *respWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

func (rw *respWriter) flush() {
	_ = http.NewResponseController(rw.ResponseWriter).Flush()
}

var requestIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// newRequestID returns the caller's X-Request-Id when it is a short plain token, else a random one.
func newRequestID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); requestIDRE.MatchString(v) {
		return v
	}
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "gw-" + hex.EncodeToString(b[:])
}

// route is a parsed path.
type route struct {
	health    bool
	anthropic bool
	// chat is POST /r/bedrock/v1/chat/completions (gateway contract §12.2).
	chat    bool
	op      gateway.Operation
	rtOp    bedrock.Operation
	modelID string
}

// operations of /model/{modelId}/{operation}.
var modelOps = map[string]struct {
	op   gateway.Operation
	rtOp bedrock.Operation
}{
	"converse":                    {gateway.OpConverse, bedrock.OpConverse},
	"converse-stream":             {gateway.OpConverseStream, bedrock.OpConverseStream},
	"invoke":                      {gateway.OpInvoke, bedrock.OpInvoke},
	"invoke-with-response-stream": {gateway.OpInvokeStream, bedrock.OpInvokeWithResponseStream},
}

// parsePath routes by the escaped path; ok is false for anything the gateway does not serve.
func parsePath(method, escaped string) (route, bool) {
	switch escaped {
	case "/healthz":
		return route{health: true}, method == http.MethodGet || method == http.MethodHead
	case "/anthropic/v1/messages":
		return route{anthropic: true}, method == http.MethodPost
	}
	rest, ok := strings.CutPrefix(escaped, "/model/")
	if !ok || method != http.MethodPost {
		return route{}, false
	}
	i := strings.LastIndex(rest, "/")
	if i <= 0 {
		return route{}, false
	}
	op, ok := modelOps[rest[i+1:]]
	if !ok {
		return route{}, false
	}
	id, err := pathUnescape(rest[:i])
	if err != nil || id == "" {
		return route{}, false
	}
	return route{op: op.op, rtOp: op.rtOp, modelID: id}, true
}

func (s *Server) serveCore(rw *respWriter, r *http.Request) {
	rt, ok := parsePath(r.Method, r.URL.EscapedPath())
	switch {
	case !ok:
		rw.writeProblem(domain.NotFound("operation"), 0, strings.HasPrefix(r.URL.Path, "/anthropic/"))
	case rt.health:
		rw.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(rw, `{"status":"ok"}`)
	default:
		s.call(rw, r, rt)
	}
}

// sourceIP is the connection's peer address. A forwarded header is never trusted: the gateway is
// reached directly, and the source IP is what identifies the pod (D57).
func sourceIP(r *http.Request) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, err
	}
	return a.Unmap(), nil
}

// readBody reads the body up to the limit: over it is 413, an unreadable one 400.
func (s *Server) readBody(rw *respWriter, r *http.Request, anthropic bool) ([]byte, bool) {
	tooBig := func() ([]byte, bool) {
		rw.writeProblem(badRequest("body", "is larger than the 20 MB limit"), http.StatusRequestEntityTooLarge, anthropic)
		return nil, false
	}
	if r.ContentLength > s.maxBody {
		return tooBig()
	}
	b, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, s.maxBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return tooBig()
		}
		rw.writeProblem(badRequest("body", "could not be read"), 0, anthropic)
		return nil, false
	}
	return b, true
}

// call serves one model call: identify early, read, parse, then Service.Do with the forwarding function.
func (s *Server) call(rw *respWriter, r *http.Request, rt route) {
	start := time.Now()
	ip, err := sourceIP(r)
	if err != nil {
		rw.writeProblem(domain.Unauthenticated(), 0, rt.anthropic)
		return
	}
	ctx := r.Context()
	token := r.Header.Get(TokenHeader)
	if _, err := s.svc.Authenticate(ctx, ip, token); err != nil {
		s.finish(rw, rt, err, start, 0)
		return
	}
	body, ok := s.readBody(rw, r, rt.anthropic)
	if !ok {
		return
	}
	var p *parsed
	switch {
	case rt.anthropic:
		p, err = parseAnthropic(body, r.Header)
	case rt.chat:
		p, err = parseChatBedrock(body)
	case rt.op == gateway.OpConverse || rt.op == gateway.OpConverseStream:
		p, err = parseConverse(rt.rtOp, rt.op, rt.modelID, body)
	default:
		p, err = parseInvoke(rt.rtOp, rt.op, rt.modelID, body)
	}
	if err != nil {
		rw.writeProblem(err, 0, rt.anthropic)
		return
	}
	if rt.chat {
		rt.op = p.op
	}

	var upstream time.Duration
	var out *bedrock.Response
	err = s.svc.Do(ctx, gateway.Request{SourceIP: ip, Token: token, Op: p.op, ModelID: p.modelID, MaxTokens: p.maxTokens,
		InputBytes: len(body), RequestID: rw.requestID},
		func(ctx context.Context, a gateway.Authorized) (gateway.Result, error) {
			t0 := time.Now()
			defer func() { upstream = time.Since(t0) }()
			ub, err := p.upstreamBody(a)
			if err != nil {
				return gateway.Result{}, err
			}
			call := bedrock.Call{Op: p.rtOp, ProfileARN: a.ProfileARN, RoleARN: a.RoleARN, Body: ub}
			if p.stream {
				return s.stream(ctx, rw, p, call)
			}
			resp, err := s.up.Do(ctx, call)
			if err != nil {
				return gateway.Result{}, err
			}
			out = resp
			u, reported := usageFromResponse(resp.Header, resp.Body)
			return gateway.Result{Usage: u, UsageReported: reported, Started: true}, nil
		})
	if err != nil {
		s.finish(rw, rt, err, start, upstream)
		return
	}
	switch {
	case out != nil && p.wire == wireChat:
		writeChatComplete(rw, out, p.modelID)
	case out != nil:
		writeComplete(rw, out)
	}
	s.done(rt, rw.status, start, upstream)
}

// writeComplete sends a complete Bedrock response unchanged, with the headers an SDK reads.
func writeComplete(rw *respWriter, out *bedrock.Response) {
	h := rw.Header()
	h.Set("Content-Type", firstNonEmpty(out.Header.Get("Content-Type"), "application/json"))
	for _, k := range []string{"X-Amzn-Bedrock-Input-Token-Count", "X-Amzn-Bedrock-Output-Token-Count"} {
		if v := out.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	rw.WriteHeader(http.StatusOK)
	_, _ = rw.Write(out.Body)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// finish reports a failed call: a JSON error when nothing was sent yet, nothing when the client left, and
// after a stream began the stream itself carried the error.
func (s *Server) finish(rw *respWriter, rt route, err error, start time.Time, upstream time.Duration) {
	switch {
	case clientGone(err):
		s.done(rt, 499, start, upstream)
		return
	case !rw.started:
		rw.writeProblem(err, 0, rt.anthropic)
	}
	if domain.KindOf(err) == domain.KindUnavailable && s.obs != nil {
		s.obs.ReserveFailed("unavailable")
	}
	if k := domain.KindOf(err); k == domain.KindInternal {
		s.log.Error("model call failed", "requestId", rw.requestID, "err", err)
	}
	s.done(rt, rw.status, start, upstream)
}

func (s *Server) done(rt route, status int, start time.Time, upstream time.Duration) {
	if s.obs == nil {
		return
	}
	added := time.Since(start) - upstream
	if added < 0 {
		added = 0
	}
	op := string(rt.op)
	if rt.anthropic {
		op = string(gateway.OpAnthropicMessages)
	}
	s.obs.Done(op, status, added)
}

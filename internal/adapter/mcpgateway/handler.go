package mcpgateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpwire"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/mcp"
)

// Aggregated is the server name of the aggregated endpoint.
const Aggregated = "afs"

// Config configures a Handler.
type Config struct {
	Identify  Identify
	Admission Admission
	Upstream  Upstream
	Ledger    Ledger
	Sealer    *mcpwire.Sealer // session ids and list cursors
	Version   string          // gateway version, for serverInfo
	Defaults  Limits          // from mcp.limits
	// SearchLimitMax caps afs_search's limit (mcp.search_limit_max); 0 means mcp.MaxSearchLimit.
	SearchLimitMax int
	// Boost, when set, returns per-server multipliers from the caller's recent use.
	Boost func(ctx context.Context, c Caller) map[string]float64
	Log   *slog.Logger
	Now   func() time.Time
}

// Handler serves /mcp and /mcp/{server}.
type Handler struct {
	cfg     Config
	index   *indexCache
	limiter *limiter
}

// New returns a Handler. Identify, Admission, Upstream, Ledger and Sealer are required.
func New(cfg Config) (*Handler, error) {
	if cfg.Identify == nil || cfg.Admission == nil || cfg.Upstream == nil || cfg.Ledger == nil || cfg.Sealer == nil {
		return nil, errors.New("mcpgateway: identify, admission, upstream, ledger and sealer are required")
	}
	d := &cfg.Defaults
	if d.MaxRequestBytes <= 0 {
		d.MaxRequestBytes = 1 << 20
	}
	if d.MaxResponseBytes <= 0 {
		d.MaxResponseBytes = 8 << 20
	}
	if d.Timeout <= 0 {
		d.Timeout = 60 * time.Second
	}
	if d.Concurrency <= 0 {
		d.Concurrency = 8
	}
	if cfg.SearchLimitMax <= 0 || cfg.SearchLimitMax > mcp.MaxSearchLimit {
		cfg.SearchLimitMax = mcp.MaxSearchLimit
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Handler{cfg: cfg, index: newIndexCache(32), limiter: newLimiter(cfg.Now)}, nil
}

// call is the state of one request.
type call struct {
	h       *Handler
	ctx     context.Context
	caller  Caller
	snap    *Snapshot
	server  *Server // nil on the aggregated endpoint
	version string
	reqID   string
	msg     *mcpwire.Message
	ev      Event
	start   time.Time
	session string
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	slug, ok := route(r.URL.Path)
	if !ok {
		notFound(w)
		return
	}
	if r.Header.Get("Origin") != "" {
		writeEnvelope(w, http.StatusForbidden, "AccessDenied", "browser origins are not accepted", "origin_not_allowed")
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent) // sessions are sealed; nothing to end
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	caller, err := h.cfg.Identify(r)
	if err != nil {
		writeEnvelope(w, http.StatusForbidden, "AccessDenied", "caller not identified", "caller_unknown")
		return
	}
	snap, err := h.cfg.Admission.Snapshot(r.Context(), caller)
	if err != nil {
		// Admission fails closed.
		writeEnvelope(w, http.StatusServiceUnavailable, "Unavailable", "admission unavailable", "admission_unavailable")
		return
	}
	c := &call{h: h, ctx: r.Context(), caller: caller, snap: snap, start: h.cfg.Now(), reqID: newID()}
	limit := h.cfg.Defaults.MaxRequestBytes
	if slug != "" {
		for i := range snap.Servers {
			if snap.Servers[i].Slug == slug {
				c.server = &snap.Servers[i]
			}
		}
		if c.server == nil {
			notFound(w)
			return
		}
		if c.server.Limits.MaxRequestBytes > 0 {
			limit = c.server.Limits.MaxRequestBytes
		}
	}
	c.ev = Event{RequestID: c.reqID, Caller: caller, ServerKind: Aggregated}
	if c.server != nil {
		c.ev.ServerID, c.ev.ServerKind = c.server.ID, c.server.Kind
	}
	defer func() {
		c.ev.At = c.start
		c.ev.GatewayMS = int(h.cfg.Now().Sub(c.start).Milliseconds())
		h.cfg.Ledger.Record(c.ev)
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	c.ev.RequestBytes = len(body)
	if err != nil {
		c.refuse("request_too_large")
		c.ev.Method = r.Header.Get(mcpwire.HeaderMethod)
		c.reply(w, http.StatusRequestEntityTooLarge, mcpwire.NewError(nil, mcpwire.CodeInvalidRequest, "request too large", reasonData("request_too_large", c.reqID)))
		return
	}
	m, perr := mcpwire.Parse(body)
	if perr != nil {
		c.ev.Outcome = OutcomeProtocolError
		c.reply(w, http.StatusBadRequest, &mcpwire.Message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: perr})
		return
	}
	c.msg = m
	c.ev.Method, c.ev.Name = m.Method, mcpwire.NameOf(m.Method, m.Params)
	if herr := mcpwire.CheckHeaders(r.Header, m); herr != nil {
		c.refuse("header_mismatch")
		c.reply(w, http.StatusBadRequest, &mcpwire.Message{JSONRPC: "2.0", ID: idOrNull(m.ID), Error: herr})
		return
	}
	c.version = mcpwire.Version(r.Header)
	c.ev.Protocol = c.version
	if !mcpwire.IsSupported(c.version) {
		c.ev.Outcome = OutcomeProtocolError
		c.reply(w, http.StatusBadRequest, mcpwire.NewError(m.ID, mcpwire.CodeInvalidParams, "unsupported protocol version",
			map[string]any{"supported": mcpwire.Supported}))
		return
	}
	if !m.IsRequest() {
		c.ev.Outcome = OutcomeOK
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if c.version == mcpwire.Version20251125 && m.Method != "initialize" {
		sid := r.Header.Get(mcpwire.HeaderSessionID)
		if sid == "" {
			c.ev.Outcome = OutcomeProtocolError
			c.reply(w, http.StatusBadRequest, mcpwire.NewError(m.ID, mcpwire.CodeInvalidRequest, "Mcp-Session-Id is required", nil))
			return
		}
		if _, err := h.cfg.Sealer.Open(sid, caller.SubjectID); err != nil {
			c.ev.Outcome = OutcomeProtocolError
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}
	status, resp := c.dispatch()
	if c.session != "" {
		w.Header().Set(mcpwire.HeaderSessionID, c.session)
	}
	c.reply(w, status, resp)
}

// route returns the server slug of a path: "" for /mcp, the slug for /mcp/{slug}.
func route(p string) (string, bool) {
	switch {
	case p == "/mcp" || p == "/mcp/":
		return "", true
	case strings.HasPrefix(p, "/mcp/"):
		s := strings.TrimPrefix(p, "/mcp/")
		if mcp.ValidateSlug(s) != nil {
			return "", false
		}
		return s, true
	}
	return "", false
}

func (c *call) dispatch() (int, *mcpwire.Message) {
	m := c.msg
	switch m.Method {
	case "initialize":
		return c.initialize()
	case "server/discover":
		return c.ok(c.serverInfo(true))
	case "ping":
		return c.ok(struct{}{})
	case "tools/list":
		return c.list(mcp.KindTool, "tools")
	case "resources/list":
		return c.list(mcp.KindResource, "resources")
	case "resources/templates/list":
		return c.list(mcp.KindResourceTemplate, "resourceTemplates")
	case "prompts/list":
		return c.list(mcp.KindPrompt, "prompts")
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.Name == "" {
			return c.protocolError(mcpwire.CodeInvalidParams, "params.name is required")
		}
		if c.server == nil {
			switch p.Name {
			case "afs_search":
				return c.search(p.Arguments)
			case "afs_describe":
				return c.describe(p.Arguments)
			case "afs_call":
				return c.metaCall(p.Arguments)
			}
		}
		return c.forward(mcp.KindTool, p.Name, "name", m.Params)
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.URI == "" {
			return c.protocolError(mcpwire.CodeInvalidParams, "params.uri is required")
		}
		return c.forward(mcp.KindResource, p.URI, "uri", m.Params)
	case "prompts/get":
		var p struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(m.Params, &p) != nil || p.Name == "" {
			return c.protocolError(mcpwire.CodeInvalidParams, "params.name is required")
		}
		return c.forward(mcp.KindPrompt, p.Name, "name", m.Params)
	}
	c.ev.Outcome = OutcomeProtocolError
	return http.StatusOK, mcpwire.NewError(m.ID, mcpwire.CodeMethodNotFound, "method not found: "+m.Method, nil)
}

func (c *call) initialize() (int, *mcpwire.Message) {
	var p struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
	}
	_ = json.Unmarshal(c.msg.Params, &p)
	v := mcpwire.Version20251125
	if mcpwire.IsSupported(p.ProtocolVersion) {
		v = p.ProtocolVersion
	}
	sid, err := c.h.cfg.Sealer.Seal(mcpwire.Session{Version: v, Subject: c.caller.SubjectID, Capabilities: p.Capabilities,
		Expires: c.h.cfg.Now().Add(24 * time.Hour).Unix()})
	if err != nil {
		return c.protocolError(mcpwire.CodeInternalError, "session")
	}
	c.session = sid
	info := c.serverInfo(false)
	info["protocolVersion"] = v
	return c.ok(info)
}

func (c *call) serverInfo(discover bool) map[string]any {
	name := Aggregated
	if c.server != nil {
		name = c.server.Slug
	}
	out := map[string]any{
		"serverInfo": map[string]string{"name": name, "version": c.h.cfg.Version},
		"capabilities": map[string]any{
			"tools":     map[string]bool{"listChanged": true},
			"resources": map[string]bool{"listChanged": true, "subscribe": false},
			"prompts":   map[string]bool{"listChanged": true},
		},
	}
	if discover {
		out["supportedVersions"] = mcpwire.Supported
	}
	if c.server == nil && c.snap.Mode != "full" {
		out["instructions"] = "Find tools with afs_search, read their schemas with afs_describe, and run them with afs_call or by calling <server>__<tool> directly."
	}
	return out
}

// visible returns the allowed entries of one kind for this endpoint, in qualified-name order.
func (c *call) visible(kind mcp.Kind) []Entry {
	var out []Entry
	for _, e := range c.snap.Entries {
		if !e.Decision.Allowed || e.Primitive.Kind != kind || c.server != nil && e.ServerID != c.server.ID {
			continue
		}
		out = append(out, e)
	}
	sortEntries(out)
	return out
}

const pageSize = 100

func (c *call) list(kind mcp.Kind, field string) (int, *mcpwire.Message) {
	var p struct {
		Cursor string `json:"cursor"`
	}
	_ = json.Unmarshal(c.msg.Params, &p)
	offset := 0
	if p.Cursor != "" {
		s, err := c.h.cfg.Sealer.Open(p.Cursor, c.caller.SubjectID)
		prefix := "cursor:" + c.msg.Method + ":"
		if err != nil || !strings.HasPrefix(s.Version, prefix) {
			return c.protocolError(mcpwire.CodeInvalidParams, "invalid cursor")
		}
		offset, _ = strconv.Atoi(strings.TrimPrefix(s.Version, prefix))
	}
	var defs []json.RawMessage
	if kind == mcp.KindTool && c.server == nil && c.snap.Mode != "full" {
		defs = metaTools()
	} else {
		for _, e := range c.visible(kind) {
			defs = append(defs, c.present(e))
		}
		if kind == mcp.KindTool && c.server == nil {
			defs = append(defs, metaTools()...)
		}
	}
	if offset > len(defs) {
		offset = len(defs)
	}
	end := min(offset+pageSize, len(defs))
	out := map[string]any{field: nonNil(defs[offset:end])}
	if end < len(defs) {
		cur, err := c.h.cfg.Sealer.Seal(mcpwire.Session{Version: "cursor:" + c.msg.Method + ":" + strconv.Itoa(end),
			Subject: c.caller.SubjectID, Expires: c.h.cfg.Now().Add(time.Hour).Unix()})
		if err == nil {
			out["nextCursor"] = cur
		}
	}
	return c.ok(out)
}

func nonNil(d []json.RawMessage) []json.RawMessage {
	if d == nil {
		return []json.RawMessage{}
	}
	return d
}

// present returns an entry's definition as this endpoint shows it: on the aggregated endpoint the
// name (or URI) is qualified with the server slug.
func (c *call) present(e Entry) json.RawMessage {
	if c.server != nil {
		return e.Definition
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(e.Definition, &obj) != nil {
		obj = map[string]json.RawMessage{}
	}
	key := "name"
	switch e.Primitive.Kind {
	case mcp.KindResource:
		key = "uri"
	case mcp.KindResourceTemplate:
		key = "uriTemplate"
	}
	q, _ := json.Marshal(e.Primitive.QualifiedName())
	obj[key] = q
	b, _ := json.Marshal(obj)
	return b
}

// lookup finds the entry an agent names, qualified on the aggregated endpoint. Templates match
// resources/read by URI template expansion being the upstream's job: a read of a URI that is not a
// listed resource is allowed when it matches an allowed template's literal prefix.
func (c *call) lookup(kind mcp.Kind, name string) (*Entry, string, bool) {
	server, plain := "", name
	if c.server == nil {
		s, n, ok := mcp.SplitQualifiedName(name)
		if !ok {
			return nil, "", false
		}
		server, plain = s, n
	} else {
		server = c.server.Slug
	}
	var tmpl *Entry
	for i := range c.snap.Entries {
		e := &c.snap.Entries[i]
		if e.Primitive.Server != server {
			continue
		}
		if e.Primitive.Kind == kind && e.Primitive.Name == plain {
			return e, plain, true
		}
		if kind == mcp.KindResource && e.Primitive.Kind == mcp.KindResourceTemplate && templateMatch(e.Primitive.Name, plain) {
			if tmpl == nil || !tmpl.Decision.Allowed {
				tmpl = e
			}
		}
	}
	if tmpl != nil {
		return tmpl, plain, true
	}
	return nil, plain, false
}

// templateMatch reports whether uri could come from an RFC 6570 template: the literal parts must
// appear in order. It is a filter for admission; the upstream validates the URI itself.
func templateMatch(tmpl, uri string) bool {
	var lits []string
	for {
		i := strings.IndexByte(tmpl, '{')
		if i < 0 {
			lits = append(lits, tmpl)
			break
		}
		lits = append(lits, tmpl[:i])
		j := strings.IndexByte(tmpl[i:], '}')
		if j < 0 {
			return false
		}
		tmpl = tmpl[i+j+1:]
	}
	pattern := strings.Join(lits, "*")
	return mcp.Glob(pattern, uri)
}

func (c *call) serverByID(id string) *Server {
	for i := range c.snap.Servers {
		if c.snap.Servers[i].ID == id {
			return &c.snap.Servers[i]
		}
	}
	return nil
}

// forward admits and sends a call to its upstream. params is rewritten with the plain name.
func (c *call) forward(kind mcp.Kind, name, key string, params json.RawMessage) (int, *mcpwire.Message) {
	e, plain, found := c.lookup(kind, name)
	c.ev.Name = name
	if !found || !e.Decision.Allowed {
		reason := "primitive_unknown"
		if found {
			reason = string(e.Reason)
			c.ev.ServerID = e.ServerID
		}
		c.refuse(reason)
		return http.StatusOK, mcpwire.NewError(c.msg.ID, mcpwire.CodeInvalidParams, "Unknown "+kindWord(kind)+": "+name, nil)
	}
	srv := c.serverByID(e.ServerID)
	if srv == nil {
		c.refuse(string(mcp.ReasonServerNotEnabledForAgent))
		return http.StatusOK, mcpwire.NewError(c.msg.ID, mcpwire.CodeInvalidParams, "Unknown "+kindWord(kind)+": "+name, nil)
	}
	c.ev.ServerID, c.ev.ServerKind, c.ev.Digest = srv.ID, srv.Kind, e.Digest
	c.ev.Name = plain
	key2 := c.caller.SubjectID + "\x00" + e.Primitive.QualifiedName()
	if rl := e.Decision.RateLimit; rl > 0 && !c.h.limiter.allow(key2, rl) {
		c.refuse("rate_limited")
		return http.StatusTooManyRequests, mcpwire.NewError(c.msg.ID, mcpwire.CodeServerError, "rate limited", reasonData("rate_limited", c.reqID))
	}
	conc := srv.Limits.Concurrency
	if conc <= 0 {
		conc = c.h.cfg.Defaults.Concurrency
	}
	release, ok := c.h.limiter.acquire(c.caller.SubjectID+"\x00"+srv.ID, conc)
	if !ok {
		c.refuse("rate_limited")
		return http.StatusTooManyRequests, mcpwire.NewError(c.msg.ID, mcpwire.CodeServerError, "too many concurrent calls", reasonData("rate_limited", c.reqID))
	}
	defer release()

	var obj map[string]json.RawMessage
	_ = json.Unmarshal(params, &obj)
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	pn, _ := json.Marshal(plain)
	obj[key] = pn
	delete(obj, "_meta") // the gateway does not forward the agent's _meta except trace context
	if meta := traceMeta(params); meta != nil {
		obj["_meta"] = meta
	}
	np, _ := json.Marshal(obj)
	up := &mcpwire.Message{JSONRPC: "2.0", ID: c.msg.ID, Method: c.msg.Method, Params: np}
	if e.Primitive.Kind == mcp.KindTool {
		up.Method = "tools/call"
	}

	timeout := srv.Limits.Timeout
	if timeout <= 0 {
		timeout = c.h.cfg.Defaults.Timeout
	}
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	t0 := c.h.cfg.Now()
	resp, err := c.h.cfg.Upstream.Call(ctx, *srv, up)
	c.ev.UpstreamMS = int(c.h.cfg.Now().Sub(t0).Milliseconds())
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		c.ev.Outcome, c.ev.Reason = OutcomeTimeout, "timeout"
		return http.StatusGatewayTimeout, mcpwire.NewError(c.msg.ID, mcpwire.CodeTimeout, "upstream timed out", reasonData("timeout", c.reqID))
	case errors.Is(err, context.Canceled):
		c.ev.Outcome = OutcomeCancelled
		return http.StatusOK, mcpwire.NewError(c.msg.ID, mcpwire.CodeServerError, "cancelled", nil)
	case errors.Is(err, mcpwire.ErrResponseTooLarge):
		c.ev.Outcome, c.ev.Reason = OutcomeUpstreamUnavailable, "response_too_large"
		return http.StatusOK, mcpwire.NewError(c.msg.ID, mcpwire.CodeInternalError, "upstream response too large", reasonData("response_too_large", c.reqID))
	case err != nil:
		var ue *UpstreamError
		if errors.As(err, &ue) && ue.Reason != "" {
			c.ev.Outcome, c.ev.Reason = OutcomeRefused, ue.Reason
			return ue.Status, mcpwire.NewError(c.msg.ID, mcpwire.CodeServerError, ue.Message, reasonData(ue.Reason, c.reqID))
		}
		c.ev.Outcome, c.ev.Reason = OutcomeUpstreamUnavailable, "upstream_unavailable"
		return http.StatusBadGateway, mcpwire.NewError(c.msg.ID, mcpwire.CodeServerError, "upstream unavailable", reasonData("upstream_unavailable", c.reqID))
	}
	resp.ID = c.msg.ID
	if resp.Error != nil {
		c.ev.Outcome, c.ev.JSONRPCCode = OutcomeProtocolError, resp.Error.Code
		return http.StatusOK, resp
	}
	c.ev.Outcome = OutcomeOK
	var isErr struct {
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(resp.Result, &isErr) == nil && isErr.IsError {
		c.ev.Outcome = OutcomeToolError
	}
	resp.Result = withRequestID(resp.Result, c.reqID)
	return http.StatusOK, resp
}

// UpstreamError lets the Upstream adapter return a contract refusal (gateway contract §13.4), for
// example auth_reconnect_needed or federation_not_ready, with its HTTP status.
type UpstreamError struct {
	Status  int
	Reason  string
	Message string
}

func (e *UpstreamError) Error() string { return "mcpgateway: upstream refused: " + e.Reason }

func kindWord(k mcp.Kind) string {
	switch k {
	case mcp.KindResource, mcp.KindResourceTemplate:
		return "resource"
	case mcp.KindPrompt:
		return "prompt"
	}
	return "tool"
}

func traceMeta(params json.RawMessage) json.RawMessage {
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if json.Unmarshal(params, &p) != nil || p.Meta == nil {
		return nil
	}
	out := map[string]json.RawMessage{}
	for _, k := range []string{"traceparent", "tracestate", "progressToken"} {
		if v, ok := p.Meta[k]; ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	b, _ := json.Marshal(out)
	return b
}

func withRequestID(result json.RawMessage, id string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(result, &obj) != nil || obj == nil {
		return result
	}
	meta := map[string]json.RawMessage{}
	if m, ok := obj["_meta"]; ok {
		_ = json.Unmarshal(m, &meta)
	}
	v, _ := json.Marshal(id)
	meta["io.afs/requestId"] = v
	mb, _ := json.Marshal(meta)
	obj["_meta"] = mb
	b, _ := json.Marshal(obj)
	return b
}

func (c *call) refuse(reason string) {
	c.ev.Outcome, c.ev.Reason = OutcomeRefused, reason
}

func (c *call) ok(result any) (int, *mcpwire.Message) {
	m, err := mcpwire.NewResult(c.msg.ID, result)
	if err != nil {
		return c.protocolError(mcpwire.CodeInternalError, "encode")
	}
	if c.ev.Outcome == "" {
		c.ev.Outcome = OutcomeOK
	}
	return http.StatusOK, m
}

func (c *call) protocolError(code int, msg string) (int, *mcpwire.Message) {
	c.ev.Outcome, c.ev.JSONRPCCode = OutcomeProtocolError, code
	return http.StatusOK, mcpwire.NewError(c.msg.ID, code, msg, nil)
}

func (c *call) reply(w http.ResponseWriter, status int, m *mcpwire.Message) {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(m)
	c.ev.HTTPStatus, c.ev.ResponseBytes = status, buf.Len()
	if m.Error != nil && c.ev.JSONRPCCode == 0 {
		c.ev.JSONRPCCode = m.Error.Code
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func reasonData(reason, reqID string) map[string]string {
	return map[string]string{"reason": reason, "requestId": reqID}
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func notFound(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) }

func writeEnvelope(w http.ResponseWriter, status int, code, msg, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg, "details": map[string]string{"reason": reason}})
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// limiter holds per-minute counters and concurrency slots, per replica.
type limiter struct {
	mu    sync.Mutex
	now   func() time.Time
	win   map[string]window
	slots map[string]int
}

type window struct {
	minute int64
	n      int
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, win: map[string]window{}, slots: map[string]int{}}
}

func (l *limiter) allow(key string, perMinute int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.now().Unix() / 60
	w := l.win[key]
	if w.minute != m {
		w = window{minute: m}
	}
	if w.n >= perMinute {
		l.win[key] = w
		return false
	}
	w.n++
	l.win[key] = w
	return true
}

func (l *limiter) acquire(key string, max int) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.slots[key] >= max {
		return nil, false
	}
	l.slots[key]++
	return func() {
		l.mu.Lock()
		l.slots[key]--
		if l.slots[key] == 0 {
			delete(l.slots, key)
		}
		l.mu.Unlock()
	}, true
}

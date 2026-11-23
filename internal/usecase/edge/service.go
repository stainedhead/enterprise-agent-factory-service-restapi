package edge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
)

// CookieName is the host-only session cookie (Secure, HttpOnly, SameSite=Lax, no Domain, Path=/).
const CookieName = "__Host-afs-collab"

// Reserved paths on team hosts (never forwarded) and the apex callback.
const (
	PathSignin   = "/_afs/signin"
	PathHandoff  = "/_afs/handoff"
	PathSignout  = "/_afs/signout"
	PathCallback = "/oauth2/callback"
)

// SigninCookieName is the short-lived host-only cookie that binds a sign-in to the browser that started it
// (finding 8). It holds a random value; only its hash travels in the sealed state and handoff envelope.
const SigninCookieName = "__Host-afs-signin"

// SigninCookieTTL is its lifetime in seconds (the state's lifetime).
const SigninCookieTTL = 600

// GroupReauthAfter is how old a session may get when its access comes only from an IdP group binding
// (RD-24): after it the person signs in again through the provider so group removals take effect.
const GroupReauthAfter = 60 * time.Minute

// Cache lifetimes (EDGE-4): decisions are cached at most 60 s, a negative result 10 s.
const (
	DecisionTTL = 60 * time.Second
	NegativeTTL = 10 * time.Second
	// StateTTL is how long a sign-in may take.
	StateTTL = 10 * time.Minute
	// SecretAlertEvery is the spacing of collaboration.secret_detected per team, person and pattern.
	SecretAlertEvery = 5 * time.Minute
)

// Request is what the HTTP adapter knows about one request. It never carries a body.
type Request struct {
	Method, Host, Path, Query string
	// Cookie is the value of CookieName, "" when absent.
	Cookie string
	// XAfsAuthorization is the header value, "" when absent.
	XAfsAuthorization string
	// Navigate is true for browser navigation (Accept: text/html and Sec-Fetch-Mode: navigate).
	Navigate        bool
	ContentLength   int64
	ContentEncoding string
	ContentType     string
	// Upgrade is true for a WebSocket handshake.
	Upgrade bool
	// EventStream is true when the client asks for text/event-stream (Server-Sent Events).
	EventStream bool
	// Origin and SecFetchSite are the request headers of the same names (CSRF check).
	Origin, SecFetchSite string
	// SigninCookie is the value of SigninCookieName, "" when absent.
	SigninCookie string
	// Recheck marks the periodic re-authorization of an open stream: caches are bypassed and no rate
	// limit or count is charged.
	Recheck    bool
	RemoteAddr string
	RequestID  string
}

// Action is what the adapter does with the request.
type Action int

// Actions.
const (
	// Forward sends the request to Result.Upstream.
	Forward Action = iota
	// Respond answers with Status, Code, Reason, Message and Details.
	Respond
	// Redirect answers 302 to Location, setting or clearing the cookie.
	Redirect
)

// Result is the decision for one request.
type Result struct {
	Action   Action
	Upstream string
	Status   int
	Code     string
	Reason   string
	Message  string
	Required string
	Patterns []string
	// RetryAfter is the Retry-After seconds, 0 for none.
	RetryAfter int
	Location   string
	// Cookie is set when SetCookie is true; ClearCookie expires it.
	Cookie       string
	CookieMaxAge int
	ClearCookie  bool
	SetCookie    bool
	// SigninCookie is set when SetSigninCookie is true (SigninCookieTTL seconds); ClearSigninCookie expires it.
	SigninCookie      string
	SetSigninCookie   bool
	ClearSigninCookie bool

	Route     collab.Route
	Class     collab.Class
	Kind      collab.PrincipalKind
	Principal string
	// Caller is the principal for per-frame decisions on a WebSocket.
	Caller collab.Principal
	// ScanBody tells the adapter to scan the request body; ScanMode is block or warn.
	ScanBody bool
	ScanMode string
	// MaxBody is the largest body the adapter may read, in bytes.
	MaxBody           int64
	ServiceAdminEntry bool
	Outcome           string
}

// Service is the edge use case set.
type Service struct {
	Table      *collab.Table
	Routes     RouteSource
	Principals Principals
	Sessions   Sessions
	Identity   Identity
	Sealer     Sealer
	Access     *collab.AccessBuffer
	Sink       AccessSink
	Alerts     Alerts
	Records    Records
	Limiter    *collab.Limiter
	// CacheSize bounds each decision cache (DefaultCacheSize when 0); UnknownPerMinute bounds the lookups of
	// unknown credentials per source address per minute (DefaultUnknownPerMinute when 0).
	CacheSize, UnknownPerMinute int
	Now                         func() time.Time
	// Rand is the random source of codes and session ids (crypto/rand.Reader in production).
	Rand io.Reader

	mu      sync.Mutex
	people  *lru[string, cached[[]Binding]]
	sess    *lru[[32]byte, cached[Session]]
	keys    *lru[string, cached[KeyInfo]]
	unknown *lru[string, missWindow]
	alerts  map[string]*alertState
}

type cached[T any] struct {
	v   T
	ok  bool
	exp time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) init() {
	if s.people == nil {
		n := s.CacheSize
		if n <= 0 {
			n = DefaultCacheSize
		}
		s.people, s.sess, s.keys = newLRU[string, cached[[]Binding]](n), newLRU[[32]byte, cached[Session]](n), newLRU[string, cached[KeyInfo]](n)
		s.unknown, s.alerts = newLRU[string, missWindow](n), map[string]*alertState{}
	}
}

func respond(status int, code, reason string) Result {
	return Result{Action: Respond, Status: status, Code: code, Reason: reason}
}

// Refresh loads the route table and the settings (start, notification, every 30 s). A failure keeps the
// table, which the lookup stops trusting after collab.MaxStale.
func (s *Service) Refresh(ctx context.Context) error {
	set, err := s.Routes.Settings(ctx)
	if err != nil {
		return fmt.Errorf("edge: settings: %w", err)
	}
	routes, err := s.Routes.Routes(ctx)
	if err != nil {
		return fmt.Errorf("edge: routes: %w", err)
	}
	s.Table.Replace(routes, set)
	return nil
}

// Flush writes the buffered access counts, at most collab.MaxFlushRows per call; a failed write puts the rows
// back for the next flush.
func (s *Service) Flush(ctx context.Context) error {
	for s.Access.Len() > 0 {
		rows := s.Access.Take()
		if len(rows) == 0 {
			return nil
		}
		if err := s.Sink.Record(ctx, rows); err != nil {
			s.Access.Restore(rows)
			return fmt.Errorf("edge: record access: %w", err)
		}
	}
	return nil
}

func (s *Service) record(r Result, outcome string, bytes int64) {
	if r.Route.TeamID == "" {
		return
	}
	kind, id := r.Kind, r.Principal
	if kind == "" {
		kind, id = collab.KindAnonymous, ""
	}
	if len(id) > 64 {
		id = id[:64]
	}
	s.Access.Add(s.now(), collab.AccessKey{TeamID: r.Route.TeamID, Surface: r.Class.Surface(), Exposure: r.Route.ExposureLabel(), Kind: kind, PrincipalID: id,
		Outcome: outcome, ServiceAdminEntry: r.ServiceAdminEntry}, bytes)
}

func (s *Service) event(req Request, r Result, name string, started time.Time) {
	if s.Records == nil || r.Route.TeamID == "" {
		return
	}
	kind, id := string(r.Kind), r.Principal
	if kind == "" {
		kind = string(collab.KindAnonymous)
	}
	s.Records.Write(Event{TeamID: r.Route.TeamID, Event: name, PrincipalKind: kind, PrincipalID: id, Method: req.Method,
		Route: RouteTemplate(req.Path), Status: r.Status, Reason: r.Reason, Duration: s.now().Sub(started),
		ServiceAdminEntry: r.ServiceAdminEntry, RequestID: req.RequestID})
}

// RouteTemplate reduces a path to its first segment, so no id or query ever reaches a record (PC-OQ-4).
func RouteTemplate(path string) string {
	p := strings.TrimPrefix(path, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	return "/" + p
}

func hash(b string) [32]byte { return sha256.Sum256([]byte(b)) }

func (s *Service) random(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(s.Rand, b); err != nil {
		return nil, err
	}
	return b, nil
}

// Handle decides one request.
func (s *Service) Handle(ctx context.Context, req Request) Result {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	started := s.now()
	req.Host = NormalizeHost(req.Host)
	r := s.handle(ctx, req)
	if r.Outcome == "" {
		r.Outcome = collab.OutcomeOf(collab.Decision{Allow: r.Action == Forward, Status: r.Status, Code: r.Code, Reason: r.Reason})
	}
	if r.Action != Redirect || r.SetCookie || r.ClearCookie {
		s.record(r, r.Outcome, max(req.ContentLength, 0))
	}
	switch {
	case r.SetCookie:
		s.event(req, r, "sign_in", started)
	case r.ClearCookie:
		s.event(req, r, "sign_out", started)
	case r.Action == Respond && r.Route.TeamID != "":
		s.event(req, r, "denied", started)
	}
	return r
}

// Recheck decides an open WebSocket or event stream again with fresh data (no cache, no rate limit, no
// count). The adapter closes the stream unless the result is Forward with the same permissions.
func (s *Service) Recheck(ctx context.Context, req Request) Result {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	req.Recheck, req.Host = true, NormalizeHost(req.Host)
	return s.handle(ctx, req)
}

// crossOrigin reports a cookie-authenticated request that a page on another origin could have caused:
// every non-safe method, WebSocket upgrade and event stream must carry Origin equal to this team host, or,
// when Origin is absent and the request is not an upgrade, Sec-Fetch-Site: same-origin (finding 1).
func crossOrigin(req Request) bool {
	if collab.Safe(req.Method) && !req.Upgrade && !req.EventStream {
		return false
	}
	if req.Origin != "" {
		return req.Origin != "https://"+req.Host
	}
	if req.Upgrade {
		return true
	}
	return !strings.EqualFold(req.SecFetchSite, "same-origin")
}

func (s *Service) handle(ctx context.Context, req Request) Result {
	if !s.Table.Fresh() {
		return respond(503, "Unavailable", "upstream_unavailable").withRetry(30)
	}
	route, kind := s.Table.Lookup(req.Host)
	switch kind {
	case collab.HostUnknown:
		return respond(404, "NotFound", "")
	case collab.HostApex:
		if req.Method == "GET" && req.Path == PathCallback {
			return s.callback(ctx, req)
		}
		return respond(404, "NotFound", "")
	}
	if !route.WebUIEffective && !route.RESTAPIEffective {
		return Result{Action: Respond, Status: 404, Code: "NotFound", Route: route}
	}
	switch {
	case req.Path == PathSignin && req.Method == "GET":
		return s.signin(ctx, req, route)
	case req.Path == PathHandoff && req.Method == "GET":
		return s.handoff(ctx, req, route)
	case req.Path == PathSignout && req.Method == "POST":
		if req.Cookie != "" && crossOrigin(req) {
			return Result{Route: route, Class: collab.ClassWeb}.deny(403, "AccessDenied", "cross_origin")
		}
		return s.signout(ctx, req, route)
	case strings.HasPrefix(req.Path, "/_afs/"):
		return Result{Action: Respond, Status: 404, Code: "NotFound", Route: route}
	}
	return s.proxy(ctx, req, route)
}

func (r Result) withRetry(n int) Result { r.RetryAfter = n; return r }

// identity is who the credentials say the caller is.
type identity struct {
	kind     collab.PrincipalKind
	id       string
	bindings []Binding
	// reason and status describe a credential that cannot be used.
	status int
	code   string
	reason string
	// reauth is true when a session ended only because its access comes from a group binding and it is
	// older than GroupReauthAfter (RD-24): the sign-in redirect asks the provider silently.
	reauth bool
}

func (s *Service) proxy(ctx context.Context, req Request, route collab.Route) Result {
	base := Result{Route: route}
	hasCookie, hasAPI := req.Cookie != "", req.XAfsAuthorization != ""
	if hasCookie && hasAPI {
		base.Class = collab.ClassWeb
		return base.deny(400, "ValidationError", "ambiguous_credentials")
	}
	base.Class = collab.ClassAPI
	if hasCookie || (!hasAPI && req.Navigate) {
		base.Class = collab.ClassWeb
	}
	if hasCookie && crossOrigin(req) {
		return base.deny(403, "AccessDenied", "cross_origin")
	}
	if !route.Effective(base.Class.Surface()) {
		// With exactly one surface not effective the caller needs a binding to learn that (PC-D31): resolve first.
		return s.refusedSurface(ctx, req, base)
	}
	id := s.identify(ctx, req, route, base.Class)
	base.Kind, base.Principal = id.kind, id.id
	if id.status != 0 {
		if base.Class == collab.ClassWeb && req.Navigate && id.reason != "malformed_credential" && id.status != 429 {
			return s.signinRedirect(req, base, id.reauth)
		}
		return base.deny(id.status, id.code, id.reason)
	}
	p, sae := s.principal(route.TeamID, id)
	d := collab.Decide(route, base.Class, req.Method, p)
	if !d.Allow {
		return base.denyDecision(d)
	}
	base.ServiceAdminEntry = sae(req.Method, base.Class)
	base.Caller = p
	if methodOverride(req.Query) && (!p.Has(collab.PermContribute) || collab.Safe(req.Method)) {
		r := base.deny(403, "AccessDenied", "permission_required")
		r.Required = collab.PermContribute
		return r
	}
	if ok, wait := s.Limiter.Allow(route.TeamID + "|" + string(id.kind) + "|" + id.id); !ok && !req.Recheck {
		r := base.deny(429, "Throttled", "")
		r.RetryAfter = wait
		return r
	}
	set := s.Table.Settings()
	if !collab.Safe(req.Method) && !req.Upgrade {
		if req.ContentLength > set.MaxBodyBytes() {
			return base.deny(400, "ValidationError", "body_too_large")
		}
		if set.ContentScan != "off" && req.ContentLength != 0 && req.ContentEncoding != "" && !strings.EqualFold(req.ContentEncoding, "identity") {
			return base.deny(400, "ValidationError", "unsupported_encoding")
		}
	}
	base.Action, base.Upstream, base.Status, base.MaxBody = Forward, route.Upstream(), 200, set.MaxBodyBytes()
	if set.ContentScan != "off" && p.Has(collab.PermContribute) {
		switch {
		case req.Upgrade:
			// Text frames of a WebSocket are scanned by the adapter; ScanBody stays false (no request body).
			base.ScanMode = set.ContentScan
		case !collab.Safe(req.Method) && req.ContentLength != 0:
			switch collab.ClassifyBody(mediaType(req.ContentType)) {
			case collab.BodyScan:
				base.ScanBody, base.ScanMode = true, set.ContentScan
			case collab.BodyUnsupported:
				if set.ContentScan == "block" {
					// A body the scanner cannot read could hide a credential (finding 4, RD-25).
					return base.unsupportedMedia()
				}
			}
		}
	}
	return base
}

func (r Result) unsupportedMedia() Result {
	r = r.deny(415, "ValidationError", "unsupported_media_type")
	r.Outcome = collab.OutcomeDeniedBad
	return r
}

// OnUnreadable answers a body the scanner could not decode (an unknown charset, a malformed multipart).
func (s *Service) OnUnreadable(r Result) Result {
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	out := r.unsupportedMedia()
	s.record(out, collab.OutcomeDeniedBad, 0)
	return out
}

// methodOverride reports a "_method" query parameter, which some frameworks treat as the HTTP method.
func methodOverride(query string) bool {
	q, _ := url.ParseQuery(query)
	for k := range q {
		if strings.EqualFold(k, "_method") {
			return true
		}
	}
	return false
}

func mediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

func (r Result) deny(status int, code, reason string) Result {
	r.Action, r.Status, r.Code, r.Reason = Respond, status, code, reason
	return r
}

func (r Result) denyDecision(d collab.Decision) Result {
	r = r.deny(d.Status, d.Code, d.Reason)
	r.Required, r.RetryAfter = d.Required, d.RetryAfter
	if d.Status == 404 {
		r.Reason = ""
	}
	return r
}

// refusedSurface answers a request for a surface that is not effective: 403 surface_disabled for a caller
// who holds a binding on the team, 404 (no detail) for everyone else.
func (s *Service) refusedSurface(ctx context.Context, req Request, base Result) Result {
	if base.Route.OtherLevel(base.Class.Surface()) {
		// Effective at the other level only: this hostname gives nothing away (CD-38).
		return base.deny(404, "NotFound", "")
	}
	id := s.identify(ctx, req, base.Route, base.Class)
	base.Kind, base.Principal = id.kind, id.id
	if id.status == 0 {
		if p, _ := s.principal(base.Route.TeamID, id); p.HasBinding {
			return base.deny(403, "AccessDenied", "surface_disabled")
		}
	}
	return base.deny(404, "NotFound", "")
}

// signinRedirect sends the browser to sign in. It is not counted: the contract's outcome list has no value
// for it and denied_unauthenticated feeds the denied-spike alert, which a person simply opening a link must
// not raise (RD-26). silent asks the provider for prompt=none (RD-24).
func (s *Service) signinRedirect(req Request, base Result, silent bool) Result {
	q := url.Values{"return": {safeReturn(req.Path, req.Query)}}
	if silent {
		q.Set("silent", "1")
	}
	base.Action, base.Status, base.Location = Redirect, 302, PathSignin+"?"+q.Encode()
	base.Outcome = collab.OutcomeDeniedUnauth
	return base
}

// principal builds the authz principal and the service-admin-entry test.
func (s *Service) principal(teamID string, id identity) (collab.Principal, func(method string, c collab.Class) bool) {
	ap := authz.Principal{ID: id.id, Type: authz.PrincipalUser}
	if id.kind == collab.KindAPIKey {
		ap.Type = authz.PrincipalAPIKey
	}
	for _, b := range id.bindings {
		switch authz.Role(b.Role) {
		case authz.RoleServiceAdmin:
			ap.ServiceAdmin = true
		case authz.RoleTeamAdmin, authz.RoleTeamViewer, authz.RoleTeamCollaborator:
			ap.Bindings = append(ap.Bindings, authz.Binding{Role: authz.Role(b.Role), TeamID: teamID})
		}
	}
	res := authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}
	has := func(p authz.Principal) func(string) bool {
		return func(perm string) bool { return authz.Authorize(p, authz.Permission(perm), res).Allowed() }
	}
	teamOnly := ap
	teamOnly.ServiceAdmin = false
	cp := collab.Principal{HasBinding: len(ap.Bindings) > 0 || ap.ServiceAdmin, Has: has(ap)}
	sae := func(method string, c collab.Class) bool {
		if !ap.ServiceAdmin {
			return false
		}
		need := collab.PermAccess
		if !collab.Safe(method) {
			need = collab.PermContribute
		}
		return !has(teamOnly)(need) || (c == collab.ClassAPI && !has(teamOnly)(collab.PermAPI))
	}
	return cp, sae
}

func bearer(h string) (string, bool) {
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return "", false
	}
	return strings.TrimSpace(h[len(p):]), true
}

func (s *Service) identify(ctx context.Context, req Request, route collab.Route, class collab.Class) identity {
	bad := func(status int, code, reason string) identity {
		return identity{status: status, code: code, reason: reason}
	}
	ip := sourceIP(req.RemoteAddr)
	if class == collab.ClassWeb {
		if req.Cookie == "" {
			return bad(401, "Unauthenticated", "session_expired")
		}
		raw, err := base64.RawURLEncoding.DecodeString(req.Cookie)
		if err != nil || len(raw) != 32 {
			return bad(401, "Unauthenticated", "session_expired")
		}
		ses, ok, limited := s.session(ctx, sha256.Sum256(raw), ip, req.Recheck)
		if limited {
			return bad(429, "Throttled", "")
		}
		if !ok || ses.TeamID != route.TeamID {
			return bad(401, "Unauthenticated", "session_expired")
		}
		id := s.person(ctx, route, ses.Issuer+"|"+ses.Subject, ses.Subject, ses.ProviderID, ses.Groups, req.Recheck)
		if id.status == 0 && s.groupOnly(id.bindings) && s.now().Sub(s.issuedAt(ses)) >= GroupReauthAfter {
			id.status, id.code, id.reason, id.reauth = 401, "Unauthenticated", "session_expired", true
		}
		return id
	}
	tok, ok := bearer(req.XAfsAuthorization)
	if !ok || tok == "" {
		if req.XAfsAuthorization == "" {
			return bad(401, "Unauthenticated", "token_invalid")
		}
		return bad(400, "ValidationError", "malformed_credential")
	}
	if strings.HasPrefix(tok, "afsk_") {
		info, found, limited := s.key(ctx, route.TeamID, hash(tok), ip, req.Recheck)
		switch {
		case limited:
			return bad(429, "Throttled", "")
		case !found:
			return bad(401, "Unauthenticated", "token_invalid")
		case info.State == "revoked":
			return bad(401, "Unauthenticated", "key_revoked")
		case info.State == "expired":
			return bad(401, "Unauthenticated", "key_expired")
		}
		return identity{kind: collab.KindAPIKey, id: info.ID, bindings: []Binding{{Role: info.Role}}}
	}
	claims, err := s.Identity.VerifyBearer(ctx, route, tok)
	if err != nil {
		var te *TokenError
		if errors.As(err, &te) && te.Expired {
			return bad(401, "Unauthenticated", "token_expired")
		}
		return bad(401, "Unauthenticated", "token_invalid")
	}
	return s.person(ctx, route, claims.Issuer+"|"+claims.Subject, claims.Subject, route.ProviderID, claims.Groups, req.Recheck)
}

// groupOnly is true when every binding of the caller comes from an IdP group.
func (s *Service) groupOnly(b []Binding) bool {
	if len(b) == 0 {
		return false
	}
	for _, x := range b {
		if !x.Group {
			return false
		}
	}
	return true
}

// issuedAt is when the session was created: the store's value when it has one, else the expiry minus the
// configured lifetime.
func (s *Service) issuedAt(ses Session) time.Time {
	if !ses.CreatedAt.IsZero() {
		return ses.CreatedAt
	}
	return ses.ExpiresAt.Add(-time.Duration(s.Table.Settings().SessionTTLMinutes) * time.Minute)
}

// groupsKey is a stable hash of a caller's groups: a changed group list never reuses a cached decision.
func groupsKey(groups []string) string {
	g := append([]string(nil), groups...)
	sort.Strings(g)
	h := sha256.Sum256([]byte(strings.Join(g, "\x00")))
	return base64.RawStdEncoding.EncodeToString(h[:12])
}

func (s *Service) person(ctx context.Context, route collab.Route, key, subject, provider string, groups []string, fresh bool) identity {
	id := identity{kind: collab.KindUser, id: subject}
	ck := route.TeamID + "|" + key + "|" + groupsKey(groups)
	if !fresh {
		s.mu.Lock()
		c, hit := s.people.get(ck)
		s.mu.Unlock()
		if hit && s.now().Before(c.exp) {
			id.bindings = c.v
			return id
		}
	}
	b, err := s.Principals.Person(ctx, route.TeamID, subject, provider, groups)
	if err != nil {
		return identity{status: 503, code: "Unavailable", reason: "upstream_unavailable"}
	}
	ttl := DecisionTTL
	if len(b) == 0 {
		ttl = NegativeTTL
	}
	s.mu.Lock()
	s.people.put(ck, cached[[]Binding]{v: b, exp: s.now().Add(ttl)})
	s.mu.Unlock()
	id.bindings = b
	return id
}

// session returns the session for an id hash. limited is true when the source address has used up its
// lookups of unknown credentials and the database was not asked.
func (s *Service) session(ctx context.Context, h [32]byte, ip string, fresh bool) (ses Session, ok, limited bool) {
	s.mu.Lock()
	c, hit := s.sess.get(h)
	if hit && !fresh && s.now().Before(c.exp) {
		s.mu.Unlock()
		return c.v, c.ok, false
	}
	if !fresh && s.unknownBlocked(ip) {
		s.mu.Unlock()
		return Session{}, false, true
	}
	s.mu.Unlock()
	ses, ok, err := s.Sessions.Get(ctx, h)
	if err != nil {
		return Session{}, false, false
	}
	ttl := DecisionTTL
	if !ok {
		ttl = NegativeTTL
	}
	s.mu.Lock()
	s.sess.put(h, cached[Session]{v: ses, ok: ok, exp: s.now().Add(ttl)})
	if !ok && !fresh {
		s.noteUnknown(ip)
	}
	s.mu.Unlock()
	return ses, ok, false
}

func (s *Service) key(ctx context.Context, teamID string, h [32]byte, ip string, fresh bool) (info KeyInfo, ok, limited bool) {
	ck := teamID + "|" + string(h[:])
	s.mu.Lock()
	c, hit := s.keys.get(ck)
	if hit && !fresh && s.now().Before(c.exp) {
		s.mu.Unlock()
		return c.v, c.ok, false
	}
	if !fresh && s.unknownBlocked(ip) {
		s.mu.Unlock()
		return KeyInfo{}, false, true
	}
	s.mu.Unlock()
	info, ok, err := s.Principals.Key(ctx, teamID, h)
	if err != nil {
		return KeyInfo{}, false, false
	}
	ttl := DecisionTTL
	if !ok {
		ttl = NegativeTTL
	}
	s.mu.Lock()
	s.keys.put(ck, cached[KeyInfo]{v: info, ok: ok, exp: s.now().Add(ttl)})
	if !ok && !fresh {
		s.noteUnknown(ip)
	}
	s.mu.Unlock()
	return info, ok, false
}

// FrameAllowed reports whether a client WebSocket frame is allowed: only callers with contribute may send
// one; the adapter closes the connection with code 1008 otherwise (PC-D17).
func (s *Service) FrameAllowed(r Result) bool { return collab.FrameAllowed(r.Caller) }

// OnScan records what the body scan found. It returns the 422 result for block, or nothing for warn. Alerts
// are sent at most once per team, person and pattern per SecretAlertEvery, with the accumulated count; the
// matched text never reaches this method.
func (s *Service) OnScan(ctx context.Context, r Result, matches []collab.Match) (Result, bool) {
	if len(matches) == 0 {
		return r, false
	}
	s.mu.Lock()
	s.init()
	s.mu.Unlock()
	action := "warned"
	if r.ScanMode == "block" {
		action = "blocked"
	}
	var patterns []string
	for _, m := range matches {
		patterns = append(patterns, m.Pattern)
		s.alert(ctx, r, m, action)
	}
	if r.ScanMode != "block" {
		s.record(r, collab.OutcomeSecretWarned, 0)
		return r, false
	}
	out := r.deny(422, "SecretDetected", "")
	out.Patterns = patterns
	out.Message = "A credential was found in the request. Store it as an AFS secret instead of sending it to Paperclip."
	out.Outcome = collab.OutcomeDeniedSecret
	s.record(out, collab.OutcomeDeniedSecret, 0)
	return out, true
}

type alertState struct {
	last  time.Time
	count int
}

func (s *Service) alert(ctx context.Context, r Result, m collab.Match, action string) {
	key := r.Route.TeamID + "|" + r.Principal + "|" + m.Pattern
	s.mu.Lock()
	st := s.alerts[key]
	if st == nil {
		st = &alertState{}
		s.alerts[key] = st
	}
	st.count += m.Count
	if !st.last.IsZero() && s.now().Sub(st.last) < SecretAlertEvery {
		s.mu.Unlock()
		return
	}
	count := st.count
	st.last, st.count = s.now(), 0
	s.mu.Unlock()
	id := r.Principal
	if len(id) > 64 {
		id = id[:64]
	}
	_ = s.Alerts.SecretDetected(ctx, r.Route.TeamID, r.Class.Surface(), r.Kind, id, m.Pattern, action, count)
}

package edge

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
)

// signState is what the sign-in redirect carries through the provider, sealed (AEAD) so the apex callback
// can trust it without a store. It holds no token. Bind is the hash of the value in the browser's
// SigninCookieName cookie: only the browser that started the sign-in can finish it (finding 8).
type signState struct {
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	TeamID   string `json:"t"`
	Host     string `json:"h"`
	Return   string `json:"r"`
	Bind     string `json:"b"`
	Silent   bool   `json:"s,omitempty"`
	Expires  int64  `json:"e"`
}

// handoffEnvelope is the sealed value of the handoff link's code: the database code plus the hash of the
// sign-in cookie it may be redeemed with. The database row has no column for the hash.
type handoffEnvelope struct {
	Code    string `json:"c"`
	Bind    string `json:"b"`
	Expires int64  `json:"e"`
}

// safeReturn keeps a return target on the same host: exactly one leading "/" followed by a character that is
// neither "/" nor "\", with no control character, backslash or reserved edge path anywhere, in the form given
// or percent-decoded once. Anything else becomes "/".
func safeReturn(path, query string) string {
	if !safeRef(path) || !safeRef(query) {
		return "/"
	}
	if path == "" || path[0] != '/' || strings.HasPrefix(path, "/_afs/") {
		return "/"
	}
	if len(path) > 1 && (path[1] == '/' || path[1] == '\\') {
		return "/"
	}
	if dec, err := url.PathUnescape(path); err != nil || !safeRef(dec) || (len(dec) > 1 && (dec[1] == '/' || dec[1] == '\\')) {
		return "/"
	}
	if query != "" {
		return path + "?" + query
	}
	return path
}

// safeRef is false when s has a control character (tab and newline included) or a backslash, raw or
// percent-encoded once.
func safeRef(s string) bool {
	bad := func(v string) bool {
		for _, r := range v {
			if r < 0x20 || r == 0x7f || r == '\\' || (r >= 0x80 && r < 0xa0) {
				return true
			}
		}
		return false
	}
	if bad(s) {
		return false
	}
	dec, err := url.PathUnescape(s)
	return err != nil || !bad(dec)
}

// returnTarget validates a "path?query" return value.
func returnTarget(ret string) string {
	path, query := ret, ""
	if i := strings.IndexByte(ret, '?'); i >= 0 {
		path, query = ret[:i], ret[i+1:]
	}
	return safeReturn(path, query)
}

func hashSlice(v string) []byte { h := hash(v); return h[:] }

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// callbackURL is the apex callback of a level: a team host signs in through the apex of its own level, so the
// private entry never depends on a public name and the cookie stays host-only.
func (s *Service) callbackURL(l collab.Level) string {
	return "https://" + s.Table.Settings().BaseDomain(l) + PathCallback
}

func (s *Service) signin(ctx context.Context, req Request, route collab.Route) Result {
	base := Result{Route: route, Class: collab.ClassWeb}
	if !route.WebUIEffective {
		return base.deny(404, "NotFound", "")
	}
	q, _ := url.ParseQuery(req.Query)
	vb, err := s.random(32)
	if err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	nb, err := s.random(16)
	if err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	cb, err := s.random(32)
	if err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	cookie := b64(cb)
	bind := hash(cookie)
	st := signState{Verifier: b64(vb), Nonce: b64(nb), TeamID: route.TeamID, Host: route.Hostname,
		Return: returnTarget(q.Get("return")), Bind: b64(bind[:]), Silent: q.Get("silent") == "1", Expires: s.now().Add(StateTTL).Unix()}
	plain, _ := json.Marshal(st)
	sealed, err := s.Sealer.Seal(plain)
	if err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	ch := sha256.Sum256([]byte(st.Verifier))
	prompt := ""
	if st.Silent {
		prompt = "none"
	}
	loc, err := s.Identity.AuthURL(ctx, route, s.callbackURL(route.Level), sealed, b64(ch[:]), st.Nonce, prompt)
	if err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	base.Action, base.Status, base.Location, base.Outcome = Redirect, 302, loc, collab.OutcomeAllowed
	base.SetSigninCookie, base.SigninCookie = true, cookie
	return base
}

// silentFailures are the provider errors that mean a prompt=none request needs the person to interact.
var silentFailures = map[string]bool{"login_required": true, "interaction_required": true, "consent_required": true,
	"account_selection_required": true}

// callback runs on the apex: it opens the state, exchanges the code, stores a one-time handoff and sends
// the browser back to its team host.
func (s *Service) callback(ctx context.Context, req Request) Result {
	q, _ := url.ParseQuery(req.Query)
	fail := func(status int, code, reason string) Result { return respond(status, code, reason) }
	plain, err := s.Sealer.Open(q.Get("state"))
	if err != nil || q.Get("state") == "" {
		return fail(400, "ValidationError", "malformed_credential")
	}
	var st signState
	if json.Unmarshal(plain, &st) != nil || st.Expires < s.now().Unix() {
		return fail(401, "Unauthenticated", "session_expired")
	}
	route, kind := s.Table.Lookup(st.Host)
	if kind != collab.HostTeam || route.TeamID != st.TeamID {
		return fail(404, "NotFound", "")
	}
	base := Result{Route: route, Class: collab.ClassWeb}
	if st.Silent && silentFailures[q.Get("error")] {
		// The provider could not re-authenticate silently: start an interactive sign-in.
		rq := url.Values{"return": {st.Return}}
		base.Action, base.Status, base.Outcome = Redirect, 302, collab.OutcomeAllowed
		base.Location = "https://" + NormalizeHost(route.Hostname) + PathSignin + "?" + rq.Encode()
		return base
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		return base.deny(401, "Unauthenticated", "token_invalid")
	}
	claims, err := s.Identity.Exchange(ctx, route, s.callbackURL(route.Level), q.Get("code"), st.Verifier)
	if err != nil || claims.Nonce != st.Nonce {
		return base.deny(401, "Unauthenticated", "token_invalid")
	}
	cb, err := s.random(32)
	if err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	code := b64(cb)
	if err := s.Sessions.HandoffCreate(ctx, hash(code), route.TeamID, NormalizeHost(route.Hostname), claims.Issuer, claims.Subject, route.ProviderID, claims.Groups); err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	env, _ := json.Marshal(handoffEnvelope{Code: code, Bind: st.Bind, Expires: s.now().Add(StateTTL).Unix()})
	sealed, err := s.Sealer.Seal(env)
	if err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	hq := url.Values{"code": {sealed}, "return": {st.Return}}
	base.Action, base.Status, base.Outcome = Redirect, 302, collab.OutcomeAllowed
	base.Location = "https://" + NormalizeHost(route.Hostname) + PathHandoff + "?" + hq.Encode()
	return base
}

// handoff runs on the team host: it consumes the code once and sets the host-only session cookie.
func (s *Service) handoff(ctx context.Context, req Request, route collab.Route) Result {
	base := Result{Route: route, Class: collab.ClassWeb}
	q, _ := url.ParseQuery(req.Query)
	if q.Get("code") == "" {
		return base.deny(401, "Unauthenticated", "token_invalid")
	}
	plain, err := s.Sealer.Open(q.Get("code"))
	var env handoffEnvelope
	if err != nil || json.Unmarshal(plain, &env) != nil || env.Code == "" || env.Expires < s.now().Unix() {
		return base.deny(401, "Unauthenticated", "token_invalid")
	}
	// The link works only in the browser that started the sign-in (finding 8).
	if req.SigninCookie == "" || subtle.ConstantTimeCompare([]byte(b64(hashSlice(req.SigninCookie))), []byte(env.Bind)) != 1 {
		return base.deny(401, "Unauthenticated", "token_invalid")
	}
	code := env.Code
	sb, err := s.random(32)
	if err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	sid := b64(sb)
	team, exp, ok, err := s.Sessions.HandoffConsume(ctx, hash(code), req.Host, sha256.Sum256(sb))
	if err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	if !ok || team != route.TeamID {
		return base.deny(401, "Unauthenticated", "token_invalid")
	}
	age := int(exp.Sub(s.now()).Seconds())
	if age < 1 {
		return base.deny(401, "Unauthenticated", "session_expired")
	}
	base.Action, base.Status, base.Outcome = Redirect, 302, collab.OutcomeSignedIn
	base.Location = returnTarget(q.Get("return"))
	base.SetCookie, base.Cookie, base.CookieMaxAge = true, sid, age
	base.ClearSigninCookie = true
	return base
}

// signout revokes the session named by the cookie and clears it. It is idempotent.
func (s *Service) signout(ctx context.Context, req Request, route collab.Route) Result {
	base := Result{Route: route, Class: collab.ClassWeb}
	if raw, err := base64.RawURLEncoding.DecodeString(req.Cookie); err == nil && len(raw) == 32 {
		h := sha256.Sum256(raw)
		_ = s.Sessions.Revoke(ctx, h)
		s.mu.Lock()
		s.sess.delete(h)
		s.mu.Unlock()
	}
	base.Action, base.Status, base.Location, base.ClearCookie, base.Outcome = Redirect, 302, "/", true, collab.OutcomeSignedOut
	return base
}

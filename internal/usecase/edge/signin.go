package edge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
)

// signState is what the sign-in redirect carries through the provider, sealed (AEAD) so the apex callback
// can trust it without a store. It holds no token.
type signState struct {
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	TeamID   string `json:"t"`
	Host     string `json:"h"`
	Return   string `json:"r"`
	Expires  int64  `json:"e"`
}

// safeReturn keeps a return target on the same host: a rooted path that is neither protocol-relative nor
// a reserved edge path. Anything else becomes "/".
func safeReturn(path, query string) string {
	if path == "" || path[0] != '/' || strings.HasPrefix(path, "//") || strings.HasPrefix(path, "/\\") ||
		strings.HasPrefix(path, "/_afs/") || strings.ContainsAny(path, "\r\n") {
		return "/"
	}
	if query != "" {
		return path + "?" + query
	}
	return path
}

// returnTarget validates a "path?query" return value.
func returnTarget(ret string) string {
	path, query := ret, ""
	if i := strings.IndexByte(ret, '?'); i >= 0 {
		path, query = ret[:i], ret[i+1:]
	}
	return safeReturn(path, query)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (s *Service) callbackURL() string {
	return "https://" + s.Table.Settings().BaseDomain + PathCallback
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
	st := signState{Verifier: b64(vb), Nonce: b64(nb), TeamID: route.TeamID, Host: req.Host,
		Return: returnTarget(q.Get("return")), Expires: s.now().Add(StateTTL).Unix()}
	plain, _ := json.Marshal(st)
	sealed, err := s.Sealer.Seal(plain)
	if err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	ch := sha256.Sum256([]byte(st.Verifier))
	loc, err := s.Identity.AuthURL(ctx, route, s.callbackURL(), sealed, b64(ch[:]), st.Nonce)
	if err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	base.Action, base.Status, base.Location, base.Outcome = Redirect, 302, loc, collab.OutcomeAllowed
	return base
}

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
	route, ok := s.Table.RouteByTeam(st.TeamID)
	if !ok || route.Hostname != st.Host {
		return fail(404, "NotFound", "")
	}
	base := Result{Route: route, Class: collab.ClassWeb}
	if q.Get("error") != "" || q.Get("code") == "" {
		return base.deny(401, "Unauthenticated", "token_invalid")
	}
	claims, err := s.Identity.Exchange(ctx, route, s.callbackURL(), q.Get("code"), st.Verifier)
	if err != nil || claims.Nonce != st.Nonce {
		return base.deny(401, "Unauthenticated", "token_invalid")
	}
	cb, err := s.random(32)
	if err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	code := b64(cb)
	if err := s.Sessions.HandoffCreate(ctx, hash(code), route.TeamID, route.Hostname, claims.Issuer, claims.Subject, route.ProviderID, claims.Groups); err != nil {
		return base.deny(503, "Unavailable", "upstream_unavailable")
	}
	hq := url.Values{"code": {code}, "return": {st.Return}}
	base.Action, base.Status, base.Outcome = Redirect, 302, collab.OutcomeAllowed
	base.Location = "https://" + route.Hostname + PathHandoff + "?" + hq.Encode()
	return base
}

// handoff runs on the team host: it consumes the code once and sets the host-only session cookie.
func (s *Service) handoff(ctx context.Context, req Request, route collab.Route) Result {
	base := Result{Route: route, Class: collab.ClassWeb}
	q, _ := url.ParseQuery(req.Query)
	code := q.Get("code")
	if code == "" {
		return base.deny(401, "Unauthenticated", "token_invalid")
	}
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
	return base
}

// signout revokes the session named by the cookie and clears it. It is idempotent.
func (s *Service) signout(ctx context.Context, req Request, route collab.Route) Result {
	base := Result{Route: route, Class: collab.ClassWeb}
	if raw, err := base64.RawURLEncoding.DecodeString(req.Cookie); err == nil && len(raw) == 32 {
		h := sha256.Sum256(raw)
		_ = s.Sessions.Revoke(ctx, h)
		s.mu.Lock()
		delete(s.sess, h)
		s.mu.Unlock()
	}
	base.Action, base.Status, base.Location, base.ClearCookie, base.Outcome = Redirect, 302, "/", true, collab.OutcomeSignedOut
	return base
}

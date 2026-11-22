// Package edge is the use case set of afs-edge, the only internet-reachable component of the Paperclip
// collaboration feature (EDGE-1..7). It decides, for each request to a team's collaboration hostname, what
// the HTTP adapter does: forward it, answer it, or redirect it. It owns no socket, no SQL and no HTTP
// type; every collaborator is a port. It imports the domain and authz only.
package edge

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
)

// RouteSource reads the route table and the collab.* settings (afs.collab_routes, afs.collab_settings).
type RouteSource interface {
	Routes(ctx context.Context) ([]collab.Route, error)
	Settings(ctx context.Context) (collab.Settings, error)
}

// Binding is a role binding that applies to a caller on a team (afs.collab_principal).
type Binding struct {
	Role string // team_admin, team_viewer, team_collaborator or service_admin
}

// KeyInfo is a team API key as the database reports it (afs.collab_principal_key).
type KeyInfo struct {
	ID        string
	Role      string
	State     string // active, expired or revoked
	ExpiresAt time.Time
}

// Principals resolves callers to bindings.
type Principals interface {
	Person(ctx context.Context, teamID, subject, providerID string, groups []string) ([]Binding, error)
	Key(ctx context.Context, teamID string, sha256 [32]byte) (KeyInfo, bool, error)
}

// Session is a live server-side session (afs.collab_session_get).
type Session struct {
	TeamID, Issuer, Subject, ProviderID string
	Groups                              []string
	ExpiresAt                           time.Time
}

// Sessions is the session and handoff store (database contract 7.2.8). Only hashes cross this port.
type Sessions interface {
	HandoffCreate(ctx context.Context, codeHash [32]byte, teamID, host, issuer, subject, providerID string, groups []string) error
	// HandoffConsume returns the new session's team and expiry; ok is false when the code is unknown, used,
	// expired or bound to another host.
	HandoffConsume(ctx context.Context, codeHash [32]byte, host string, sessionHash [32]byte) (teamID string, expires time.Time, ok bool, err error)
	Get(ctx context.Context, idHash [32]byte) (Session, bool, error)
	Revoke(ctx context.Context, idHash [32]byte) error
}

// Claims are the verified facts of an identity token.
type Claims struct {
	Issuer, Subject string
	Groups          []string
	Nonce           string
}

// Token errors the identity port returns, so the edge can name the 401 reason.
type TokenError struct{ Expired bool }

func (e *TokenError) Error() string {
	if e.Expired {
		return "token expired"
	}
	return "token invalid"
}

// Identity talks to a team's sign-in provider.
type Identity interface {
	// AuthURL builds the authorization request (code flow, S256 challenge).
	AuthURL(ctx context.Context, r collab.Route, redirectURI, state, challenge, nonce string) (string, error)
	// Exchange trades the code for an ID token and validates it (issuer, audience = the collaboration client
	// id, expiry, nonce, signature). The client secret is read inside the adapter and never returned.
	Exchange(ctx context.Context, r collab.Route, redirectURI, code, verifier string) (Claims, error)
	// VerifyBearer validates an access or ID token sent in X-Afs-Authorization; *TokenError on failure.
	VerifyBearer(ctx context.Context, r collab.Route, token string) (Claims, error)
}

// Sealer seals and opens the sign-in state parameter (AEAD, two active keys).
type Sealer interface {
	Seal(plain []byte) (string, error)
	Open(sealed string) ([]byte, error)
}

// AccessSink stores five-minute access counts (afs.record_collab_access).
type AccessSink interface {
	Record(ctx context.Context, rows []collab.Row) error
}

// Alerts emits collaboration.secret_detected (afs.emit_collab_secret_detected).
type Alerts interface {
	SecretDetected(ctx context.Context, teamID string, surface collab.Surface, kind collab.PrincipalKind, principalID, pattern, action string, count int) error
}

// Event is one structured access record: sign-in, sign-out or a denied request. It carries no body, header
// value, cookie, token or query string.
type Event struct {
	TeamID, Event, PrincipalKind, PrincipalID, Method, Route, Reason string
	Status                                                           int
	Bytes                                                            int64
	Duration                                                         time.Duration
	ServiceAdminEntry                                                bool
	RequestID                                                        string
}

// Records writes access events.
type Records interface{ Write(Event) }

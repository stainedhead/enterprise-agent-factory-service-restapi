// Package auth authenticates callers of afs-api (RA-2): an identity provider token (OIDC, validated by
// a TokenVerifier), a team or service API key (afsk_…), and, from M6, a workload token. It turns each
// into an authz.Principal. It imports only domain, authz and the shared ports.
package auth

import (
	"context"
	"crypto/sha256"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// APIKeyPrefix starts every API key secret (API contract §2).
const APIKeyPrefix = "afsk_"

// providerCacheTTL is how long the enabled provider list is reused, so a disabled provider stops
// authenticating within seconds without a database read on every request.
const providerCacheTTL = 10 * time.Second

// Provider is an enabled row of afs.identity_providers. Providers are service level; a team chooses
// one (teams.identity_provider_id). There is one client id per provider (CD-9): it is both the
// console and the CLI audience.
type Provider struct {
	ID          string
	Kind        string
	Name        string
	IssuerURL   string
	ClientID    string
	GroupsClaim string
	IsDefault   bool
}

// Identity is what a verified token says about its bearer.
type Identity struct {
	Provider Provider
	Subject  string
	Email    string
	Name     string
	Groups   []string
}

// TokenVerifier validates an identity provider token against the JWKS of the provider that issued
// it. It returns domain.Unauthenticated for a token that is malformed, expired, for another audience,
// signed with an unknown key or issued by none of providers, and domain.Unavailable when the keys
// cannot be fetched and none are cached (RA-US-2.3).
type TokenVerifier interface {
	Verify(ctx context.Context, token string, providers []Provider) (Identity, error)
}

// ProviderSource lists the enabled identity providers.
type ProviderSource interface {
	EnabledProviders(ctx context.Context) ([]Provider, error)
}

// BindingRow is one role binding that may apply to a caller, with the provider facts the group rule
// needs. TeamProviderID is the team's own provider (empty: the default provider).
type BindingRow struct {
	Role              authz.Role
	TeamID            string
	Group             string // set for a group binding
	BindingProviderID string // the provider a group binding names (empty: the default provider)
	TeamProviderID    string
}

// PrincipalStore reads and writes the people behind a token.
type PrincipalStore interface {
	// SignIn creates or refreshes the users row of (issuer, subject) and returns its id. A disabled
	// user gets domain.Unauthenticated.
	SignIn(ctx context.Context, u UserClaims, now time.Time) (userID string, err error)
	// Bindings returns the bindings of userID and the group bindings of any of groups, on teams that
	// are not deleted.
	Bindings(ctx context.Context, userID string, groups []string) ([]BindingRow, error)
}

// UserClaims are the identity facts stored on the users row.
type UserClaims struct {
	Issuer, Subject, Email, Name string
}

// WorkloadAuthenticator authenticates projected ServiceAccount tokens (RA-2b, M6-D). Matches reports
// whether token is one, without validating it.
type WorkloadAuthenticator interface {
	Matches(token string) bool
	Authenticate(ctx context.Context, token string) (*authz.Principal, error)
}

// Service authenticates bearer credentials.
type Service struct {
	Verifier  TokenVerifier
	Providers ProviderSource
	Store     PrincipalStore
	Keys      port.CredentialLookup
	// Workload is optional; nil until M6-D.
	Workload WorkloadAuthenticator
	Clock    port.Clock

	mu       sync.Mutex
	cached   []Provider
	cachedAt time.Time
}

// Authenticate turns a bearer token into a principal. Every credential failure is
// domain.Unauthenticated and never says which part failed; a dependency that is down is
// domain.Unavailable. The token is never part of an error.
func (s *Service) Authenticate(ctx context.Context, token string) (*authz.Principal, error) {
	switch {
	case token == "":
		return nil, domain.Unauthenticated()
	case strings.HasPrefix(token, APIKeyPrefix):
		return s.apiKey(ctx, token)
	case s.Workload != nil && s.Workload.Matches(token):
		return s.Workload.Authenticate(ctx, token)
	}
	return s.user(ctx, token)
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// HashAPIKey is the SHA-256 stored in api_keys.key_sha256 for a full key (prefix included).
func HashAPIKey(key string) [32]byte { return sha256.Sum256([]byte(key)) }

func (s *Service) apiKey(ctx context.Context, token string) (*authz.Principal, error) {
	rec, ok, err := s.Keys.AuthenticateAPIKey(ctx, HashAPIKey(token))
	if err != nil {
		return nil, err
	}
	if !ok || !rec.Usable() {
		return nil, domain.Unauthenticated()
	}
	p := &authz.Principal{ID: rec.KeyID, Type: authz.PrincipalAPIKey}
	role := authz.Role(rec.Role)
	switch {
	case rec.TeamID == "" && role == authz.RoleSecurityOperator:
		p.SecurityOperator = true
	case rec.TeamID != "" && role.TeamRole():
		p.Bindings = []authz.Binding{{Role: role, TeamID: rec.TeamID}}
	default:
		// A key whose role does not fit its scope cannot exist (database CHECK); refuse it.
		return nil, domain.Unauthenticated()
	}
	return p, nil
}

func (s *Service) providers(ctx context.Context) ([]Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.cached != nil && now.Sub(s.cachedAt) < providerCacheTTL {
		return s.cached, nil
	}
	ps, err := s.Providers.EnabledProviders(ctx)
	if err != nil {
		return nil, err
	}
	if ps == nil {
		ps = []Provider{}
	}
	s.cached, s.cachedAt = ps, now
	return ps, nil
}

// SignInProviders lists the enabled providers for the public sign-in screen (RA-US-2.5). The list is
// the one used to validate tokens, so what a client is offered is what the API accepts.
func (s *Service) SignInProviders(ctx context.Context) ([]Provider, error) { return s.providers(ctx) }

func (s *Service) user(ctx context.Context, token string) (*authz.Principal, error) {
	providers, err := s.providers(ctx)
	if err != nil {
		return nil, err
	}
	id, err := s.Verifier.Verify(ctx, token, providers)
	if err != nil {
		return nil, err
	}
	userID, err := s.Store.SignIn(ctx, UserClaims{Issuer: id.Provider.IssuerURL, Subject: id.Subject, Email: id.Email, Name: id.Name}, s.now())
	if err != nil {
		return nil, err
	}
	rows, err := s.Store.Bindings(ctx, userID, id.Groups)
	if err != nil {
		return nil, err
	}
	p := Resolve(id, userID, rows, defaultProviderID(providers))
	return &p, nil
}

func defaultProviderID(ps []Provider) string {
	for _, p := range ps {
		if p.IsDefault {
			return p.ID
		}
	}
	return ""
}

// Resolve builds the principal of a signed-in user from the binding rows (RA-2). A user binding
// always counts. A group binding counts only when the token's group list names the group and the
// token comes from the right provider: the one the binding names (else the default provider), and for
// a team binding also the team's own provider (else the default provider) — RA-US-2.2.
func Resolve(id Identity, userID string, rows []BindingRow, defaultProvider string) authz.Principal {
	p := authz.Principal{ID: userID, Type: authz.PrincipalUser, Email: id.Email, Name: id.Name}
	for _, r := range rows {
		if r.Group != "" && !groupCounts(id, r, defaultProvider) {
			continue
		}
		switch {
		case r.Role == authz.RoleServiceAdmin:
			p.ServiceAdmin = true
		case r.Role == authz.RoleSecurityOperator:
			p.SecurityOperator = true
		case r.Role.TeamRole() && r.TeamID != "":
			b := authz.Binding{Role: r.Role, TeamID: r.TeamID}
			if !slices.Contains(p.Bindings, b) {
				p.Bindings = append(p.Bindings, b)
			}
		}
	}
	return p
}

func groupCounts(id Identity, r BindingRow, defaultProvider string) bool {
	if !slices.Contains(id.Groups, r.Group) {
		return false
	}
	via := id.Provider.ID
	if r.BindingProviderID != "" && r.BindingProviderID != via {
		return false
	}
	if r.TeamID != "" {
		want := r.TeamProviderID
		if want == "" {
			want = defaultProvider
		}
		return want == via
	}
	return r.BindingProviderID != "" || via == defaultProvider
}

// SessionOf turns a principal into the database session of database contract §3.
func SessionOf(p authz.Principal, requestID string) port.Session {
	s := port.Session{Actor: p.ID, ServiceAdmin: p.ServiceAdmin, SecurityOperator: p.SecurityOperator,
		TeamIDs: p.TeamIDs(), RequestID: requestID}
	switch {
	case p.Workload != nil && p.Workload.PoolID != "":
		s.ActorType = port.ActorPool
	case p.Workload != nil:
		s.ActorType = port.ActorAgent
	case p.Type == authz.PrincipalAPIKey:
		s.ActorType = port.ActorAPIKey
	default:
		s.ActorType = port.ActorUser
	}
	return s
}

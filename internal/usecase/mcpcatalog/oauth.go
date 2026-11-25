package mcpcatalog

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// OAuth connect for oauth_user servers (MCP gateway PRD §9 case 5, API contract CD-48): a team-admin starts the
// authorization code + PKCE flow against the server's authorization server, found by Protected Resource Metadata
// (RFC 9728) and Authorization Server Metadata (RFC 8414). AFS is a public client identified by its Client ID
// Metadata Document URL. The callback is bound to a single-use state (10 minutes) and to the issuer (RFC 9207),
// exchanges the code with the resource indicator (RFC 8707) and writes the refresh token to Secrets Manager at the
// gateway path. The PKCE verifier is derived from the state with a keyed HMAC, so nothing is stored but the state's
// hash and afs-api never reads a secret back.

// OAuthStateTTL is how long a started connect may be completed.
const OAuthStateTTL = 10 * time.Minute

// OAuthServer is an authorization server as discovery found it for one resource.
type OAuthServer struct {
	Issuer                string
	AuthorizationEndpoint string
	TokenEndpoint         string
	// Resource is the protected resource identifier (RFC 9728 resource), sent as the RFC 8707 resource parameter.
	Resource string
	// Scopes are the resource's scopes_supported, requested at authorization.
	Scopes []string
}

// OAuthExchange is an authorization code grant request.
type OAuthExchange struct {
	Code, Verifier, RedirectURI, ClientID, Resource string
}

// OAuthTokens is the part of a token response AFS keeps. Subject is a display name for the connected account (from
// an ID token's email or preferred_username when one is returned); it is never used for a decision.
type OAuthTokens struct {
	RefreshToken domain.SecretValue
	Scopes       []string
	Subject      string
}

// OAuthClient is the outbound OAuth port (adapter: internal/adapter/mcpoauth).
type OAuthClient interface {
	// Discover reads the resource's Protected Resource Metadata and its first authorization server's metadata.
	Discover(ctx context.Context, resourceURL string) (OAuthServer, error)
	// Exchange redeems an authorization code at the token endpoint.
	Exchange(ctx context.Context, as OAuthServer, req OAuthExchange) (OAuthTokens, error)
}

// OAuthSettings configure the connect flow. A flow without ClientID or RedirectURI is refused as not configured.
type OAuthSettings struct {
	// ClientID is the URL of AFS's Client ID Metadata Document for this environment (AFS_MCP_OAUTH_CLIENT_ID).
	ClientID string
	// RedirectURI is the public URL of GET /v1/mcp-oauth/callback (AFS_MCP_OAUTH_REDIRECT_URI).
	RedirectURI string
	// ConsoleURL is the console's base URL the callback redirects to (AFS_CONSOLE_URL).
	ConsoleURL string
	// StateKey keys the HMAC that derives the PKCE verifier from the state (at least 32 bytes).
	StateKey []byte
	// Rand makes states (crypto/rand in production).
	Rand io.Reader
}

// OAuthStart is the answer of StartMcpOAuthConnect.
type OAuthStart struct {
	AuthorizationURL string
	State            string
	ExpiresAt        time.Time
}

// OAuthState is a claimed connect state.
type OAuthState struct {
	TeamID, ServerID, CreatedBy string
}

// ReasonOAuthNotConfigured is the 409 reason when the environment has no Client ID Metadata Document or callback URL.
const ReasonOAuthNotConfigured = "oauth_not_configured"

// ReasonOAuthDiscoveryFailed is the 409 reason when RFC 9728 or RFC 8414 discovery fails at start.
const ReasonOAuthDiscoveryFailed = "oauth_discovery_failed"

func (o OAuthSettings) configured() bool {
	return o.ClientID != "" && o.RedirectURI != "" && o.ConsoleURL != "" && len(o.StateKey) >= 32 && o.Rand != nil
}

// verifier derives the PKCE code verifier (RFC 7636: 43 characters of base64url) from the state.
func (o OAuthSettings) verifier(state string) string {
	m := hmac.New(sha256.New, o.StateKey)
	m.Write([]byte("afs-mcp-oauth-verifier\x00"))
	m.Write([]byte(state))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func stateHash(state string) []byte {
	h := sha256.Sum256([]byte(state))
	return h[:]
}

func challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// StartOAuth implements StartMcpOAuthConnect: an oauth_user server the team uses (its own, or an enterprise server
// switched on for it). It writes a single-use state and answers the authorization URL. Audited, no event (§4.12).
func (s *Service) StartOAuth(ctx context.Context, c acting.Caller, teamID, serverID string) (OAuthStart, error) {
	const action = "StartMcpOAuthConnect"
	if err := s.checkTeam(ctx, c, action, teamID, authz.TeamMcpServersWrite); err != nil {
		return OAuthStart{}, err
	}
	if !s.OAuth.configured() || s.OAuthHTTP == nil {
		return OAuthStart{}, domain.Conflict(ReasonOAuthNotConfigured, "OAuth connect is not configured in this environment", nil)
	}
	var srv domain.McpServerRecord
	if err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		row, err := s.Repo.GetTeamServer(ctx, tx, teamID, serverID)
		srv = row.Server
		return err
	}); err != nil {
		return OAuthStart{}, err
	}
	if srv.AuthKind != domain.McpAuthOAuthUser {
		return OAuthStart{}, domain.Validation(domain.FieldError{Field: "authKind", Reason: domain.ReasonAuthKindUnsupported})
	}
	if !srv.IsTeam() && srv.CredentialMode == domain.McpCredentialShared && !c.Principal.ServiceAdmin {
		return OAuthStart{}, s.refuse(ctx, c, action, teamID, authz.TeamMcpServersWrite, domain.ReasonNotTeamServer,
			"the shared account of an enterprise server is connected by a service admin")
	}
	as, err := s.OAuthHTTP.Discover(ctx, srv.URL)
	if err != nil {
		return OAuthStart{}, domain.Conflict(ReasonOAuthDiscoveryFailed,
			"the server publishes no usable OAuth protected resource or authorization server metadata", nil)
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(s.OAuth.Rand, raw); err != nil {
		return OAuthStart{}, err
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	expires := s.Now().Add(OAuthStateTTL).Truncate(time.Second)
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", s.OAuth.ClientID)
	q.Set("redirect_uri", s.OAuth.RedirectURI)
	q.Set("state", state)
	q.Set("code_challenge", challenge(s.OAuth.verifier(state)))
	q.Set("code_challenge_method", "S256")
	q.Set("resource", as.Resource)
	if len(as.Scopes) > 0 {
		q.Set("scope", strings.Join(as.Scopes, " "))
	}
	sep := "?"
	if strings.Contains(as.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	out := OAuthStart{AuthorizationURL: as.AuthorizationEndpoint + sep + q.Encode(), State: state, ExpiresAt: expires}
	err = s.write(ctx, c, func(ctx context.Context, tx port.Tx) error {
		if err := s.Repo.InsertOAuthState(ctx, tx, stateHash(state), teamID, serverID, expires); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record(action, kindCredential, serverID, teamID, nil,
			map[string]any{"serverId": serverID, "issuer": as.Issuer, "expiresAt": expires}))
	})
	if err != nil {
		return OAuthStart{}, err
	}
	return out, nil
}

// CallbackInput is the query of GET /v1/mcp-oauth/callback.
type CallbackInput struct {
	Code, State, Issuer, Error     string
	RequestID, SourceIP, UserAgent string
}

// CallbackResult is where the browser goes next: the console's server page with the outcome.
type CallbackResult struct {
	Redirect string
	// Reason is empty on success; otherwise a short code (state_invalid, issuer_mismatch, authorization_denied,
	// discovery_failed, exchange_failed, no_refresh_token, not_configured, server_changed).
	Reason string
}

// CompleteOAuth implements CompleteMcpOAuthConnect. It has no bearer: the state is the authority, claimed once. A
// failure is never an error page with detail; the browser is redirected with a reason code.
func (s *Service) CompleteOAuth(ctx context.Context, in CallbackInput) CallbackResult {
	fail := func(teamID, serverID, reason string) CallbackResult {
		return CallbackResult{Redirect: s.consoleURL(teamID, serverID, reason), Reason: reason}
	}
	if !s.OAuth.configured() || s.OAuthHTTP == nil {
		return fail("", "", "not_configured")
	}
	if in.State == "" || len(in.State) > 512 {
		return fail("", "", "state_invalid")
	}
	anon := acting.Caller{Principal: authz.Principal{ID: "mcp-oauth-callback", Type: authz.PrincipalUser}, RequestID: in.RequestID}
	var st *OAuthState
	if err := s.write(ctx, anon, func(ctx context.Context, tx port.Tx) error {
		var err error
		st, err = s.Repo.ClaimOAuthState(ctx, tx, stateHash(in.State))
		return err
	}); err != nil || st == nil {
		return fail("", "", "state_invalid")
	}
	if in.Error != "" || in.Code == "" {
		return fail(st.TeamID, st.ServerID, "authorization_denied")
	}
	// The person who started the connect, on the team the state names: the start checked team-mcp-servers:write.
	c := acting.Caller{Principal: authz.Principal{ID: st.CreatedBy, Type: principalTypeOf(st.CreatedBy),
		Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: st.TeamID}}},
		RequestID: in.RequestID, SourceIP: in.SourceIP, UserAgent: in.UserAgent}
	var srv domain.McpServerRecord
	if err := s.read(ctx, c, func(ctx context.Context, tx port.Tx) error {
		row, err := s.Repo.GetTeamServer(ctx, tx, st.TeamID, st.ServerID)
		srv = row.Server
		return err
	}); err != nil || srv.AuthKind != domain.McpAuthOAuthUser {
		return fail(st.TeamID, st.ServerID, "server_changed")
	}
	as, err := s.OAuthHTTP.Discover(ctx, srv.URL)
	if err != nil {
		return fail(st.TeamID, st.ServerID, "discovery_failed")
	}
	// RFC 9207: the response names the issuer that issued it; a mix-up sends the code to the wrong server.
	if in.Issuer != "" && in.Issuer != as.Issuer {
		return fail(st.TeamID, st.ServerID, "issuer_mismatch")
	}
	tok, err := s.OAuthHTTP.Exchange(ctx, as, OAuthExchange{Code: in.Code, Verifier: s.OAuth.verifier(in.State),
		RedirectURI: s.OAuth.RedirectURI, ClientID: s.OAuth.ClientID, Resource: as.Resource})
	if err != nil {
		return fail(st.TeamID, st.ServerID, "exchange_failed")
	}
	if tok.RefreshToken.IsZero() {
		return fail(st.TeamID, st.ServerID, "no_refresh_token")
	}
	val, err := oauthSecret(as, s.OAuth.ClientID, tok.RefreshToken)
	if err != nil {
		return fail(st.TeamID, st.ServerID, "exchange_failed")
	}
	subject := tok.Subject
	if subject == "" {
		subject = "service account at " + hostOf(as.Issuer)
	}
	scope := srv.CredentialScope(st.TeamID)
	// Only a service admin can start a connect of an enterprise server's shared account (StartOAuth), and the state
	// is bound to that start, so the shared row is written with service-admin rights.
	c.Principal.ServiceAdmin = scope == domain.McpSharedScope
	if _, err := s.storeCredential(ctx, c, "CompleteMcpOAuthConnect", st.ServerID, scope, val, func(r *domain.McpCredentialRecord) {
		r.Subject, r.Scopes = subject, tok.Scopes
	}, func(x domain.McpServerRecord) (string, error) {
		if x.AuthKind != domain.McpAuthOAuthUser {
			return "", domain.Validation(domain.FieldError{Field: "authKind", Reason: domain.ReasonAuthKindUnsupported})
		}
		if scope == domain.McpSharedScope {
			return "", nil
		}
		return st.TeamID, nil
	}); err != nil {
		return fail(st.TeamID, st.ServerID, "exchange_failed")
	}
	return CallbackResult{Redirect: s.consoleURL(st.TeamID, st.ServerID, "")}
}

// oauthSecret is the Secrets Manager value the gateway refreshes from.
func oauthSecret(as OAuthServer, clientID string, refresh domain.SecretValue) (domain.SecretValue, error) {
	b, err := json.Marshal(map[string]string{"refreshToken": refresh.Expose(), "tokenEndpoint": as.TokenEndpoint,
		"issuer": as.Issuer, "clientId": clientID, "resource": as.Resource})
	if err != nil {
		return domain.SecretValue{}, err
	}
	return domain.NewSecretValue("refreshToken", string(b))
}

func principalTypeOf(id string) authz.PrincipalType {
	if strings.HasPrefix(id, "key_") {
		return authz.PrincipalAPIKey
	}
	return authz.PrincipalUser
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

// consoleURL is the console page of the server for the team, with the outcome.
func (s *Service) consoleURL(teamID, serverID, reason string) string {
	base := strings.TrimRight(s.OAuth.ConsoleURL, "/")
	path := "/mcp-servers"
	if teamID != "" && serverID != "" {
		path = "/teams/" + url.PathEscape(teamID) + "/mcp-servers/" + url.PathEscape(serverID)
	}
	q := url.Values{}
	if reason == "" {
		q.Set("mcpOAuth", "connected")
	} else {
		q.Set("mcpOAuth", "failed")
		q.Set("reason", reason)
	}
	return base + path + "?" + q.Encode()
}

// ErrOAuthUpstream marks a discovery or token endpoint failure; the adapter wraps it.
var ErrOAuthUpstream = errors.New("mcpcatalog: OAuth upstream failed")

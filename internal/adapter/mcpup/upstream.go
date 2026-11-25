// Package mcpup is the team gateway's MCP client of each upstream server (gateway contract §13.6): it resolves the
// server's endpoint, attaches the credential its auth kind calls for, keeps a 2025-11-25 upstream session, answers
// server requests for input on the agent's behalf (§13.7), and lists inventories for the refresh. The agent's own
// credentials are never forwarded: the gateway handler builds a fresh upstream message, and this package sends
// only a token issued for the upstream. Access tokens live in memory, keyed by server, and are refreshed before
// they expire. Nothing here logs or returns a credential.
package mcpup

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpgateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpoauth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpwire"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpinventory"
)

// Target is what the gateway needs to reach one server (afs.mcp_servers, no credential).
type Target struct {
	ID              string
	TeamID          string // "" for an enterprise server
	Slug            string
	Transport       domain.McpTransport
	Location        domain.EndpointLocation
	URL             string // "" for a hosted server
	CABundle        string
	AuthKind        domain.McpAuthKind
	AuthHeader      string
	Federation      *domain.McpFederation
	ProtocolVersion string
	CredentialMode  string
	ReviewStatus    string
	DriftPolicy     string
	MaxResponse     int64
}

// Targets reads targets (postgres.McpGatewayStore, through a cache).
type Targets interface {
	Target(ctx context.Context, id string) (Target, error)
}

// State records what the gateway learns about credentials and federation (afs_gateway column grants).
type State interface {
	// MarkMcpCredential sets the status of the server's credential in scope (set or expired) and emits
	// mcp_server.auth_expired or .auth_restored when it changes.
	MarkMcpCredential(ctx context.Context, serverID, scope, status string) error
	// ReportMcpFederation records the outcome of a federation exchange for the team (ready or failing).
	ReportMcpFederation(ctx context.Context, serverID, teamID, status, lastError string) error
}

// Tokens mints access tokens (mcpoauth.Client).
type Tokens interface {
	Discover(ctx context.Context, resourceURL string) (mcpcatalog.OAuthServer, error)
	Refresh(ctx context.Context, tokenEndpoint, clientID, refreshToken, resource string) (mcpoauth.AccessToken, error)
	ClientCredentials(ctx context.Context, tokenEndpoint, clientID, clientSecret, resource string) (mcpoauth.AccessToken, error)
	TokenExchange(ctx context.Context, tokenEndpoint, subjectToken, audience, resource string) (mcpoauth.AccessToken, error)
}

// Signer signs a request with the team gateway's Pod Identity role (AWS SigV4).
type Signer interface {
	Sign(ctx context.Context, req *http.Request, body []byte, service, region string) error
}

// Config configures an Upstream.
type Config struct {
	Targets Targets
	State   State
	Tokens  Tokens
	Signer  Signer // nil refuses aws_sigv4 servers with federation_not_ready
	// TeamID and Namespace are the team gateway's own team.
	TeamID    string
	Namespace string
	// CredentialsDir holds the mounted credentials, one file per key (domain.McpCredentialKey).
	CredentialsDir string
	// FederationRoot holds the projected tokens per audience (domain.McpGatewayFederationRoot).
	FederationRoot string
	// Region is the default AWS region for SigV4 when the host names none.
	Region string
	// HTTP builds the client for a target; nil uses a client with the target's CA bundle and no redirects.
	HTTP    func(t Target) (*http.Client, error)
	Version string // the gateway's version, sent as clientInfo
	Now     func() time.Time
}

// Upstream implements mcpgateway.Upstream.
type Upstream struct {
	cfg Config

	mu       sync.Mutex
	sessions map[string]string // server id → upstream Mcp-Session-Id (2025-11-25)
	tokens   map[string]mcpoauth.AccessToken
	rotated  map[string]rotated // a refresh token the server rotated, until the mounted file changes
	clients  map[string]*http.Client
}

type rotated struct {
	from, to string
}

var _ mcpgateway.Upstream = (*Upstream)(nil)

// New returns an Upstream.
func New(cfg Config) *Upstream {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.FederationRoot == "" {
		cfg.FederationRoot = domain.McpGatewayFederationRoot
	}
	return &Upstream{cfg: cfg, sessions: map[string]string{}, tokens: map[string]mcpoauth.AccessToken{},
		rotated: map[string]rotated{}, clients: map[string]*http.Client{}}
}

// Errors the handler maps to the contract's refusals (gateway contract §13.4).
var (
	errReconnect  = &mcpgateway.UpstreamError{Status: http.StatusOK, Reason: "auth_reconnect_needed", Message: "the server's account must be connected again"}
	errFederation = &mcpgateway.UpstreamError{Status: http.StatusServiceUnavailable, Reason: "federation_not_ready", Message: "workload federation is not ready"}
	// ErrCredentialMissing means the credential the server needs is not mounted.
	ErrCredentialMissing = errors.New("mcpup: credential missing")
)

// URL is where the gateway reaches t.
func (u *Upstream) URL(t Target) string {
	if t.Transport == domain.McpTransportHosted {
		return domain.McpHostedURL(u.cfg.Namespace, t.Slug)
	}
	return t.URL
}

// version is the protocol the gateway speaks upstream: the one recorded at registration, else 2025-11-25, which
// every current server accepts.
func version(t Target) string {
	if mcpwire.IsSupported(t.ProtocolVersion) {
		return t.ProtocolVersion
	}
	return mcpwire.Version20251125
}

// Call implements mcpgateway.Upstream.
func (u *Upstream) Call(ctx context.Context, s mcpgateway.Server, m *mcpwire.Message) (*mcpwire.Message, error) {
	t, err := u.cfg.Targets.Target(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	if s.Limits.MaxResponseBytes > 0 {
		t.MaxResponse = s.Limits.MaxResponseBytes
	}
	c, err := u.client(ctx, t)
	if err != nil {
		return nil, err
	}
	var elicited bool
	onOther := func(x *mcpwire.Message) {
		if x.IsRequest() {
			elicited = elicited || x.Method == "elicitation/create"
			u.answerServerRequest(ctx, c, t, x)
		}
	}
	resp, err := u.send(ctx, c, t, m, onOther)
	if err != nil {
		return nil, err
	}
	if elicited && m.Method == "tools/call" {
		// An unattended agent cannot answer for a person (§13.7).
		res, _ := json.Marshal(map[string]any{"isError": true, "content": []map[string]any{{"type": "text",
			"text": "This server needs a person to answer. Unattended agents cannot."}}, "_meta": map[string]any{"io.afs/reason": "elicitation_unsupported"}})
		return &mcpwire.Message{JSONRPC: "2.0", ID: m.ID, Result: res}, nil
	}
	return resp, nil
}

// send calls the server, opening a 2025-11-25 session first when needed and once more when the server dropped it.
func (u *Upstream) send(ctx context.Context, c *mcpwire.Client, t Target, m *mcpwire.Message, onOther func(*mcpwire.Message)) (*mcpwire.Message, error) {
	if c.Version != mcpwire.Version20251125 {
		resp, err := c.Call(ctx, m, "", onOther)
		return resp, u.authFailed(ctx, t, err)
	}
	for attempt := 0; ; attempt++ {
		sid, err := u.session(ctx, c, t)
		if err != nil {
			return nil, u.authFailed(ctx, t, err)
		}
		resp, err := c.Call(ctx, m, sid, onOther)
		var he *mcpwire.HTTPError
		if attempt == 0 && errors.As(err, &he) && he.Status == http.StatusNotFound && sid != "" {
			u.mu.Lock()
			delete(u.sessions, t.ID)
			u.mu.Unlock()
			continue
		}
		return resp, u.authFailed(ctx, t, err)
	}
}

// authFailed drops a cached access token the server refused, so the next call mints a new one.
func (u *Upstream) authFailed(_ context.Context, t Target, err error) error {
	var he *mcpwire.HTTPError
	if errors.As(err, &he) && (he.Status == http.StatusUnauthorized || he.Status == http.StatusForbidden) {
		u.mu.Lock()
		delete(u.tokens, t.ID)
		u.mu.Unlock()
	}
	return err
}

// session returns the upstream session id, initialising one when there is none. Servers that answer without a
// session id are stateless, which is kept as "".
func (u *Upstream) session(ctx context.Context, c *mcpwire.Client, t Target) (string, error) {
	u.mu.Lock()
	sid, ok := u.sessions[t.ID]
	u.mu.Unlock()
	if ok {
		return sid, nil
	}
	params, _ := json.Marshal(map[string]any{"protocolVersion": c.Version, "capabilities": map[string]any{"elicitation": map[string]any{}},
		"clientInfo": map[string]any{"name": "afs-gateway", "version": u.cfg.Version}})
	init := &mcpwire.Message{JSONRPC: "2.0", ID: json.RawMessage(`"afs-init"`), Method: "initialize", Params: params}
	var got string
	hc := *c
	hc.HTTP = &http.Client{Transport: sessionCapture{next: transport(c.HTTP), got: &got}, Timeout: timeout(c.HTTP)}
	resp, err := hc.Call(ctx, init, "", nil)
	if err != nil {
		return "", err
	}
	if resp.Error != nil {
		return "", fmt.Errorf("mcpup: initialize refused: %d", resp.Error.Code)
	}
	if _, err := c.Call(ctx, &mcpwire.Message{JSONRPC: "2.0", Method: "notifications/initialized"}, got, nil); err != nil {
		return "", err
	}
	u.mu.Lock()
	u.sessions[t.ID] = got
	u.mu.Unlock()
	return got, nil
}

// sessionCapture reads Mcp-Session-Id from the initialize answer.
type sessionCapture struct {
	next http.RoundTripper
	got  *string
}

func (s sessionCapture) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.next.RoundTrip(r)
	if err == nil {
		*s.got = resp.Header.Get(mcpwire.HeaderSessionID)
	}
	return resp, err
}

func transport(c *http.Client) http.RoundTripper {
	if c != nil && c.Transport != nil {
		return c.Transport
	}
	return http.DefaultTransport
}

func timeout(c *http.Client) time.Duration {
	if c != nil {
		return c.Timeout
	}
	return 0
}

// answerServerRequest answers what an upstream asks of its client: elicitation is declined (no person is there),
// sampling and every other request are refused with method not found (§13.7). Errors are ignored: the upstream
// sees its own request time out.
func (u *Upstream) answerServerRequest(ctx context.Context, c *mcpwire.Client, t Target, x *mcpwire.Message) {
	var reply *mcpwire.Message
	if x.Method == "elicitation/create" {
		res, _ := json.Marshal(map[string]string{"action": "decline"})
		reply = &mcpwire.Message{JSONRPC: "2.0", ID: x.ID, Result: res}
	} else {
		reply = mcpwire.NewError(x.ID, mcpwire.CodeMethodNotFound, "not supported by afs-gateway", nil)
	}
	u.mu.Lock()
	sid := u.sessions[t.ID]
	u.mu.Unlock()
	_, _ = c.Call(context.WithoutCancel(ctx), reply, sid, nil)
}

// client builds the wire client of t with its credential.
func (u *Upstream) client(ctx context.Context, t Target) (*mcpwire.Client, error) {
	hc, err := u.httpClient(t)
	if err != nil {
		return nil, err
	}
	c := &mcpwire.Client{HTTP: hc, URL: u.URL(t), Version: version(t), MaxResponse: t.MaxResponse}
	switch t.AuthKind {
	case domain.McpAuthNone, "":
	case domain.McpAuthStatic:
		v, err := u.file(t)
		if err != nil {
			return nil, err
		}
		name, val := "Authorization", "Bearer "+v
		if t.AuthHeader != "" {
			name, val = t.AuthHeader, v
		}
		c.Header = func(h http.Header) { h.Set(name, val) }
	case domain.McpAuthOAuthUser, domain.McpAuthOAuthClient, domain.McpAuthWorkloadFederation:
		tok, err := u.access(ctx, t)
		if err != nil {
			return nil, err
		}
		c.Header = func(h http.Header) { h.Set("Authorization", "Bearer "+tok) }
	case domain.McpAuthAWSSigV4:
		if u.cfg.Signer == nil {
			return nil, errFederation
		}
		service, region := sigv4Scope(c.URL, u.cfg.Region)
		c.Sign = func(req *http.Request, body []byte) error { return u.cfg.Signer.Sign(ctx, req, body, service, region) }
	default:
		return nil, ErrCredentialMissing
	}
	return c, nil
}

// sigv4Scope reads the service and region from an AWS host (<service>.<region>.amazonaws.com and the like);
// other hosts sign for execute-api in the gateway's region.
func sigv4Scope(raw, defRegion string) (service, region string) {
	host := raw
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/:"); i >= 0 {
		host = host[:i]
	}
	labels := strings.Split(strings.ToLower(host), ".")
	for i := len(labels) - 1; i >= 2; i-- {
		if labels[i] == "amazonaws" || labels[i] == "api" && i+1 < len(labels) && labels[i+1] == "aws" {
			return labels[i-2], labels[i-1]
		}
	}
	return "execute-api", defRegion
}

func (u *Upstream) httpClient(t Target) (*http.Client, error) {
	if u.cfg.HTTP != nil {
		return u.cfg.HTTP(t)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	key := t.ID + "\x00" + t.CABundle
	if c, ok := u.clients[key]; ok {
		return c, nil
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if t.CABundle != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(t.CABundle)) {
			return nil, errors.New("mcpup: the server's CA bundle holds no certificate")
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	c := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	u.clients[key] = c
	return c, nil
}

// file reads the server's mounted credential.
func (u *Upstream) file(t Target) (string, error) {
	if u.cfg.CredentialsDir == "" || domain.ValidMcpSlug(t.Slug) != nil {
		return "", ErrCredentialMissing
	}
	b, err := os.ReadFile(filepath.Join(u.cfg.CredentialsDir, domain.McpCredentialKey(t.Slug))) //nolint:gosec // mount dir and a validated slug
	if err != nil {
		return "", ErrCredentialMissing
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", ErrCredentialMissing
	}
	return v, nil
}

// refreshBefore is how long before expiry an access token is minted again.
const refreshBefore = 2 * time.Minute

// access returns a cached access token or mints one.
func (u *Upstream) access(ctx context.Context, t Target) (string, error) {
	now := u.cfg.Now()
	u.mu.Lock()
	if tok, ok := u.tokens[t.ID]; ok && now.Add(refreshBefore).Before(tok.Expires) {
		u.mu.Unlock()
		return tok.Value, nil
	}
	u.mu.Unlock()
	if u.cfg.Tokens == nil {
		return "", ErrCredentialMissing
	}
	var tok mcpoauth.AccessToken
	var err error
	switch t.AuthKind {
	case domain.McpAuthOAuthUser:
		tok, err = u.refresh(ctx, t)
	case domain.McpAuthOAuthClient:
		tok, err = u.clientCredentials(ctx, t)
	default:
		tok, err = u.federate(ctx, t)
	}
	if err != nil {
		return "", err
	}
	u.mu.Lock()
	u.tokens[t.ID] = tok
	u.mu.Unlock()
	return tok.Value, nil
}

func (u *Upstream) scope(t Target) string {
	if t.TeamID != "" {
		return t.TeamID
	}
	if t.CredentialMode == domain.McpCredentialShared {
		return domain.McpSharedScope
	}
	return u.cfg.TeamID
}

// oauthSecret is the oauth_user secret afs-api writes at connect (mcpcatalog).
type oauthSecret struct {
	RefreshToken  string `json:"refreshToken"`
	TokenEndpoint string `json:"tokenEndpoint"`
	ClientID      string `json:"clientId"`
	Resource      string `json:"resource"`
}

func (u *Upstream) refresh(ctx context.Context, t Target) (mcpoauth.AccessToken, error) {
	raw, err := u.file(t)
	if err != nil {
		return mcpoauth.AccessToken{}, errReconnect
	}
	var s oauthSecret
	if json.Unmarshal([]byte(raw), &s) != nil || s.RefreshToken == "" || s.TokenEndpoint == "" {
		return mcpoauth.AccessToken{}, errReconnect
	}
	rt := s.RefreshToken
	u.mu.Lock()
	if r, ok := u.rotated[t.ID]; ok && r.from == s.RefreshToken {
		rt = r.to
	}
	u.mu.Unlock()
	tok, err := u.cfg.Tokens.Refresh(ctx, s.TokenEndpoint, s.ClientID, rt, s.Resource)
	scope := u.scope(t)
	if errors.Is(err, mcpoauth.ErrInvalidGrant) {
		if u.cfg.State != nil {
			_ = u.cfg.State.MarkMcpCredential(context.WithoutCancel(ctx), t.ID, scope, "expired")
		}
		return mcpoauth.AccessToken{}, errReconnect
	}
	if err != nil {
		return mcpoauth.AccessToken{}, err
	}
	if tok.Refresh != "" && tok.Refresh != rt {
		// The secret cannot be rewritten from here (afs-gateway has no Secrets Manager write); the rotated token is
		// kept in memory until the mounted file changes or the pod restarts.
		u.mu.Lock()
		u.rotated[t.ID] = rotated{from: s.RefreshToken, to: tok.Refresh}
		u.mu.Unlock()
	}
	if u.cfg.State != nil {
		_ = u.cfg.State.MarkMcpCredential(context.WithoutCancel(ctx), t.ID, scope, "set")
	}
	return tok, nil
}

func (u *Upstream) clientCredentials(ctx context.Context, t Target) (mcpoauth.AccessToken, error) {
	raw, err := u.file(t)
	if err != nil {
		return mcpoauth.AccessToken{}, err
	}
	var s struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if json.Unmarshal([]byte(raw), &s) != nil || s.ClientID == "" || s.ClientSecret == "" {
		return mcpoauth.AccessToken{}, ErrCredentialMissing
	}
	as, err := u.cfg.Tokens.Discover(ctx, u.URL(t))
	if err != nil {
		return mcpoauth.AccessToken{}, err
	}
	return u.cfg.Tokens.ClientCredentials(ctx, as.TokenEndpoint, s.ClientID, s.ClientSecret, as.Resource)
}

func (u *Upstream) federate(ctx context.Context, t Target) (mcpoauth.AccessToken, error) {
	f := t.Federation
	if f == nil || f.TokenEndpoint == "" {
		return mcpoauth.AccessToken{}, errFederation
	}
	report := func(status, msg string) {
		if u.cfg.State != nil {
			_ = u.cfg.State.ReportMcpFederation(context.WithoutCancel(ctx), t.ID, u.cfg.TeamID, status, msg)
		}
	}
	b, err := os.ReadFile(filepath.Join(u.cfg.FederationRoot, domain.McpFederationDir(f.Audience), "token")) //nolint:gosec // fixed root and a sanitised name
	if err != nil || strings.TrimSpace(string(b)) == "" {
		report("failing", "the projected token for the audience is not mounted")
		return mcpoauth.AccessToken{}, errFederation
	}
	tok, err := u.cfg.Tokens.TokenExchange(ctx, f.TokenEndpoint, strings.TrimSpace(string(b)), "", u.URL(t))
	if err != nil {
		msg := "token exchange refused"
		if !errors.Is(err, mcpoauth.ErrInvalidGrant) {
			msg = "token endpoint unavailable"
		}
		report("failing", msg)
		return mcpoauth.AccessToken{}, errFederation
	}
	report("ready", "")
	return tok, nil
}

// Lister returns an inventory lister of t for the refresh.
func (u *Upstream) Lister(ctx context.Context, t Target) (mcpinventory.Lister, error) {
	c, err := u.client(ctx, t)
	if err != nil {
		return nil, err
	}
	return lister{u: u, c: c, t: t}, nil
}

type lister struct {
	u *Upstream
	c *mcpwire.Client
	t Target
}

func (l lister) List(ctx context.Context, method, cursor string) ([]json.RawMessage, string, error) {
	p := map[string]any{}
	if cursor != "" {
		p["cursor"] = cursor
	}
	params, _ := json.Marshal(p)
	resp, err := l.u.send(ctx, l.c, l.t, &mcpwire.Message{JSONRPC: "2.0", ID: json.RawMessage(`"afs-list"`), Method: method, Params: params}, nil)
	if err != nil {
		return nil, "", err
	}
	if resp.Error != nil {
		if resp.Error.Code == mcpwire.CodeMethodNotFound {
			return nil, "", mcpinventory.ErrUnsupported
		}
		return nil, "", fmt.Errorf("mcpup: %s: jsonrpc %d", method, resp.Error.Code)
	}
	field := ""
	for _, ls := range mcpinventory.Lists {
		if ls.Method == method {
			field = ls.Field
		}
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		return nil, "", fmt.Errorf("mcpup: %s: result is not an object", method)
	}
	var items []json.RawMessage
	if raw, ok := out[field]; ok {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, "", fmt.Errorf("mcpup: %s: %s is not a list", method, field)
		}
	}
	var next string
	_ = json.Unmarshal(out["nextCursor"], &next)
	return items, next, nil
}

var _ Tokens = (*mcpoauth.Client)(nil)

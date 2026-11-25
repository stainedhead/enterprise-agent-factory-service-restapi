// Package mcpoauth is the outbound OAuth client of the MCP connect flow: discovery by Protected Resource Metadata
// (RFC 9728) and Authorization Server Metadata (RFC 8414, with the OpenID Connect discovery fallback), and the
// authorization code grant with PKCE (RFC 7636) and a resource indicator (RFC 8707). Every URL must be https.
// Responses are size-limited, and no response body appears in an error, because a token endpoint may echo secrets.
package mcpoauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
)

const maxBody = 64 << 10

// Client implements mcpcatalog.OAuthClient.
type Client struct {
	// HTTP sends the requests; nil uses a client with a 10-second timeout.
	HTTP *http.Client
}

var _ mcpcatalog.OAuthClient = (*Client)(nil)

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func fail(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{mcpcatalog.ErrOAuthUpstream}, args...)...)
}

// httpsURL parses u and requires https with a host.
func httpsURL(u string) (*url.URL, error) {
	p, err := url.Parse(u)
	if err != nil || p.Scheme != "https" || p.Host == "" || p.User != nil {
		return nil, fail("not an https URL: %q", u)
	}
	return p, nil
}

// wellKnown inserts /.well-known/<name> between the host and the path (RFC 9728 §3.1, RFC 8414 §3.1).
func wellKnown(p *url.URL, name string) string {
	path := strings.TrimSuffix(p.EscapedPath(), "/")
	return p.Scheme + "://" + p.Host + "/.well-known/" + name + path
}

func (c *Client) getJSON(ctx context.Context, u string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fail("request: %v", err)
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.http().Do(req)
	if err != nil {
		return fail("GET %s: %v", u, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fail("GET %s: status %d", u, res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(dst); err != nil {
		return fail("GET %s: not JSON metadata", u)
	}
	return nil
}

type resourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

type serverMetadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

// Discover implements mcpcatalog.OAuthClient.
func (c *Client) Discover(ctx context.Context, resourceURL string) (mcpcatalog.OAuthServer, error) {
	res, err := httpsURL(resourceURL)
	if err != nil {
		return mcpcatalog.OAuthServer{}, err
	}
	var prm resourceMetadata
	err = c.getJSON(ctx, wellKnown(res, "oauth-protected-resource"), &prm)
	if err != nil && strings.Trim(res.Path, "/") != "" {
		err = c.getJSON(ctx, res.Scheme+"://"+res.Host+"/.well-known/oauth-protected-resource", &prm)
	}
	if err != nil {
		return mcpcatalog.OAuthServer{}, err
	}
	// RFC 9728 §3.3: the metadata is about this resource.
	pr, err := httpsURL(prm.Resource)
	if err != nil || !strings.EqualFold(pr.Host, res.Host) {
		return mcpcatalog.OAuthServer{}, fail("protected resource metadata names another resource")
	}
	if len(prm.AuthorizationServers) == 0 {
		return mcpcatalog.OAuthServer{}, fail("protected resource metadata names no authorization server")
	}
	issuer := prm.AuthorizationServers[0]
	ip, err := httpsURL(issuer)
	if err != nil {
		return mcpcatalog.OAuthServer{}, err
	}
	var asm serverMetadata
	err = c.getJSON(ctx, wellKnown(ip, "oauth-authorization-server"), &asm)
	if err != nil {
		err = c.getJSON(ctx, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", &asm)
	}
	if err != nil {
		return mcpcatalog.OAuthServer{}, err
	}
	// RFC 8414 §3.3: the issuer in the metadata is the one it was fetched for.
	if asm.Issuer != issuer {
		return mcpcatalog.OAuthServer{}, fail("authorization server metadata names issuer %q, want %q", asm.Issuer, issuer)
	}
	if _, err := httpsURL(asm.AuthorizationEndpoint); err != nil {
		return mcpcatalog.OAuthServer{}, err
	}
	if _, err := httpsURL(asm.TokenEndpoint); err != nil {
		return mcpcatalog.OAuthServer{}, err
	}
	// MCP authorization: a client must refuse a server that does not advertise S256 PKCE.
	if !slices.Contains(asm.CodeChallengeMethodsSupported, "S256") {
		return mcpcatalog.OAuthServer{}, fail("the authorization server does not advertise PKCE S256")
	}
	return mcpcatalog.OAuthServer{Issuer: asm.Issuer, AuthorizationEndpoint: asm.AuthorizationEndpoint,
		TokenEndpoint: asm.TokenEndpoint, Resource: prm.Resource, Scopes: prm.ScopesSupported}, nil
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token"`
	Error        string `json:"error"`
}

// ErrInvalidGrant means the token endpoint refused the grant itself (RFC 6749 §5.2 invalid_grant): a refresh
// token that expired or was revoked, or a federated assertion the server does not trust.
var ErrInvalidGrant = errors.New("mcpoauth: invalid_grant")

// postToken posts a form to a token endpoint and decodes the answer. A 400 or 401 whose error is invalid_grant
// wraps ErrInvalidGrant; no body text ever reaches the error.
func (c *Client) postToken(ctx context.Context, endpoint string, form url.Values, basic [2]string) (tokenResponse, error) {
	if _, err := httpsURL(endpoint); err != nil {
		return tokenResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, fail("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic[0] != "" {
		req.SetBasicAuth(url.QueryEscape(basic[0]), url.QueryEscape(basic[1]))
	}
	res, err := c.http().Do(req)
	if err != nil {
		return tokenResponse{}, fail("token endpoint: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	var tr tokenResponse
	decErr := json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(&tr)
	if res.StatusCode != http.StatusOK {
		if decErr == nil && tr.Error == "invalid_grant" && (res.StatusCode == http.StatusBadRequest || res.StatusCode == http.StatusUnauthorized) {
			return tokenResponse{}, errors.Join(fail("token endpoint: invalid_grant"), ErrInvalidGrant)
		}
		return tokenResponse{}, fail("token endpoint: status %d", res.StatusCode)
	}
	if decErr != nil {
		return tokenResponse{}, fail("token endpoint: not a JSON token response")
	}
	return tr, nil
}

// Exchange implements mcpcatalog.OAuthClient. The access token in the response is dropped: the gateway mints its own
// from the refresh token per call.
func (c *Client) Exchange(ctx context.Context, as mcpcatalog.OAuthServer, x mcpcatalog.OAuthExchange) (mcpcatalog.OAuthTokens, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {x.Code}, "redirect_uri": {x.RedirectURI},
		"client_id": {x.ClientID}, "code_verifier": {x.Verifier}}
	if x.Resource != "" {
		form.Set("resource", x.Resource)
	}
	tr, err := c.postToken(ctx, as.TokenEndpoint, form, [2]string{})
	if err != nil {
		return mcpcatalog.OAuthTokens{}, err
	}
	out := mcpcatalog.OAuthTokens{Scopes: strings.Fields(tr.Scope), Subject: subjectOf(tr.IDToken)}
	if tr.RefreshToken != "" {
		if out.RefreshToken, err = domain.NewSecretValue("refreshToken", tr.RefreshToken); err != nil {
			return mcpcatalog.OAuthTokens{}, fail("token endpoint: refresh token unusable")
		}
	}
	return out, nil
}

// AccessToken is a short-lived token the gateway sends upstream. It lives in memory only.
type AccessToken struct {
	Value   string
	Expires time.Time
	// Refresh is a rotated refresh token, when the server issued one (RFC 6749 §6).
	Refresh string
}

// Token grant types the gateway uses (URNs, not credentials).
//
//nolint:gosec // grant and token type URNs
const (
	grantRefresh       = "refresh_token"
	grantClient        = "client_credentials"
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeJWT       = "urn:ietf:params:oauth:token-type:jwt"
	tokenTypeAccess    = "urn:ietf:params:oauth:token-type:access_token"
)

func (c *Client) access(ctx context.Context, endpoint string, form url.Values, basic [2]string, now time.Time) (AccessToken, error) {
	tr, err := c.postToken(ctx, endpoint, form, basic)
	if err != nil {
		return AccessToken{}, err
	}
	if tr.AccessToken == "" {
		return AccessToken{}, fail("token endpoint: no access token")
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 || ttl > 24*time.Hour {
		ttl = time.Hour
	}
	return AccessToken{Value: tr.AccessToken, Expires: now.Add(ttl), Refresh: tr.RefreshToken}, nil
}

// Refresh mints an access token from a user-delegated refresh token (RFC 6749 §6), for the resource (RFC 8707).
func (c *Client) Refresh(ctx context.Context, tokenEndpoint, clientID, refreshToken, resource string) (AccessToken, error) {
	form := url.Values{"grant_type": {grantRefresh}, "refresh_token": {refreshToken}, "client_id": {clientID}}
	if resource != "" {
		form.Set("resource", resource)
	}
	return c.access(ctx, tokenEndpoint, form, [2]string{}, time.Now())
}

// ClientCredentials mints an access token with the client credentials grant (RFC 6749 §4.4), the client
// authenticating with HTTP Basic (client_secret_basic), for the resource.
func (c *Client) ClientCredentials(ctx context.Context, tokenEndpoint, clientID, clientSecret, resource string) (AccessToken, error) {
	form := url.Values{"grant_type": {grantClient}}
	if resource != "" {
		form.Set("resource", resource)
	}
	return c.access(ctx, tokenEndpoint, form, [2]string{clientID, clientSecret}, time.Now())
}

// TokenExchange trades the gateway's projected ServiceAccount token for an access token to the resource (RFC 8693).
func (c *Client) TokenExchange(ctx context.Context, tokenEndpoint, subjectToken, audience, resource string) (AccessToken, error) {
	form := url.Values{"grant_type": {grantTokenExchange}, "subject_token": {subjectToken}, "subject_token_type": {tokenTypeJWT},
		"requested_token_type": {tokenTypeAccess}}
	if audience != "" {
		form.Set("audience", audience)
	}
	if resource != "" {
		form.Set("resource", resource)
	}
	return c.access(ctx, tokenEndpoint, form, [2]string{}, time.Now())
}

// subjectOf reads a display name from an ID token's claims. The token is not verified: the name is shown, never used
// for a decision.
func subjectOf(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Sub               string `json:"sub"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	for _, v := range []string{claims.Email, claims.PreferredUsername, claims.Sub} {
		if v != "" {
			if len(v) > 200 {
				v = v[:200]
			}
			return v
		}
	}
	return ""
}

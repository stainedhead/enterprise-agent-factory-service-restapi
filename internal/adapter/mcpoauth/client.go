// Package mcpoauth is the outbound OAuth client of the MCP connect flow: discovery by Protected Resource Metadata
// (RFC 9728) and Authorization Server Metadata (RFC 8414, with the OpenID Connect discovery fallback), and the
// authorization code grant with PKCE (RFC 7636) and a resource indicator (RFC 8707). Every URL must be https.
// Responses are size-limited, and no response body appears in an error, because a token endpoint may echo secrets.
package mcpoauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token"`
}

// Exchange implements mcpcatalog.OAuthClient. The access token in the response is dropped: the gateway mints its own
// from the refresh token per call.
func (c *Client) Exchange(ctx context.Context, as mcpcatalog.OAuthServer, x mcpcatalog.OAuthExchange) (mcpcatalog.OAuthTokens, error) {
	if _, err := httpsURL(as.TokenEndpoint); err != nil {
		return mcpcatalog.OAuthTokens{}, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {x.Code}, "redirect_uri": {x.RedirectURI},
		"client_id": {x.ClientID}, "code_verifier": {x.Verifier}}
	if x.Resource != "" {
		form.Set("resource", x.Resource)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, as.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return mcpcatalog.OAuthTokens{}, fail("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := c.http().Do(req)
	if err != nil {
		return mcpcatalog.OAuthTokens{}, fail("token endpoint: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return mcpcatalog.OAuthTokens{}, fail("token endpoint: status %d", res.StatusCode)
	}
	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(res.Body, maxBody)).Decode(&tr); err != nil {
		return mcpcatalog.OAuthTokens{}, fail("token endpoint: not a JSON token response")
	}
	out := mcpcatalog.OAuthTokens{Scopes: strings.Fields(tr.Scope), Subject: subjectOf(tr.IDToken)}
	if tr.RefreshToken != "" {
		if out.RefreshToken, err = domain.NewSecretValue("refreshToken", tr.RefreshToken); err != nil {
			return mcpcatalog.OAuthTokens{}, fail("token endpoint: refresh token unusable")
		}
	}
	return out, nil
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

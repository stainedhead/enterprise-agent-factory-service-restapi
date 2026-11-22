package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
)

// ClientSecrets reads a team's collaboration client secret (a Secrets Manager ARN). The value is used for
// one token exchange and is never logged, returned or stored.
type ClientSecrets interface {
	Secret(ctx context.Context, arn string) (string, error)
}

// CodeFlow is the authorization code + PKCE client of afs-edge. It implements edge.Identity. Token
// validation reuses the Verifier's key sets: signature algorithms are asymmetric only, keys are cached.
type CodeFlow struct {
	v       *Verifier
	secrets ClientSecrets

	mu   sync.Mutex
	disc map[string]discovery
}

type discovery struct {
	AuthEndpoint  string `json:"authorization_endpoint"`
	TokenEndpoint string `json:"token_endpoint"`
	fetched       time.Time
}

// NewCodeFlow returns the client; v validates tokens, secrets supplies client secrets.
func NewCodeFlow(v *Verifier, secrets ClientSecrets) *CodeFlow {
	return &CodeFlow{v: v, secrets: secrets, disc: map[string]discovery{}}
}

var _ edge.Identity = (*CodeFlow)(nil)

func (c *CodeFlow) discovery(ctx context.Context, issuer string) (discovery, error) {
	issuer = normalize(issuer)
	c.mu.Lock()
	d, ok := c.disc[issuer]
	c.mu.Unlock()
	if ok && c.v.opts.Now().Sub(d.fetched) < c.v.opts.TTL {
		return d, nil
	}
	if err := c.v.getJSON(ctx, issuer+"/.well-known/openid-configuration", &d); err != nil {
		return discovery{}, fmt.Errorf("oidc discovery: %w", err)
	}
	for _, e := range []string{d.AuthEndpoint, d.TokenEndpoint} {
		u, err := url.Parse(e)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return discovery{}, errors.New("oidc discovery: endpoint is not an https URL")
		}
	}
	d.fetched = c.v.opts.Now()
	c.mu.Lock()
	c.disc[issuer] = d
	c.mu.Unlock()
	return d, nil
}

// AuthURL implements edge.Identity.
func (c *CodeFlow) AuthURL(ctx context.Context, r collab.Route, redirectURI, state, challenge, nonce, prompt string) (string, error) {
	if r.IssuerURL == "" || r.ClientID == "" {
		return "", errors.New("oidc: the team has no collaboration client")
	}
	d, err := c.discovery(ctx, r.IssuerURL)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(d.AuthEndpoint)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", r.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	if prompt != "" {
		q.Set("prompt", prompt)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Exchange implements edge.Identity.
func (c *CodeFlow) Exchange(ctx context.Context, r collab.Route, redirectURI, code, verifier string) (edge.Claims, error) {
	d, err := c.discovery(ctx, r.IssuerURL)
	if err != nil {
		return edge.Claims{}, err
	}
	secret, err := c.secrets.Secret(ctx, r.ClientSecretARN)
	if err != nil {
		return edge.Claims{}, errors.New("oidc: the client secret is unavailable")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI},
		"client_id": {r.ClientID}, "client_secret": {secret}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return edge.Claims{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.v.opts.HTTP.Do(req)
	if err != nil {
		return edge.Claims{}, errors.New("oidc: token endpoint unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, maxDocument)).Decode(&tok) != nil || tok.IDToken == "" {
		return edge.Claims{}, fmt.Errorf("oidc: token endpoint answered %d", resp.StatusCode)
	}
	cl, err := c.verify(ctx, r, tok.IDToken, []string{r.ClientID})
	if err != nil {
		return edge.Claims{}, err
	}
	return cl, nil
}

// VerifyBearer implements edge.Identity.
//
// A bearer token is checked with the rules afs-api uses (Verifier.Verify): the audience must contain the
// provider's own client id, the console client id or the CLI public client id. The collaboration client id is
// accepted as well when the provider has one, so a token issued to it works too (empty ids never match); the REST API surface itself needs no collaboration client (finding 6).
func (c *CodeFlow) VerifyBearer(ctx context.Context, r collab.Route, token string) (edge.Claims, error) {
	return c.verify(ctx, r, token, []string{r.ProviderClientID, r.ProviderCLIClientID, r.ClientID})
}

// verify checks signature, issuer, audience (one of aud, empty entries ignored) and times of a token.
func (c *CodeFlow) verify(ctx context.Context, r collab.Route, token string, aud []string) (edge.Claims, error) {
	bad := &edge.TokenError{}
	jws, err := jose.ParseSignedCompact(token, signatureAlgorithms)
	if err != nil || len(jws.Signatures) != 1 || r.IssuerURL == "" {
		return edge.Claims{}, bad
	}
	k, err := c.v.key(ctx, normalize(r.IssuerURL), jws.Signatures[0].Header.KeyID)
	if err != nil {
		return edge.Claims{}, bad
	}
	payload, err := jws.Verify(k)
	if err != nil {
		return edge.Claims{}, bad
	}
	var cl claims
	var extra struct {
		Nonce string `json:"nonce"`
	}
	if json.Unmarshal(payload, &cl) != nil || json.Unmarshal(payload, &extra) != nil {
		return edge.Claims{}, bad
	}
	if normalize(cl.Issuer) != normalize(r.IssuerURL) || cl.Subject == "" {
		return edge.Claims{}, bad
	}
	if !anyAudience(audiences(cl.Audience), aud) {
		return edge.Claims{}, bad
	}
	now := c.v.opts.Now()
	if cl.Expiry == nil {
		return edge.Claims{}, bad
	}
	if !now.Before(unix(*cl.Expiry).Add(c.v.opts.Leeway)) {
		return edge.Claims{}, &edge.TokenError{Expired: true}
	}
	if cl.NotBefore != nil && now.Add(c.v.opts.Leeway).Before(unix(*cl.NotBefore)) {
		return edge.Claims{}, bad
	}
	return edge.Claims{Issuer: cl.Issuer, Subject: cl.Subject, Groups: groups(payload, r.GroupsClaim), Nonce: extra.Nonce}, nil
}

// anyAudience reports whether the token's audiences include one of the accepted, non-empty ones.
func anyAudience(have, accepted []string) bool {
	for _, a := range accepted {
		if a != "" && slices.Contains(have, a) {
			return true
		}
	}
	return false
}

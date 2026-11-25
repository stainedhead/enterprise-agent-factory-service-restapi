package main

import (
	"context"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpoauth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
)

// wireMcp builds the governed MCP server use cases (release 1.3.0, contract §4.14) and binds their handlers.
// Credentials go through the shared Secrets Manager writer (wireSecrets runs first). The OAuth connect verifier is
// keyed by a subkey of the page token key, so every replica derives the same one.
func wireMcp(c *components, s settings) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	sub := hmac.New(sha256.New, c.PageKey)
	sub.Write([]byte("afs-mcp-oauth-state-key"))
	c.bind(httpapi.BindMcp(&httpapi.McpAPI{
		Mcp: &mcpcatalog.Service{
			Deps: deps, Repo: postgres.NewMcpRepo(), Secrets: mcpSecrets{c.SecretsWriter}, ClusterIssuer: s.ClusterOIDCIssuer,
			OAuthHTTP: &mcpoauth.Client{HTTP: s.McpOAuth.HTTP},
			OAuth: mcpcatalog.OAuthSettings{ClientID: s.McpOAuth.ClientID, RedirectURI: s.McpOAuth.RedirectURI,
				ConsoleURL: s.McpOAuth.ConsoleURL, StateKey: sub.Sum(nil), Rand: crand.Reader},
		},
		Pages: httpapi.NewPageTokens(c.PageKey, nil),
	}))
	c.bind(httpapi.BindMcpPending())
}

// mcpSecrets narrows the Secrets Manager writer to the MCP credential port.
type mcpSecrets struct{ w secrets.Writer }

func (m mcpSecrets) Create(ctx context.Context, name string, v domain.SecretValue, token string, tags map[string]string) (string, error) {
	out, err := m.w.Create(ctx, name, v, token, tags)
	return out.ARN, err
}

func (m mcpSecrets) PutValue(ctx context.Context, arn string, v domain.SecretValue, token string) error {
	_, err := m.w.PutValue(ctx, arn, v, token)
	return err
}

func (m mcpSecrets) ForceDelete(ctx context.Context, arn string) error {
	return m.w.ForceDelete(ctx, arn)
}

func (m mcpSecrets) ScheduleDeletion(ctx context.Context, arn string, days int) error {
	return m.w.ScheduleDeletion(ctx, arn, days)
}

// mcpOAuthConfig is the environment's OAuth client for MCP connect.
type mcpOAuthConfig struct {
	// ClientID is the https URL of AFS's Client ID Metadata Document.
	ClientID string
	// RedirectURI is the public https URL of GET /v1/mcp-oauth/callback.
	RedirectURI string
	// ConsoleURL is the console's https base URL.
	ConsoleURL string
	// HTTP reaches authorization servers; nil uses a client with a 10 s timeout. Tests point it at a fake.
	HTTP *http.Client
}

// mcpOAuthSettings reads AFS_MCP_OAUTH_CLIENT_ID, AFS_MCP_OAUTH_REDIRECT_URI and AFS_CONSOLE_URL. All three or none:
// a partial setup is a startup error.
func mcpOAuthSettings(env func(string) string, s *settings) error {
	names := []string{"AFS_MCP_OAUTH_CLIENT_ID", "AFS_MCP_OAUTH_REDIRECT_URI", "AFS_CONSOLE_URL"}
	vals := make([]string, len(names))
	set := 0
	for i, n := range names {
		vals[i] = strings.TrimSpace(env(n))
		if vals[i] == "" {
			continue
		}
		set++
		u, err := url.Parse(vals[i])
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return fmt.Errorf("%s: %q is not an https URL", n, vals[i])
		}
	}
	if set != 0 && set != len(names) {
		return fmt.Errorf("MCP OAuth connect needs all of %s", strings.Join(names, ", "))
	}
	s.McpOAuth.ClientID, s.McpOAuth.RedirectURI, s.McpOAuth.ConsoleURL = vals[0], vals[1], vals[2]
	return nil
}

package main

import (
	"context"
	crand "crypto/rand"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
)

// wireMcp builds the governed MCP server use cases (release 1.3.0, contract §4.14) and binds their handlers.
// Credentials go through the shared Secrets Manager writer (wireSecrets runs first); the two OAuth connect
// operations stay pending.
func wireMcp(c *components, s settings) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	c.bind(httpapi.BindMcp(&httpapi.McpAPI{
		Mcp: &mcpcatalog.Service{
			Deps: deps, Repo: postgres.NewMcpRepo(), Secrets: mcpSecrets{c.SecretsWriter}, ClusterIssuer: s.ClusterOIDCIssuer,
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

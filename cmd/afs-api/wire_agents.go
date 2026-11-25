package main

import (
	"context"
	crand "crypto/rand"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
)

// wireAgents binds the agent operations (M2-B).
func wireAgents(c *components, log *slog.Logger) *agents.Service {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	svc := &agents.Service{Deps: deps, Repo: postgres.NewAgentRepo(), Rand: crand.Reader, Mcp: postgres.NewMcpAccessRepo()}
	if c.SecretsWriter != nil {
		svc.Boot = bootWriter{c.SecretsWriter}
		svc.OnOrphan = func() { log.Warn("an orchestration boot secret was left for the orphan sweep") }
		if c.teamSvc != nil {
			c.teamSvc.Boot = svc
		}
	}
	c.bind(httpapi.BindAgents(&httpapi.AgentsAPI{Svc: svc,
		Pages: httpapi.NewPageTokens(c.PageKey, nil)}))
	return svc
}

// bootWriter adapts the Secrets Manager writer of the secrets use case to agents.BootWriter.
type bootWriter struct{ w secrets.Writer }

func (b bootWriter) Create(ctx context.Context, name string, v domain.SecretValue, token string, tags map[string]string) (agents.BootCreated, error) {
	cr, err := b.w.Create(ctx, name, v, token, tags)
	return agents.BootCreated{ARN: cr.ARN, VersionID: cr.VersionID}, err
}
func (b bootWriter) ForceDelete(ctx context.Context, arn string) error {
	return b.w.ForceDelete(ctx, arn)
}
func (b bootWriter) TagOrphan(ctx context.Context, arn string) error { return b.w.TagOrphan(ctx, arn) }

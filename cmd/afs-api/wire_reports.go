package main

import (
	crand "crypto/rand"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reports"
)

// wireReports builds the report use cases (M3-H) and binds their handlers.
func wireReports(c *components, log *slog.Logger) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	pages := httpapi.NewPageTokens(pageTokenKey(log), nil)
	httpapi.BindReports(&httpapi.ReportsAPI{Svc: &reports.Service{Deps: deps, Repo: postgres.NewReportsRepo()}, Pages: pages})
}

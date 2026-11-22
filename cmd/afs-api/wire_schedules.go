package main

import (
	crand "crypto/rand"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reportschedules"
)

// wireSchedules builds the report schedule use cases (M4-E2) and binds their handlers. afs-api only
// stores schedules; afs-billing runs them, and the API holds no S3 client.
func wireSchedules(c *components, log *slog.Logger) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	pages := httpapi.NewPageTokens(c.PageKey, nil)
	c.bind(httpapi.BindSchedules(&httpapi.SchedulesAPI{Svc: &reportschedules.Service{Deps: deps, Repo: postgres.NewReportSchedulesRepo()}, Pages: pages}))
}

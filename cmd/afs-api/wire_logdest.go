package main

import (
	crand "crypto/rand"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/logdest"
)

// wireLogDest builds the log destination use cases (M2-I1) on the database components and binds
// their handlers.
func wireLogDest(c *components, log *slog.Logger) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	pages := httpapi.NewPageTokens(pageTokenKey(log), nil)
	httpapi.BindLogDestinations(&httpapi.LogDestinationsAPI{Svc: &logdest.Service{Deps: deps, Repo: postgres.NewLogDestRepo()}, Pages: pages})
}

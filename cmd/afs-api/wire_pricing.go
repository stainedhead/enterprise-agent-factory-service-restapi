package main

import (
	crand "crypto/rand"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pricing"
)

// wirePricing builds the price proposal use cases (M4-D2) and binds their handlers.
func wirePricing(c *components, log *slog.Logger) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	pages := httpapi.NewPageTokens(pageTokenKey(log), nil)
	httpapi.BindPricing(&httpapi.PricingAPI{Svc: &pricing.Service{Deps: deps, Repo: postgres.NewPricingRepo()}, Pages: pages})
}

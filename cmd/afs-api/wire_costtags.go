package main

import (
	crand "crypto/rand"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/costtags"
)

// wireCostTags builds the cost tag status use case (M5-F2) and binds its handler. afs-api only reads the
// table; afs-billing's daily check writes it.
func wireCostTags(c *components, log *slog.Logger) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	c.bind(httpapi.BindCostTags(&httpapi.CostTagsAPI{Svc: &costtags.Service{Deps: deps, Repo: postgres.NewCostTagsRepo()},
		Pages: httpapi.NewPageTokens(c.PageKey, nil)}))
}

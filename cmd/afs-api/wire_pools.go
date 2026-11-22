package main

import (
	crand "crypto/rand"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pools"
)

// wirePools builds the swarm use cases (stages, pools and what hangs off them, M5-B) and binds their
// handlers as the pools area.
func wirePools(c *components, log *slog.Logger) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	httpapi.BindPools(&httpapi.PoolsAPI{
		Svc:   &pools.Service{Deps: deps, Repo: postgres.NewPoolRepo(), Entropy: crand.Reader},
		Pages: httpapi.NewPageTokens(pageTokenKey(log), nil),
	})
}

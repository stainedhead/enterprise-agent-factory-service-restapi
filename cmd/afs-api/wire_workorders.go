package main

import (
	crand "crypto/rand"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/swarmtemplates"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/workorders"
)

// wireWorkOrders builds the swarm template (M6-B) and work order (M6-C) use cases and binds their handlers.
func wireWorkOrders(c *components, log *slog.Logger) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	pages := httpapi.NewPageTokens(pageTokenKey(log), nil)
	httpapi.BindSwarmTemplates(&httpapi.SwarmTemplatesAPI{Svc: &swarmtemplates.Service{Deps: deps, Repo: postgres.NewSwarmTemplateRepo()}, Pages: pages})
	httpapi.BindWorkOrders(&httpapi.WorkOrdersAPI{Svc: &workorders.Service{Deps: deps, Repo: postgres.NewWorkOrderRepo(), Settings: c.Settings}, Pages: pages})
}

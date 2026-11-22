package main

import (
	crand "crypto/rand"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/budgets"
)

// wireBudgets builds the budget use cases (M3-E) on the database components and binds their handlers.
func wireBudgets(c *components) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	httpapi.BindBudgets(&httpapi.BudgetsAPI{Svc: &budgets.Service{Deps: deps, Repo: postgres.NewBudgetRepo()}})
}

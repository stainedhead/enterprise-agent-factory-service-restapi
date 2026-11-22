package main

import (
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/iam"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/budget"
)

// wireBudget registers the budget jobs (period opening, limit carry, expired reservations, override
// expiry and the Bedrock deny backstop) as leader-only jobs (M3-F).
func wireBudget(e budget.Adder, runner *postgres.TxRunner, audit port.AuditWriter, outbox port.OutboxWriter, ids port.IDGen, role budget.RoleDeny, m *observability.Metrics, now func() time.Time, log *slog.Logger) {
	r := &budget.Reconciler{
		Store: postgres.NewBudgetJobs(runner, audit, outbox, ids),
		IAM:   role,
		Clock: now,
		Log:   log,
	}
	if m != nil {
		r.Released = func(n int64) { m.GatewayReservationsExpired.Add(float64(n)) }
	}
	r.Register(e)
}

var _ budget.RoleDeny = (*iam.Client)(nil)

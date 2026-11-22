package main

import (
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/teardown"
)

// wireTeardown adds the ephemeral team teardown job (M5-D1) to the engine's leader-only jobs: due teams move
// to `deleting`, where the team controller's finalizer (RC-17) takes over, and a finished teardown gets its
// closing record.
func wireTeardown(s teardown.Scheduler, runner port.TxRunner, audit port.AuditWriter, outbox port.OutboxWriter, ids port.IDGen, now func() time.Time, log *slog.Logger) {
	teardown.Register(s, &teardown.Job{Store: postgres.NewTeardownStore(runner, audit, outbox, ids), Now: now, Log: log})
}

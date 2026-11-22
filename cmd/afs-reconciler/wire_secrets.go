package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/secretsmanager"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/housekeeping"
)

// smLister adapts the Secrets Manager client to housekeeping.SecretLister.
type smLister struct{ c *secretsmanager.Client }

func (l smLister) ListUnderPrefix(ctx context.Context, prefix string) ([]housekeeping.ManagedSecret, error) {
	ms, err := l.c.ListUnderPrefix(ctx, prefix)
	out := make([]housekeeping.ManagedSecret, 0, len(ms))
	for _, m := range ms {
		out = append(out, housekeeping.ManagedSecret{ARN: m.ARN, Name: m.Name, Created: m.Created, Tagged: m.Orphan})
	}
	return out, err
}

// registerOrphanSweep adds the orphan secret sweep (M2-C5) to the engine's leader-only jobs.
func registerOrphanSweep(s housekeeping.Scheduler, sm *secretsmanager.Client, runner port.TxRunner, audit port.AuditWriter, now func() time.Time, log *slog.Logger) {
	job := &housekeeping.OrphanSecrets{Lister: smLister{sm}, Scheduler: sm, Rows: postgres.NewOrphanSecretRows(runner, audit), Clock: now, Log: log}
	housekeeping.Register(s, []housekeeping.Job{{Name: "orphan-secrets", Interval: housekeeping.OrphanInterval, Run: job.Run}})
}

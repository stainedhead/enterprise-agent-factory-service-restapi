package main

import (
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/export"
)

// exportBlob returns the object store that holds export bundles: an S3 bucket (AFS_EXPORT_BUCKET) with
// server-side KMS encryption (AFS_EXPORT_KMS_KEY_ID). The S3 adapter is owned by M4-E4; until it is merged
// this returns nil and the export job stays unregistered, so a pending export is not claimed by a
// reconciler that cannot write it.
func exportBlob() export.Blob { return nil }

// wireExport registers the export bundle builder (M4-F3) as a leader-only job. It does nothing without a
// blob store.
func wireExport(e export.Scheduler, runner *postgres.TxRunner, audit port.AuditWriter, outbox port.OutboxWriter, ids port.IDGen, blob export.Blob, now func() time.Time, log *slog.Logger) bool {
	if blob == nil {
		log.Info("export bundles disabled: no export bucket configured")
		return false
	}
	b := &export.Builder{Store: postgres.NewExportJobs(runner, audit, outbox, ids), Blob: blob, Clock: now, Log: log}
	b.Register(e)
	return true
}

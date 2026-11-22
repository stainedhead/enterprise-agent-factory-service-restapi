package main

import (
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/rollup"
)

// wireRollup registers the report rollup leader jobs: afs.rollup_reports_recent(2) every 10 minutes and
// afs.rollup_reports_recent(7) nightly (-db 000018), with run and last-success metrics.
func wireRollup(e rollup.Scheduler, hk *postgres.Housekeeping, m *observability.Metrics, log *slog.Logger) {
	r := &rollup.Runner{Store: hk, Log: log}
	if m != nil {
		r.Recorder = m
	}
	r.Register(e)
}

package main

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// registerEngineStats exposes the engine's counters and queue on the registry observability serves
// (/metrics on AFS_METRICS_ADDR). Probes (/healthz, /readyz) are observability's too.
func registerEngineStats(m *observability.Metrics, stats func() engine.Stats) {
	gauge := func(name, help string, f func(engine.Stats) float64) {
		m.Registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, func() float64 { return f(stats()) }))
	}
	counter := func(result string, f func(engine.Stats) float64) {
		m.Registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "afs_reconciler_reconciles_total", Help: "Reconcile passes by result.", ConstLabels: prometheus.Labels{"result": result},
		}, func() float64 { return f(stats()) }))
	}
	counter("success", func(s engine.Stats) float64 { return float64(s.Reconciled) })
	counter("failure", func(s engine.Stats) float64 { return float64(s.Failed) })
	gauge("afs_reconciler_queue_depth", "Resources waiting to be reconciled.", func(s engine.Stats) float64 { return float64(s.QueueDepth) })
	gauge("afs_reconciler_oldest_overdue_seconds", "How long the longest-waiting due resource has waited.", func(s engine.Stats) float64 { return s.OldestOverdue.Seconds() })
	gauge("afs_reconciler_last_poll_timestamp_seconds", "Unix time of the last generation poll.", func(s engine.Stats) float64 { return unixOrZero(s) })
	gauge("afs_reconciler_last_resync_timestamp_seconds", "Unix time of the last full resync.", func(s engine.Stats) float64 {
		if s.LastResync.IsZero() {
			return 0
		}
		return float64(s.LastResync.Unix())
	})
}

func unixOrZero(s engine.Stats) float64 {
	if s.LastPoll.IsZero() {
		return 0
	}
	return float64(s.LastPoll.Unix())
}

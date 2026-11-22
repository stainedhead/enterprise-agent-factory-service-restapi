package main

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// gatewayCounters are the two counters the gateway writes for FR-R05 and FR-R08 (RD-3).
type gatewayCounters struct {
	// SettleFailures is afs_gateway_settle_failures_total.
	SettleFailures prometheus.Counter
	// UsageRowsDropped is afs_gateway_usage_rows_dropped_total{reason}.
	UsageRowsDropped *prometheus.CounterVec
}

// newGatewayCounters returns the counters of m.
//
// INTEGRATOR (after stream C merges): C-7 registers both counters in observability.NewMetrics. Replace
// the body with `return gatewayCounters{m.GatewaySettleFailures, m.GatewayUsageRowsDropped}` (use C's field
// names) and drop the unused parameter name. Until then the counters are built here and are not in the
// registry, so they are written but not scraped; registering them here as well would panic as a duplicate
// once C lands.
func newGatewayCounters(_ *observability.Metrics) gatewayCounters {
	return gatewayCounters{
		SettleFailures: prometheus.NewCounter(prometheus.CounterOpts{Name: "afs_gateway_settle_failures_total",
			Help: "Settlements that failed inline and fell back to the retry queue or were dropped."}),
		UsageRowsDropped: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "afs_gateway_usage_rows_dropped_total",
			Help: "Usage rows the gateway dropped, by reason."}, []string{"reason"}),
	}
}

// settleRecorder binds gateway.SettleRecorder to the counter.
type settleRecorder struct{ c prometheus.Counter }

// SettleFailed implements gateway.SettleRecorder.
func (r settleRecorder) SettleFailed(_, _ string, _ domain.Money, _ gateway.SettleReason) { r.c.Inc() }

// usageDropRecorder binds gateway.UsageDropRecorder to the counter.
type usageDropRecorder struct{ c *prometheus.CounterVec }

// UsageRowsDropped implements gateway.UsageDropRecorder.
func (r usageDropRecorder) UsageRowsDropped(reason gateway.UsageDropReason, rows int) {
	r.c.WithLabelValues(string(reason)).Add(float64(rows))
}

var (
	_ gateway.SettleRecorder    = settleRecorder{}
	_ gateway.UsageDropRecorder = usageDropRecorder{}
)

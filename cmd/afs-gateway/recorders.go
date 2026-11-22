package main

import (
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// settleRecorder binds gateway.SettleRecorder to afs_gateway_settle_failures_total (FR-R05).
type settleRecorder struct{ m *observability.Metrics }

// SettleFailed implements gateway.SettleRecorder.
func (r settleRecorder) SettleFailed(_, _ string, _ domain.Money, _ gateway.SettleReason) {
	r.m.SettleFailed()
}

// usageDropRecorder binds gateway.UsageDropRecorder to afs_gateway_usage_rows_dropped_total{reason}
// (FR-R08).
type usageDropRecorder struct{ m *observability.Metrics }

// UsageRowsDropped implements gateway.UsageDropRecorder.
func (r usageDropRecorder) UsageRowsDropped(reason gateway.UsageDropReason, rows int) {
	for range rows {
		r.m.UsageRowDropped(string(reason))
	}
}

var (
	_ gateway.SettleRecorder    = settleRecorder{}
	_ gateway.UsageDropRecorder = usageDropRecorder{}
)

package main

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// TestRecordersBindToTheGatewayCounters covers FR-R05 AC3 and FR-R08 AC1: a settlement that falls back
// increments afs_gateway_settle_failures_total, a dropped usage row afs_gateway_usage_rows_dropped_total
// with its reason.
func TestRecordersBindToTheGatewayCounters(t *testing.T) {
	c := newGatewayCounters(observability.NewMetrics("afs-gateway", "test"))
	sr := settleRecorder{c.SettleFailures}
	for _, r := range []gateway.SettleReason{gateway.SettleQueued, gateway.SettleQueueFull, gateway.SettleWindowExpired} {
		sr.SettleFailed("team_x", "bud_x", domain.MustMoney("0.01"), r)
	}
	if got := testutil.ToFloat64(c.SettleFailures); got != 3 {
		t.Fatalf("settle failures = %v, want 3", got)
	}
	ur := usageDropRecorder{c.UsageRowsDropped}
	ur.UsageRowsDropped(gateway.UsageQueueFull, 4)
	ur.UsageRowsDropped(gateway.UsageWriteFailed, 2)
	ur.UsageRowsDropped(gateway.UsageWriteFailed, 1)
	if got := testutil.ToFloat64(c.UsageRowsDropped.WithLabelValues("queue_full")); got != 4 {
		t.Errorf("queue_full = %v, want 4", got)
	}
	if got := testutil.ToFloat64(c.UsageRowsDropped.WithLabelValues("write_failed")); got != 3 {
		t.Errorf("write_failed = %v, want 3", got)
	}
}

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

// modelDenialRecorder binds gateway.ModelDenialRecorder to afs_gateway_model_denials_total{reason}.
type modelDenialRecorder struct{ m *observability.Metrics }

// ModelDenied implements gateway.ModelDenialRecorder.
func (r modelDenialRecorder) ModelDenied(_, _, reason string) { r.m.ModelDenied(reason) }

var (
	_ gateway.ModelDenialRecorder = modelDenialRecorder{}
	_ gateway.SettleRecorder      = settleRecorder{}
	_ gateway.UsageDropRecorder   = usageDropRecorder{}
)

// genAIOperation is the GenAI operation name of a route protocol.
var genAIOperation = map[domain.Protocol]string{domain.ProtocolChat: "chat", domain.ProtocolResponses: "chat", domain.ProtocolEmbeddings: "embeddings"}

// telemetrySink observes every route call's usage row as GenAI metrics (gateway contract §12.7) and passes
// the row on. Bedrock rows (no Route) only pass through.
type telemetrySink struct {
	next gateway.UsageSink
	m    *observability.Metrics
}

// Record implements gateway.UsageSink.
func (t telemetrySink) Record(r gateway.Record) {
	t.next.Record(r)
	rf := r.Route
	if rf == nil || r.Outcome != gateway.OutcomeOK && r.Outcome != gateway.OutcomeError {
		return
	}
	op := genAIOperation[rf.Protocol]
	if op == "" {
		op = "chat"
	}
	lv := []string{op, rf.Provider, rf.Model, r.TeamID, rf.Slug, rf.Provider, rf.Source}
	t.m.GenAIOperationDuration.WithLabelValues(lv...).Observe(float64(r.LatencyMS) / 1000)
	if rf.TTFTMS > 0 {
		t.m.GenAITimeToFirstToken.WithLabelValues(lv...).Observe(float64(rf.TTFTMS) / 1000)
	}
	t.m.GenAITokenUsage.WithLabelValues(append(lv, "input")...).Observe(float64(r.Usage.Input))
	if op != "embeddings" {
		t.m.GenAITokenUsage.WithLabelValues(append(lv, "output")...).Observe(float64(r.Usage.Output))
	}
}

var _ gateway.UsageSink = telemetrySink{}

package main

import (
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/condmsg"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// reconcileMetrics binds the recorder ports of the reconcile engine and the notification drainers
// (engine.Recorder, notify.FailureRecorder) to the Prometheus collectors, so the series the alerts and
// the dashboard read are written (FR-R06, RD-4).
type reconcileMetrics struct{ m *observability.Metrics }

// newReconcileMetrics returns the binding and installs the observability scrubber into the condition
// sanitiser, which usecase packages cannot import (FR-R13).
func newReconcileMetrics(m *observability.Metrics) reconcileMetrics {
	condmsg.SetScrubber(observability.Scrub)
	return reconcileMetrics{m: m}
}

// ReconcileDone implements engine.Recorder.
func (r reconcileMetrics) ReconcileDone(kind engine.Kind, d time.Duration, err error) {
	r.m.ReconcileDuration.WithLabelValues(string(kind)).Observe(d.Seconds())
	if err != nil {
		r.m.ReconcileErrors.WithLabelValues(string(kind)).Inc()
	}
}

// ReconcileLag implements engine.Recorder.
func (r reconcileMetrics) ReconcileLag(kind engine.Kind, d time.Duration) {
	r.m.ReconcileLag.WithLabelValues(string(kind)).Observe(d.Seconds())
}

// DriftCorrected implements engine.Recorder.
func (r reconcileMetrics) DriftCorrected(kind engine.Kind) {
	r.m.DriftCorrected.WithLabelValues(string(kind)).Inc()
}

// NotificationFailed implements notify.FailureRecorder.
func (r reconcileMetrics) NotificationFailed(channelType string) {
	r.m.NotificationFailures.WithLabelValues(channelType).Inc()
}

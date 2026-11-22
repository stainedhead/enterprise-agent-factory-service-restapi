package observability

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is a binary's Prometheus registry and the AFS collectors. Every binary builds one with
// NewMetrics; a stream records into the fields it owns. Using one struct keeps the names in
// names.go the only place a metric is declared.
type Metrics struct {
	Registry *prometheus.Registry

	APIRequests          *prometheus.CounterVec
	APIRequestDuration   *prometheus.HistogramVec
	APIInFlight          prometheus.Gauge
	APICrossTeamNotFound prometheus.Counter
	// APIOrphanSecrets counts secrets left in Secrets Manager when the compensating delete failed.
	APIOrphanSecrets prometheus.Counter

	ReconcileDuration *prometheus.HistogramVec
	ReconcileErrors   *prometheus.CounterVec
	ReconcileLag      *prometheus.HistogramVec
	DriftCorrected    *prometheus.CounterVec
	Leader            prometheus.Gauge

	GatewayReservationFailures *prometheus.CounterVec
	GatewayAddedLatency        prometheus.Histogram
	GatewayReservationsExpired prometheus.Counter
	// GatewaySettleFailures counts settlements that fell back to the retry queue or were dropped.
	GatewaySettleFailures prometheus.Counter
	// GatewayUsageRowsDropped counts lost usage rows by reason (queue_full, write_failed).
	GatewayUsageRowsDropped *prometheus.CounterVec

	RollupRuns        *prometheus.CounterVec
	RollupLastSuccess *prometheus.GaugeVec

	OutboxPending        prometheus.Gauge
	OutboxOldestAge      prometheus.Gauge
	OutboxPublishedTotal prometheus.Counter
	OutboxFailedTotal    prometheus.Counter

	NotificationFailures *prometheus.CounterVec
	CURVariance          prometheus.Gauge

	EdgeRequests      *prometheus.CounterVec
	EdgeRequestSecs   *prometheus.HistogramVec
	EdgeRefreshFailed prometheus.Counter
	EdgeAccessDropped prometheus.Counter
}

// NewMetrics registers every AFS collector plus the Go runtime and process collectors, and the
// build info of service at version.
func NewMetrics(service, version string) *Metrics {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	f := func(name, help string) prometheus.Opts { return prometheus.Opts{Name: name, Help: help} }
	m := &Metrics{Registry: r}
	m.APIRequests = prometheus.NewCounterVec(prometheus.CounterOpts(f(MetricAPIRequests, "API requests by route, method and status code.")), []string{"route", "method", "code"})
	m.APIRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: MetricAPIRequestDuration, Help: "API request duration.", Buckets: APIBuckets}, []string{"route", "method"})
	m.APIInFlight = prometheus.NewGauge(prometheus.GaugeOpts(f(MetricAPIInFlight, "API requests being served.")))
	m.APICrossTeamNotFound = prometheus.NewCounter(prometheus.CounterOpts(f(MetricAPICrossTeamNotFound, "404s that hid a resource of another team.")))
	m.APIOrphanSecrets = prometheus.NewCounter(prometheus.CounterOpts(f(MetricAPIOrphanSecrets, "Secrets Manager secrets created for a request whose database write failed and that could not be deleted again.")))
	m.ReconcileDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: MetricReconcileDuration, Help: "Duration of one reconcile.", Buckets: ReconcileBucket}, []string{"kind"})
	m.ReconcileErrors = prometheus.NewCounterVec(prometheus.CounterOpts(f(MetricReconcileErrors, "Failed reconciles.")), []string{"kind"})
	m.ReconcileLag = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: MetricReconcileLag, Help: "Time from a desired-state change to observedGeneration = generation.", Buckets: ReconcileBucket}, []string{"kind"})
	m.DriftCorrected = prometheus.NewCounterVec(prometheus.CounterOpts(f(MetricDriftCorrected, "Drift corrections.")), []string{"kind"})
	m.Leader = prometheus.NewGauge(prometheus.GaugeOpts(f(MetricLeader, "1 while this replica holds the reconciler lease.")))
	m.GatewayReservationFailures = prometheus.NewCounterVec(prometheus.CounterOpts(f(MetricGatewayReservationFailures, "Calls the gateway rejected because it could not reserve spend.")), []string{"reason"})
	m.GatewayAddedLatency = prometheus.NewHistogram(prometheus.HistogramOpts{Name: MetricGatewayAddedLatency, Help: "Latency the gateway adds, excluding Bedrock time.", Buckets: GatewayBuckets})
	m.GatewayReservationsExpired = prometheus.NewCounter(prometheus.CounterOpts(f(MetricGatewayReservationsExpired, "Spend reservations that expired unsettled.")))
	m.GatewaySettleFailures = prometheus.NewCounter(prometheus.CounterOpts(f(MetricGatewaySettleFailures, "Settlements that fell back to the retry queue or were dropped.")))
	m.GatewayUsageRowsDropped = prometheus.NewCounterVec(prometheus.CounterOpts(f(MetricGatewayUsageRowsDropped, "Usage rows the gateway could not write, by reason.")), []string{"reason"})
	m.RollupRuns = prometheus.NewCounterVec(prometheus.CounterOpts(f(MetricReportRollupRuns, "Report rollup runs by job and outcome.")), []string{"job", "outcome"})
	m.RollupLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts(f(MetricReportRollupLastSuccess, "Unix time of the last successful report rollup run, by job.")), []string{"job"})
	m.OutboxPending = prometheus.NewGauge(prometheus.GaugeOpts(f(MetricOutboxPending, "Unpublished outbox events.")))
	m.OutboxOldestAge = prometheus.NewGauge(prometheus.GaugeOpts(f(MetricOutboxOldestAge, "Age of the oldest unpublished outbox event.")))
	m.OutboxPublishedTotal = prometheus.NewCounter(prometheus.CounterOpts(f(MetricOutboxPublished, "Outbox events published.")))
	m.OutboxFailedTotal = prometheus.NewCounter(prometheus.CounterOpts(f(MetricOutboxFailed, "Failed outbox publish attempts.")))
	m.NotificationFailures = prometheus.NewCounterVec(prometheus.CounterOpts(f(MetricNotificationFailures, "Notifications that could not be delivered.")), []string{"channel_type"})
	m.CURVariance = prometheus.NewGauge(prometheus.GaugeOpts(f(MetricCURVariance, "Relative variance between gateway estimates and the CUR for the latest day.")))
	m.EdgeRequests = prometheus.NewCounterVec(prometheus.CounterOpts(f(MetricEdgeRequests, "Collaboration edge requests by surface and status code.")), []string{"surface", "code"})
	m.EdgeRequestSecs = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: MetricEdgeRequestSeconds, Help: "Collaboration edge request duration, including the upstream.", Buckets: APIBuckets}, []string{"surface"})
	m.EdgeRefreshFailed = prometheus.NewCounter(prometheus.CounterOpts(f(MetricEdgeRefreshFailed, "Route table refreshes that failed.")))
	m.EdgeAccessDropped = prometheus.NewCounter(prometheus.CounterOpts(f(MetricEdgeAccessDropped, "Access counters dropped because the buffer was full.")))
	r.MustRegister(m.EdgeRequests, m.EdgeRequestSecs, m.EdgeRefreshFailed, m.EdgeAccessDropped, m.APIRequests, m.APIRequestDuration, m.APIInFlight, m.APICrossTeamNotFound, m.APIOrphanSecrets,
		m.ReconcileDuration, m.ReconcileErrors, m.ReconcileLag, m.DriftCorrected, m.Leader,
		m.GatewayReservationFailures, m.GatewayAddedLatency, m.GatewayReservationsExpired,
		m.GatewaySettleFailures, m.GatewayUsageRowsDropped,
		m.OutboxPending, m.OutboxOldestAge, m.OutboxPublishedTotal, m.OutboxFailedTotal,
		m.NotificationFailures, m.CURVariance, m.RollupRuns, m.RollupLastSuccess)
	bi := prometheus.NewGaugeVec(prometheus.GaugeOpts(f(MetricBuildInfo, "Build information.")), []string{"service", "version"})
	r.MustRegister(bi)
	bi.WithLabelValues(service, version).Set(1)
	return m
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{Registry: m.Registry})
}

// RollupRun records one report rollup run (reconcile/rollup.Recorder).
func (m *Metrics) RollupRun(job string, _ int64, err error) {
	if err != nil {
		m.RollupRuns.WithLabelValues(job, "error").Inc()
		return
	}
	m.RollupRuns.WithLabelValues(job, "ok").Inc()
	m.RollupLastSuccess.WithLabelValues(job).SetToCurrentTime()
}

// OutboxPublished implements housekeeping.Recorder.
func (m *Metrics) OutboxPublished(n int) { m.OutboxPublishedTotal.Add(float64(n)) }

// OutboxFailed implements housekeeping.Recorder.
func (m *Metrics) OutboxFailed(n int) { m.OutboxFailedTotal.Add(float64(n)) }

// OutboxLag implements housekeeping.Recorder.
func (m *Metrics) OutboxLag(pending int, oldest time.Duration) {
	m.OutboxPending.Set(float64(pending))
	m.OutboxOldestAge.Set(oldest.Seconds())
}

// SettleFailed counts one settlement that fell back to the retry queue or was dropped (gateway
// SettleRecorder, bound in cmd/afs-gateway).
func (m *Metrics) SettleFailed() { m.GatewaySettleFailures.Inc() }

// UsageRowDropped counts one lost usage row; reason is "queue_full" or "write_failed".
func (m *Metrics) UsageRowDropped(reason string) {
	m.GatewayUsageRowsDropped.WithLabelValues(reason).Inc()
}

// NotificationFailed counts one failed notification send on a channel type (notify.FailureRecorder).
func (m *Metrics) NotificationFailed(channelType string) {
	m.NotificationFailures.WithLabelValues(channelType).Inc()
}

// SetLeader records whether this replica is the leader.
func (m *Metrics) SetLeader(is bool) {
	if is {
		m.Leader.Set(1)
		return
	}
	m.Leader.Set(0)
}

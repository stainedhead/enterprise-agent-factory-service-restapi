package observability

// Metric names. The dashboards and alerts in deploy/helm/afs/templates (prometheusrule.yaml,
// dashboards.yaml) are written against exactly these names; a test keeps the two in step. Labels are
// part of the contract: route is the operationId (or a bounded path template), never a raw path.
const (
	// RED per route, afs-api.
	MetricAPIRequests          = "afs_api_requests_total"           // route, method, code
	MetricAPIRequestDuration   = "afs_api_request_duration_seconds" // route, method (histogram)
	MetricAPIInFlight          = "afs_api_requests_in_flight"
	MetricAPICrossTeamNotFound = "afs_api_cross_team_not_found_total"

	// Reconciler.
	MetricReconcileDuration = "afs_reconcile_duration_seconds" // kind (histogram)
	MetricReconcileErrors   = "afs_reconcile_errors_total"     // kind
	MetricReconcileLag      = "afs_reconcile_lag_seconds"      // kind: change to observed generation (histogram)
	MetricDriftCorrected    = "afs_drift_corrected_total"      // kind
	MetricLeader            = "afs_reconciler_is_leader"

	// Gateway.
	MetricGatewayReservationFailures = "afs_gateway_reservation_failures_total" // reason
	MetricGatewayAddedLatency        = "afs_gateway_added_latency_seconds"      // histogram, excludes Bedrock time
	MetricGatewayReservationsExpired = "afs_gateway_reservations_expired_total"

	// Outbox.
	MetricOutboxPending   = "afs_outbox_pending_events"
	MetricOutboxOldestAge = "afs_outbox_oldest_age_seconds"
	MetricOutboxPublished = "afs_outbox_published_total"
	MetricOutboxFailed    = "afs_outbox_publish_failures_total"

	// Notifications and billing.
	MetricNotificationFailures = "afs_notification_failures_total" // channel_type
	MetricCURVariance          = "afs_cost_cur_variance_ratio"     // abs(estimate - billed) / billed, latest day

	// Build info, every binary.
	MetricBuildInfo = "afs_build_info" // service, version
)

// Buckets of the latency histograms, in seconds. The API p99 target is 0.3 s (1 s for reports) and
// the gateway's added latency target is 0.025 s, so each histogram has bucket edges on its target.
var (
	APIBuckets      = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2.5, 5, 10}
	GatewayBuckets  = []float64{0.001, 0.0025, 0.005, 0.01, 0.015, 0.025, 0.05, 0.1, 0.25, 0.5, 1}
	ReconcileBucket = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300}
)

// AllMetricNames lists every metric AFS declares (not the Go and process collectors).
func AllMetricNames() []string {
	return []string{
		MetricAPIRequests, MetricAPIRequestDuration, MetricAPIInFlight, MetricAPICrossTeamNotFound,
		MetricReconcileDuration, MetricReconcileErrors, MetricReconcileLag, MetricDriftCorrected, MetricLeader,
		MetricGatewayReservationFailures, MetricGatewayAddedLatency, MetricGatewayReservationsExpired,
		MetricOutboxPending, MetricOutboxOldestAge, MetricOutboxPublished, MetricOutboxFailed,
		MetricNotificationFailures, MetricCURVariance, MetricBuildInfo,
	}
}

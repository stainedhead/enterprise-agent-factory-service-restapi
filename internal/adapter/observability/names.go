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
	// MetricAPIOrphanSecrets counts Secrets Manager secrets left behind when the compensating delete failed (M2-C2).
	MetricAPIOrphanSecrets = "afs_api_orphan_secrets_total" //nolint:gosec // a metric name, not a credential

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
	// MetricGatewaySettleFailures counts settlements that fell back to the retry queue or were dropped (FR-R05, RD-3).
	MetricGatewaySettleFailures = "afs_gateway_settle_failures_total"
	// MetricGatewayModelDenials counts calls refused by model admission, by reason (model_not_enabled_for_service,
	// _team or _agent, model_not_priced).
	MetricGatewayModelDenials = "afs_gateway_model_denials_total"
	// MetricGatewayUsageRowsDropped counts usage rows the writer lost (FR-R08, RD-3); reason is queue_full or write_failed.
	MetricGatewayUsageRowsDropped = "afs_gateway_usage_rows_dropped_total"
	// GenAI semantic-convention metrics of route calls (gateway contract §12.7). Labels never carry an agent
	// or pool id: gen_ai_operation_name, gen_ai_provider_name, gen_ai_request_model, afs_team_id, afs_route,
	// afs_provider, afs_usage_source (and gen_ai_token_type on token usage).
	MetricGenAITokenUsage        = "gen_ai_client_token_usage" //nolint:gosec // a metric name
	MetricGenAIOperationDuration = "gen_ai_client_operation_duration_seconds"
	MetricGenAITimeToFirstToken  = "gen_ai_server_time_to_first_token_seconds" //nolint:gosec // a metric name

	// Outbox.
	MetricOutboxPending   = "afs_outbox_pending_events"
	MetricOutboxOldestAge = "afs_outbox_oldest_age_seconds"
	MetricOutboxPublished = "afs_outbox_published_total"
	MetricOutboxFailed    = "afs_outbox_publish_failures_total"

	// Notifications and billing.
	MetricNotificationFailures = "afs_notification_failures_total" // channel_type
	MetricCURVariance          = "afs_cost_cur_variance_ratio"     // abs(estimate - billed) / billed, latest day

	// Report rollups (reconciler).
	MetricReportRollupRuns        = "afs_report_rollup_runs_total"                     // job, outcome
	MetricReportRollupLastSuccess = "afs_report_rollup_last_success_timestamp_seconds" // job

	// Collaboration edge (afs-edge).
	MetricEdgeRequests       = "afs_edge_requests_total"           // surface (web_ui, rest_api), code
	MetricEdgeRequestSeconds = "afs_edge_request_duration_seconds" // surface (histogram)
	MetricEdgeRefreshFailed  = "afs_edge_route_refresh_failures_total"
	MetricEdgeAccessDropped  = "afs_edge_access_counts_dropped_total"

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
		MetricAPIRequests, MetricAPIRequestDuration, MetricAPIInFlight, MetricAPICrossTeamNotFound, MetricAPIOrphanSecrets,
		MetricEdgeRequests, MetricEdgeRequestSeconds, MetricEdgeRefreshFailed, MetricEdgeAccessDropped,
		MetricReconcileDuration, MetricReconcileErrors, MetricReconcileLag, MetricDriftCorrected, MetricLeader,
		MetricGatewayReservationFailures, MetricGatewayAddedLatency, MetricGatewayReservationsExpired,
		MetricGatewaySettleFailures, MetricGatewayUsageRowsDropped, MetricGatewayModelDenials,
		MetricOutboxPending, MetricOutboxOldestAge, MetricOutboxPublished, MetricOutboxFailed,
		MetricNotificationFailures, MetricCURVariance, MetricReportRollupRuns, MetricReportRollupLastSuccess, MetricBuildInfo,
	}
}

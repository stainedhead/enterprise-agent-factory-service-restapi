package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel"
)

const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c3JfMDEifQ.c2lnbmF0dXJlLWJ5dGVz"

func logLine(t *testing.T, f func(l *slog.Logger, ctx context.Context)) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	l := NewLogger(&buf, LogConfig{Level: "debug", Service: "afs-api", RequestID: func(ctx context.Context) string {
		s, _ := ctx.Value(rid{}).(string)
		return s
	}})
	f(l, context.WithValue(context.Background(), rid{}, "req_123"))
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("not one JSON line: %q: %v", buf.String(), err)
	}
	return m
}

type rid struct{}

func TestLoggerJSONWithRequestID(t *testing.T) {
	m := logLine(t, func(l *slog.Logger, ctx context.Context) { l.InfoContext(ctx, "hello", "team", "team_1") })
	if m["msg"] != "hello" || m["requestId"] != "req_123" || m["service"] != "afs-api" || m["team"] != "team_1" || m["level"] != "INFO" {
		t.Fatalf("record %v", m)
	}
}

func TestRedaction(t *testing.T) {
	tests := []struct {
		name string
		log  func(l *slog.Logger, ctx context.Context)
		leak string
	}{
		{"authorization attribute", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "x", "Authorization", "Bearer abc.def.ghi-secret")
		}, "ghi-secret"},
		{"gateway token attribute", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "x", "X-Afs-Gateway-Token", "tok-123456789")
		}, "tok-123456789"},
		{"x_afs_gateway_token spelling", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "x", "x_afs_gateway_token", "tok-987654321")
		}, "tok-987654321"},
		{"bearer in message", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "got Bearer s3cr3t-token-value from client")
		}, "s3cr3t-token-value"},
		{"afsk key in string", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "x", "detail", "key afsk_AbCdEf0123456789 used")
		}, "AbCdEf0123456789"},
		{"jwt in string", func(l *slog.Logger, ctx context.Context) { l.InfoContext(ctx, "x", "claim", "token was "+jwt) }, "c2lnbmF0dXJl"},
		{"jwt in error", func(l *slog.Logger, ctx context.Context) {
			l.ErrorContext(ctx, "x", "err", fmt.Errorf("verify %s: bad", jwt))
		}, "c2lnbmF0dXJl"},
		{"nested group", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "x", slog.Group("req", slog.String("password", "hunter2hunter2"), slog.String("path", "/v1/teams")))
		}, "hunter2hunter2"},
		{"header map", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "x", "headers", http.Header{"Authorization": {"Bearer zzzzzzzzzzzz"}, "Cookie": {"sid=abcdefgh"}, "X-Request-Id": {"r1"}})
		}, "zzzzzzzzzzzz"},
		{"map any", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "x", "body", map[string]any{"secretKey": "afsk_supersecretvalue1", "name": "ok"})
		}, "supersecretvalue1"},
		{"struct with a key", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "x", "cfg", struct{ K string }{"afsk_structsecret12345"})
		}, "structsecret12345"},
		{"with attrs", func(l *slog.Logger, ctx context.Context) { l.With("token", "aaaaaaaaaaaa").InfoContext(ctx, "x") }, "aaaaaaaaaaaa"},
		{"with group", func(l *slog.Logger, ctx context.Context) {
			l.WithGroup("g").InfoContext(ctx, "x", "apiKey", "bbbbbbbbbbbb")
		}, "bbbbbbbbbbbb"},
		{"aws access key id", func(l *slog.Logger, ctx context.Context) { l.InfoContext(ctx, "x", "v", "AKIAIOSFODNN7EXAMPLE") }, "AKIAIOSFODNN7EXAMPLE"},
		{"private key", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(ctx, "x", "v", "-----BEGIN RSA PRIVATE KEY-----\nMIIEvQIBADANBg\n-----END RSA PRIVATE KEY-----")
		}, "MIIEvQIBADANBg"},
		{"request id spoof", func(l *slog.Logger, ctx context.Context) {
			l.InfoContext(context.WithValue(ctx, rid{}, "Bearer leakedrequestid123"), "x")
		}, "leakedrequestid123"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := NewLogger(&buf, LogConfig{RequestID: func(ctx context.Context) string { s, _ := ctx.Value(rid{}).(string); return s }})
			tt.log(l, context.Background())
			if strings.Contains(buf.String(), tt.leak) {
				t.Fatalf("leaked %q in %s", tt.leak, buf.String())
			}
			if !strings.Contains(buf.String(), "redacted") {
				t.Fatalf("nothing marked redacted: %s", buf.String())
			}
		})
	}
}

func TestRedactionKeepsOrdinaryValues(t *testing.T) {
	m := logLine(t, func(l *slog.Logger, ctx context.Context) {
		l.InfoContext(ctx, "reconciled", "tokens", 1200, "tokenCount", "12", "name", "agent-1", "err", errors.New("connection refused"), "d", 3*time.Second)
	})
	if m["tokens"] != float64(1200) || m["tokenCount"] != "12" || m["name"] != "agent-1" || m["err"] != "connection refused" {
		t.Fatalf("ordinary values changed: %v", m)
	}
}

func TestScrubPatterns(t *testing.T) {
	for _, s := range []string{"Bearer abcdef123456", "afsk_abcd1234", jwt, "AKIAIOSFODNN7EXAMPLE", "x-afs-gateway-token: abcdefgh"} {
		if got := Scrub(s); strings.Contains(got, "abcdef123456") || strings.Contains(got, "abcd1234") || strings.Contains(got, "c2lnbmF0dXJl") || strings.Contains(got, "IOSFODNN7") || strings.Contains(got, "abcdefgh") {
			t.Errorf("Scrub(%q) = %q", s, got)
		}
	}
	if got := Scrub("plain text, team_01 and 12345"); got != "plain text, team_01 and 12345" {
		t.Errorf("changed plain text: %q", got)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "WARN": slog.LevelWarn, "warning": slog.LevelWarn, "error": slog.LevelError, "": slog.LevelInfo, "nope": slog.LevelInfo} {
		if got := ParseLevel(in); got != want {
			t.Errorf("ParseLevel(%q) = %v, want %v", in, got, want)
		}
	}
	var buf bytes.Buffer
	NewLogger(&buf, LogConfig{Level: "error"}).Info("hidden")
	if buf.Len() != 0 {
		t.Error("info logged at level error")
	}
}

func TestMetricsEndpointAndRecorder(t *testing.T) {
	m := NewMetrics("afs-reconciler", "v1.2.3")
	m.OutboxLag(7, 90*time.Second)
	m.OutboxPublished(3)
	m.OutboxFailed(1)
	m.SetLeader(true)
	m.RollupRun("report-rollup-recent", 3, nil)
	m.RollupRun("report-rollup-recent", 0, errors.New("x"))
	if testutil.ToFloat64(m.OutboxPending) != 7 || testutil.ToFloat64(m.OutboxOldestAge) != 90 {
		t.Error("outbox lag not recorded")
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil))
	body := rec.Body.String()
	for _, name := range []string{
		MetricOutboxPending, MetricOutboxOldestAge, MetricOutboxPublished, MetricOutboxFailed, MetricLeader, MetricBuildInfo,
		MetricAPIInFlight, MetricAPICrossTeamNotFound, MetricGatewayReservationsExpired, MetricCURVariance, MetricReportRollupRuns, MetricReportRollupLastSuccess, "go_goroutines",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics lacks %s", name)
		}
	}
	if !strings.Contains(body, `afs_build_info{service="afs-reconciler",version="v1.2.3"} 1`) {
		t.Error("build info missing")
	}
	m.SetLeader(false)
	if testutil.ToFloat64(m.Leader) != 0 {
		t.Error("leader not cleared")
	}
}

func TestEveryMetricNameIsRegistered(t *testing.T) {
	m := NewMetrics("x", "v")
	// Vec metrics appear in the gathered output only after a child exists; touch each.
	m.APIRequests.WithLabelValues("r", "GET", "200")
	m.APIRequestDuration.WithLabelValues("r", "GET")
	m.ReconcileDuration.WithLabelValues("team")
	m.ReconcileErrors.WithLabelValues("team")
	m.ReconcileLag.WithLabelValues("team")
	m.DriftCorrected.WithLabelValues("team")
	m.GatewayReservationFailures.WithLabelValues("db")
	m.NotificationFailures.WithLabelValues("slack")
	m.GatewayUsageRowsDropped.WithLabelValues("queue_full")
	m.GatewayModelDenials.WithLabelValues("model_not_priced")
	m.RollupRuns.WithLabelValues("j", "ok")
	m.RollupLastSuccess.WithLabelValues("j")
	m.EdgeRequests.WithLabelValues("web_ui", "200")
	m.EdgeRequestSecs.WithLabelValues("web_ui")
	mfs, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, mf := range mfs {
		have[mf.GetName()] = true
	}
	for _, n := range AllMetricNames() {
		if !have[n] {
			t.Errorf("%s declared in names.go but not registered", n)
		}
	}
}

func TestHealthEndpoints(t *testing.T) {
	var dbDown atomic.Bool
	var leader atomic.Bool
	checks := []Check{
		{Name: "database", Fn: func(context.Context) error {
			if dbDown.Load() {
				return errors.New("ping failed: password=hunter2hunter2 refused")
			}
			return nil
		}},
		SchemaCheck(func(context.Context) (int, error) { return 16, nil }, 16),
		LeaderCheck(leader.Load),
	}
	srv := httptest.NewServer(OpsServer(":0", NewMetrics("afs-api", "v"), checks...).Handler)
	defer srv.Close()
	get := func(path string) (int, map[string]any) {
		res, err := httpGet(t, srv.URL+path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return res.StatusCode, m
	}
	if c, b := get("/healthz"); c != 200 || b["status"] != "ok" {
		t.Errorf("healthz %d %v", c, b)
	}
	c, b := get("/readyz")
	checksOut := b["checks"].(map[string]any)
	if c != 200 || b["status"] != "ready" || checksOut["leader"] != "standby" || checksOut["database"] != "ok" || checksOut["schema"] != "ok" {
		t.Errorf("a standby with a healthy database must be ready: %d %v", c, b)
	}
	dbDown.Store(true)
	c, b = get("/readyz")
	if c != 503 || b["status"] != "not ready" {
		t.Fatalf("database down: %d %v", c, b)
	}
	if s, _ := b["checks"].(map[string]any)["database"].(string); strings.Contains(s, "hunter2hunter2") {
		t.Errorf("readyz leaked a secret: %q", s)
	}
	// liveness is independent of dependencies
	if c, _ := get("/healthz"); c != 200 {
		t.Error("healthz depends on the database")
	}
	if res, err := httpGet(t, srv.URL+"/metrics"); err != nil || res.StatusCode != 200 {
		t.Errorf("metrics %v %v", res, err)
	} else {
		_ = res.Body.Close()
	}
}

func TestSchemaCheck(t *testing.T) {
	tests := []struct {
		name    string
		have    int
		err     error
		wantErr bool
	}{
		{"current", 16, nil, false},
		{"newer is fine", 17, nil, false},
		{"older refuses", 15, nil, true},
		{"unreadable refuses", 0, errors.New("permission denied"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := SchemaCheck(func(context.Context) (int, error) { return tt.have, tt.err }, 16).Fn(t.Context())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestReadyzTimesOutAHungCheck(t *testing.T) {
	h := Readyz(Check{Name: "db", Fn: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }})
	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/readyz", nil))
	if rec.Code != 503 || time.Since(start) > checkTimeout+time.Second {
		t.Fatalf("code %d after %v", rec.Code, time.Since(start))
	}
}

func TestServeOpsStopsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- ServeOps(ctx, OpsServer("127.0.0.1:0", nil)) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ops server did not stop")
	}
	if err := ServeOps(t.Context(), OpsServer("not-an-address", nil)); err == nil {
		t.Fatal("a bad address must fail")
	}
}

func TestOTelDisabledWithoutEndpoint(t *testing.T) {
	sd, err := SetupOTel(t.Context(), OTelConfig{Service: "afs-api"})
	if err != nil || sd(t.Context()) != nil {
		t.Fatalf("err %v", err)
	}
}

func TestOTelExportsTracesAndMetrics(t *testing.T) {
	var traces, metrics atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch r.URL.Path {
		case "/v1/traces":
			traces.Add(1)
		case "/v1/metrics":
			metrics.Add(1)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer srv.Close()
	sd, err := SetupOTel(t.Context(), OTelConfig{Endpoint: srv.URL, Insecure: true, Service: "afs-api", Version: "v1", MetricInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := otel.Tracer("test").Start(t.Context(), "op")
	var buf bytes.Buffer
	NewLogger(&buf, LogConfig{}).InfoContext(ctx, "inside span")
	span.End()
	c, _ := otel.Meter("test").Int64Counter("afs_test_counter")
	c.Add(ctx, 1)
	if err := sd(t.Context()); err != nil {
		t.Fatal(err)
	}
	if traces.Load() == 0 || metrics.Load() == 0 {
		t.Fatalf("exported traces=%d metrics=%d", traces.Load(), metrics.Load())
	}
	var m map[string]any
	_ = json.Unmarshal(buf.Bytes(), &m)
	if m["traceId"] == nil || m["spanId"] == nil {
		t.Errorf("log carries no trace ids: %s", buf.String())
	}
}

func httpGet(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// C-7: the gateway counters of FR-R05 and FR-R08 are registered with the accessors stream B binds.
func TestGatewayCountersAndNotificationAccessors(t *testing.T) {
	m := NewMetrics("x", "v")
	m.SettleFailed()
	m.SettleFailed()
	m.UsageRowDropped("queue_full")
	m.UsageRowDropped("write_failed")
	m.UsageRowDropped("write_failed")
	m.NotificationFailed("slack")
	if got := testutil.ToFloat64(m.GatewaySettleFailures); got != 2 {
		t.Errorf("settle failures = %v", got)
	}
	if got := testutil.ToFloat64(m.GatewayUsageRowsDropped.WithLabelValues("write_failed")); got != 2 {
		t.Errorf("dropped write_failed = %v", got)
	}
	if got := testutil.ToFloat64(m.GatewayUsageRowsDropped.WithLabelValues("queue_full")); got != 1 {
		t.Errorf("dropped queue_full = %v", got)
	}
	if got := testutil.ToFloat64(m.NotificationFailures.WithLabelValues("slack")); got != 1 {
		t.Errorf("notification failures = %v", got)
	}
}

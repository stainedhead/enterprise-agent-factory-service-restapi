package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
)

func TestMetricsRED(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		path      string
		handler   http.HandlerFunc
		wantRoute string
		wantCode  string
	}{
		{"operation set by the router", "GET", "/v1/teams/team_01JD3Z5Y8K2M4N6P8Q0R2S4T6V", func(w http.ResponseWriter, r *http.Request) {
			SetRoute(r.Context(), "GetTeam")
			_, _ = w.Write([]byte("ok"))
		}, "GetTeam", "200"},
		{"explicit status", "POST", "/v1/teams", func(w http.ResponseWriter, r *http.Request) {
			SetRoute(r.Context(), "CreateTeam")
			w.WriteHeader(http.StatusCreated)
		}, "CreateTeam", "201"},
		{"error status", "DELETE", "/v1/teams/team_x", func(w http.ResponseWriter, r *http.Request) {
			SetRoute(r.Context(), "DeleteTeam")
			w.WriteHeader(http.StatusForbidden)
		}, "DeleteTeam", "403"},
		{"fallback collapses ids", "GET", "/v1/teams/team_01JD3Z5Y8K2M4N6P8Q0R2S4T6V/agents/agt_01JD3Z5Y8K2M4N6P8Q0R2S4T6V", func(w http.ResponseWriter, _ *http.Request) {}, "/v1/teams/{id}/agents/{id}", "200"},
		{"unknown path is one label", "GET", "/v1/whatever/zzz/1", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }, "unmatched", "404"},
		{"outside v1", "GET", "/favicon.ico", func(w http.ResponseWriter, _ *http.Request) {}, "unmatched", "200"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := observability.NewMetrics("afs-api", "v")
			h := Metrics(m, tt.handler)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil))
			if got := testutil.ToFloat64(m.APIRequests.WithLabelValues(tt.wantRoute, tt.method, tt.wantCode)); got != 1 {
				t.Fatalf("requests{%s,%s,%s} = %v", tt.wantRoute, tt.method, tt.wantCode, got)
			}
			if n := testutil.CollectAndCount(m.APIRequestDuration); n != 1 {
				t.Errorf("duration series = %d", n)
			}
			if testutil.ToFloat64(m.APIInFlight) != 0 {
				t.Error("in-flight gauge not released")
			}
		})
	}
}

func TestMetricsPanicStillRecordsAndReleases(t *testing.T) {
	m := observability.NewMetrics("afs-api", "v")
	h := Metrics(m, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), "GET", "/v1/teams", nil))
	}()
	if testutil.ToFloat64(m.APIInFlight) != 0 {
		t.Fatal("in-flight gauge leaked on panic")
	}
}

func TestStatusWriterKeepsFlushAndUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec}
	sw.Flush()
	if !rec.Flushed || sw.Unwrap() != http.ResponseWriter(rec) {
		t.Fatal("flush or unwrap lost")
	}
	sw.WriteHeader(202)
	sw.WriteHeader(500) // the first status wins, as net/http does
	if sw.code != 202 {
		t.Fatalf("code = %d", sw.code)
	}
}

func TestSetRouteWithoutHolderIsHarmless(t *testing.T) {
	SetRoute(t.Context(), "X")
}

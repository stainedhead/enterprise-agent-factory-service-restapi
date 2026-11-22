package httpapi

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler(nil).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestReadyzReportsDependencyFailure(t *testing.T) {
	h := NewHandler(func(*http.Request) error { return errors.New("db down") })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// D-1 (FR-R03): the public /readyz body never carries the dependency's error text; the cause goes to
// the log with the request id.
func TestReadyzRedactsDependencyErrorText(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	h := NewHandler(func(*http.Request) error {
		return errors.New("failed to connect to user=afs_app host=10.1.2.3")
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if want := `{"status":"not ready","checks":{"database":"unavailable"}}`; strings.TrimSpace(body) != want {
		t.Fatalf("body = %s, want %s", body, want)
	}
	for _, leak := range []string{"afs_app", "10.1.2.3"} {
		if strings.Contains(body, leak) {
			t.Fatalf("body leaks %q: %s", leak, body)
		}
	}
	got := logs.String()
	rid := rec.Header().Get("X-Request-Id")
	if !strings.Contains(got, "10.1.2.3") || rid == "" || !strings.Contains(got, rid) {
		t.Fatalf("log must carry the cause and request id %q: %s", rid, got)
	}
}

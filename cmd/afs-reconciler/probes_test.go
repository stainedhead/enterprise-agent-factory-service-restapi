package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
	return rec
}

func TestProbes(t *testing.T) {
	pingErr := error(nil)
	leader := false
	h := probeHandler(probes{
		Ping:   func(context.Context) error { return pingErr },
		Leader: func() bool { return leader },
		Stats: func() engine.Stats {
			return engine.Stats{Reconciled: 5, Failed: 2, QueueDepth: 3, OldestOverdue: 1500 * time.Millisecond}
		},
	})
	if r := get(h, "/healthz"); r.Code != 200 {
		t.Errorf("healthz = %d", r.Code)
	}
	if r := get(h, "/readyz"); r.Code != 200 || !strings.Contains(r.Body.String(), `"leader":false`) {
		t.Errorf("standby readyz = %d %s", r.Code, r.Body)
	}
	leader = true
	if r := get(h, "/readyz"); !strings.Contains(r.Body.String(), `"leader":true`) {
		t.Errorf("leader readyz = %s", r.Body)
	}
	pingErr = errors.New("password authentication failed for user x")
	r := get(h, "/readyz")
	if r.Code != 503 || strings.Contains(r.Body.String(), "password") {
		t.Errorf("unready readyz = %d %s (must not leak the cause)", r.Code, r.Body)
	}
	m := get(h, "/metrics").Body.String()
	for _, want := range []string{"afs_reconciler_leader 1", `afs_reconciler_reconciles_total{result="failure"} 2`, "afs_reconciler_queue_depth 3", "afs_reconciler_lag_seconds 1.5"} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q:\n%s", want, m)
		}
	}
}

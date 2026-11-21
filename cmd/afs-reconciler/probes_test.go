package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// M1-L2/F7: the engine's counters and queue are exposed on the shared registry that observability serves.
func TestEngineStatsAreRegistered(t *testing.T) {
	m := observability.NewMetrics("afs-reconciler", "v1")
	registerEngineStats(m, func() engine.Stats {
		return engine.Stats{Reconciled: 5, Failed: 2, QueueDepth: 3, OldestOverdue: 1500 * time.Millisecond, LastPoll: time.Unix(1_800_000_000, 0)}
	})
	m.SetLeader(true)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	for _, want := range []string{
		"afs_reconciler_is_leader 1",
		`afs_reconciler_reconciles_total{result="success"} 5`,
		`afs_reconciler_reconciles_total{result="failure"} 2`,
		"afs_reconciler_queue_depth 3",
		"afs_reconciler_oldest_overdue_seconds 1.5",
		"afs_reconciler_last_poll_timestamp_seconds 1.8e+09",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

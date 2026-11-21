package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// probes is what the probe and metrics endpoints report on.
type probes struct {
	// Ping checks the database.
	Ping func(ctx context.Context) error
	// Leader reports whether this replica leads.
	Leader func() bool
	// Stats snapshots the engine.
	Stats func() engine.Stats
}

func (r *reconciler) probes() probes {
	return probes{
		Ping:   func(ctx context.Context) error { return r.Pool.Ping(ctx) },
		Leader: r.Elector.IsLeader,
		Stats:  r.Engine.Stats,
	}
}

// probeHandler serves /healthz (the process is up), /readyz (the database answers; the body says whether
// this replica leads, since a standby is ready) and /metrics (Prometheus text format).
func probeHandler(p probes) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{"leader": p.Leader()}
		if err := p.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			body["status"], body["error"] = "unavailable", "database: not reachable"
		} else {
			body["status"] = "ready"
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		st := p.Stats()
		leader := 0
		if p.Leader() {
			leader = 1
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt := errWriter{w: w}
		fmt.printf("# TYPE afs_reconciler_leader gauge\nafs_reconciler_leader %d\n", leader)
		fmt.printf("# TYPE afs_reconciler_reconciles_total counter\nafs_reconciler_reconciles_total{result=\"success\"} %d\nafs_reconciler_reconciles_total{result=\"failure\"} %d\n", st.Reconciled, st.Failed)
		fmt.printf("# TYPE afs_reconciler_queue_depth gauge\nafs_reconciler_queue_depth %d\n", st.QueueDepth)
		fmt.printf("# TYPE afs_reconciler_lag_seconds gauge\nafs_reconciler_lag_seconds %g\n", st.OldestOverdue.Seconds())
		fmt.printf("# TYPE afs_reconciler_last_poll_timestamp_seconds gauge\nafs_reconciler_last_poll_timestamp_seconds %d\n", unixOrZero(st.LastPoll))
		fmt.printf("# TYPE afs_reconciler_last_resync_timestamp_seconds gauge\nafs_reconciler_last_resync_timestamp_seconds %d\n", unixOrZero(st.LastResync))
	})
	return mux
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// errWriter formats to w and ignores write errors: a scrape that disconnects needs no handling.
type errWriter struct{ w io.Writer }

func (e errWriter) printf(format string, a ...any) { _, _ = fmt.Fprintf(e.w, format, a...) }

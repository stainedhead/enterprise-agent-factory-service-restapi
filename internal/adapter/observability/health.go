package observability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// Check is one readiness check.
type Check struct {
	// Name appears in the /readyz body ("database", "schema", "leader").
	Name string
	// Fn returns nil when the dependency is ready.
	Fn func(ctx context.Context) error
	// Informational checks are reported but never make the binary not ready: a standby reconciler
	// replica is ready although it is not the leader.
	Informational bool
}

// SchemaCheck returns a Check that fails when the database schema is older than want. have reads
// the current version (postgres.CheckSchema's tracking-table read, or afs.schema_version()).
func SchemaCheck(have func(ctx context.Context) (int, error), want int) Check {
	return Check{Name: "schema", Fn: func(ctx context.Context) error {
		v, err := have(ctx)
		if err != nil {
			return fmt.Errorf("read schema version: %w", err)
		}
		if v < want {
			return fmt.Errorf("schema version %d is older than the %d this binary needs", v, want)
		}
		return nil
	}}
}

// LeaderCheck reports leadership for /readyz without affecting readiness.
func LeaderCheck(isLeader func() bool) Check {
	return Check{Name: "leader", Informational: true, Fn: func(context.Context) error {
		if isLeader() {
			return nil
		}
		return errors.New("standby")
	}}
}

// checkTimeout bounds each readiness check so a hung database cannot hang the probe.
const checkTimeout = 3 * time.Second

type readyBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// Healthz is the liveness handler: it answers 200 whenever the process can serve a request and
// checks no dependency, so a database outage never restarts the binary.
func Healthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

// Readyz is the readiness handler: 200 with every check "ok", or 503 naming the failing checks.
// Informational checks are listed but never fail the probe. Error text is the check's own message,
// which must not contain a secret (check functions return their own wording, not driver strings).
func Readyz(checks ...Check) http.Handler {
	sorted := append([]Check(nil), checks...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readyBody{Status: "ready", Checks: map[string]string{}}
		code := http.StatusOK
		for _, c := range sorted {
			ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
			err := c.Fn(ctx)
			cancel()
			switch {
			case err == nil:
				body.Checks[c.Name] = "ok"
			case c.Informational:
				body.Checks[c.Name] = err.Error()
			default:
				body.Checks[c.Name] = Scrub(err.Error())
				body.Status, code = "not ready", http.StatusServiceUnavailable
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
}

// OpsServer is the operations HTTP server every binary runs on AFS_METRICS_ADDR: /metrics, /healthz
// and /readyz. It is separate from the public listener so probes and scrapes never pass the
// public middleware chain.
func OpsServer(addr string, m *Metrics, checks ...Check) *http.Server {
	mux := http.NewServeMux()
	if m != nil {
		mux.Handle("GET /metrics", m.Handler())
	}
	mux.Handle("GET /healthz", Healthz())
	mux.Handle("GET /readyz", Readyz(checks...))
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}

// ServeOps runs srv until ctx ends, then shuts it down gracefully.
func ServeOps(ctx context.Context, srv *http.Server) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// ctx is already done; the shutdown gets its own deadline but keeps ctx's values.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}

// Command afs-api serves the Agent Factory control plane REST API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "orchestration-backfill" {
		os.Exit(runBackfill())
	}
	addr := flag.String("addr", "", "listen address (default AFS_API_ADDR, then :8080)")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.String("afs-api"))
		return
	}

	// The redacting logger from the first line on: nothing wiring logs may carry a token or key.
	log := observability.NewLogger(os.Stderr, observability.LogConfig{Level: os.Getenv("AFS_LOG_LEVEL"), Service: "afs-api", RequestID: httpapi.RequestIDFrom})
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := settingsFrom(os.Getenv)
	if err != nil {
		log.Error("afs-api configuration", "err", err)
		os.Exit(2)
	}
	if *addr == "" {
		*addr = s.Addr
	}
	c, err := wire(ctx, log, s)
	if err != nil {
		log.Error("afs-api cannot start", "err", err)
		os.Exit(1)
	}
	defer c.Close()
	// Probes and metrics on AFS_METRICS_ADDR (default :9090), apart from the public listener.
	oc := observability.EnvConfig("afs-api", buildinfo.Version, os.Getenv)
	oc.Metrics, oc.Checks, oc.RequestID = c.Metrics, c.Checks, httpapi.RequestIDFrom
	rt, err := observability.Start(ctx, oc)
	if err != nil {
		log.Error("afs-api observability", "err", err)
		os.Exit(1)
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = rt.Shutdown(sctx)
	}()
	log = rt.Log
	srv := newServer(*addr, c.Handler, s)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("afs-api starting", "addr", *addr, "version", buildinfo.Version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("afs-api stopped", "err", err)
		os.Exit(1)
	}
}

// newServer is the public listener: header, whole-request and idle timeouts bound slow clients
// (FR-R09). There is no WriteTimeout, so long exports and downloads are not cut.
func newServer(addr string, h http.Handler, s settings) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       s.HTTPReadTimeout,
		IdleTimeout:       s.HTTPIdleTimeout,
	}
}

// runBackfill is `afs-api orchestration-backfill`: it creates the orchestration agent of every team that has
// none, in batches of 20, as system:migration (RA-US-21). The Helm upgrade hook runs it once after the
// migrations; it is idempotent and resumable, so a rerun after a failure continues. Exit codes: 0 done,
// 1 some team could not be done (rerun), 2 configuration.
func runBackfill() int {
	log := observability.NewLogger(os.Stderr, observability.LogConfig{Level: os.Getenv("AFS_LOG_LEVEL"), Service: "afs-api"})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s, err := settingsFrom(os.Getenv)
	if err != nil {
		log.Error("orchestration-backfill configuration", "err", err)
		return 2
	}
	c, err := wire(ctx, log, s)
	if err != nil {
		log.Error("orchestration-backfill cannot start", "err", err)
		return 1
	}
	defer c.Close()
	if c.teamSvc == nil {
		log.Error("orchestration-backfill needs AFS_DATABASE_URL")
		return 2
	}
	return backfillWith(ctx, c.teamSvc, log)
}

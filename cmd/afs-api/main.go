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
	addr := flag.String("addr", envOr("AFS_API_ADDR", ":8080"), "listen address")
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.String("afs-api"))
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := settingsFrom(os.Getenv)
	if err != nil {
		log.Error("afs-api configuration", "err", err)
		os.Exit(2)
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
	srv := &http.Server{
		Addr:              *addr,
		Handler:           c.Handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

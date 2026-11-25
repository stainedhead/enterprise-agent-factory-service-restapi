// Command afs-gateway is the Bedrock proxy that meters tokens per team and enforces hard budget caps.
// It identifies an agent by its pod's source IP, reserves the worst-case cost before it calls Bedrock
// with the team's own role, and settles on the tokens used (gateway contract v1.0.0).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo"
)

func main() {
	version := flag.Bool("version", false, "print version and exit")
	teamNS := flag.String("team-namespace", "", "run as the team gateway of this namespace (gateway contract §12.1); needs AFS_GATEWAY_TEAM_ID")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.String("afs-gateway"))
		return
	}
	log := observability.NewLogger(os.Stderr, observability.LogConfig{Level: os.Getenv("AFS_LOG_LEVEL"), Service: "afs-gateway"})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	env := os.Getenv
	if *teamNS != "" {
		env = func(k string) string {
			if k == "AFS_GATEWAY_TEAM_NAMESPACE" {
				return *teamNS
			}
			return os.Getenv(k)
		}
	}
	if err := run(ctx, log, env); err != nil {
		log.Error("afs-gateway failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, env func(string) string) error {
	s, err := settingsFrom(env)
	if err != nil {
		return err
	}
	certs, err := newCertReloader(s.TLSCert, s.TLSKey)
	if err != nil {
		return err
	}
	m := observability.NewMetrics("afs-gateway", buildinfo.Version)
	app, err := wire(ctx, log, s, env, m)
	if err != nil {
		return err
	}
	defer app.Close()

	// JSON logs with redaction, /metrics, /healthz and /readyz on AFS_METRICS_ADDR, OTLP export.
	oc := observability.EnvConfig("afs-gateway", buildinfo.Version, env)
	oc.Metrics, oc.Checks = m, app.Checks
	rt, err := observability.Start(ctx, oc)
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = rt.Shutdown(sctx)
	}()
	log = rt.Log

	// The usage writer outlives the signal: rows of calls still draining below must be written. Close stops it.
	app.Usage.Run(context.WithoutCancel(ctx))
	if app.Mcp != nil {
		app.Mcp.Ledger.Run(context.WithoutCancel(ctx))
	}
	bg, cancelBG := context.WithCancel(ctx)
	defer cancelBG()
	var wg sync.WaitGroup
	for _, f := range app.Background {
		wg.Go(func() {
			if err := f(bg); err != nil && bg.Err() == nil {
				log.Error("gateway background task", "err", err)
			}
		})
	}

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: app.Handler.Handler(), TLSConfig: certs.tlsConfig(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	log.Info("afs-gateway serving", "version", buildinfo.Version, "addr", ln.Addr().String())

	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	// Drain: stop accepting, let streams finish, then close the books and flush usage rows.
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if serr := srv.Shutdown(sctx); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		log.Warn("gateway shutdown", "err", serr)
	}
	cancelBG()
	wg.Wait()
	// Close the books: finished calls first, then one last attempt for settlements still queued (bounded
	// by SettleTimeout), then the usage rows those produced.
	app.Service.Close(context.WithoutCancel(ctx))
	app.Usage.Close()
	if app.Mcp != nil {
		app.Mcp.Ledger.Close()
	}
	log.Info("afs-gateway stopped")
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

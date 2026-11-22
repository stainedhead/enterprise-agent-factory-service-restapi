// Command afs-edge is the only internet-reachable part of the Paperclip collaboration feature. It sits behind
// the ALB, signs people in, decides with internal/authz, scans what people send for credentials, counts
// access, and forwards to a team's Paperclip (API contract 4.4c, EDGE-1..7). It owns no tables: it
// connects as afs_edge, which can execute ten database functions and nothing else.
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
	"syscall"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo"
)

func main() {
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.String("afs-edge"))
		return
	}
	log := observability.NewLogger(os.Stderr, observability.LogConfig{Level: os.Getenv("AFS_LOG_LEVEL"), Service: "afs-edge"})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, os.Getenv); err != nil {
		log.Error("afs-edge failed", "err", err)
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
	m := observability.NewMetrics("afs-edge", buildinfo.Version)
	app, err := wire(ctx, log, s, env, m)
	if err != nil {
		return err
	}
	defer app.Close()

	oc := observability.EnvConfig("afs-edge", buildinfo.Version, env)
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

	// The loops outlive the signal only for the final flush, which Run performs itself.
	loops := make(chan struct{})
	go func() {
		defer close(loops)
		app.Run(ctx)
	}()
	feedCtx, cancelFeed := context.WithCancel(ctx)
	defer cancelFeed()
	go func() {
		if err := app.Feed(feedCtx); err != nil && feedCtx.Err() == nil {
			log.Error("change feed ended", "err", err)
		}
	}()

	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: app.Handler, TLSConfig: certs.tlsConfig(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(ln, "", "") }()
	log.Info("afs-edge serving", "version", buildinfo.Version, "addr", ln.Addr().String())

	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	// Drain: stop accepting and let requests finish; hijacked WebSocket tunnels end with their idle timeout
	// or when the pod stops. Then the final flush of the access counts.
	sctx, cancel := context.WithTimeout(context.Background(), s.DrainTimeout)
	defer cancel()
	if serr := srv.Shutdown(sctx); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
		log.Warn("edge shutdown", "err", serr)
	}
	cancelFeed()
	<-loops
	log.Info("afs-edge stopped")
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

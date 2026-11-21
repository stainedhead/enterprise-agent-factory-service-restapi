// Command afs-reconciler applies desired state from PostgreSQL to EKS and AWS, at startup and
// continuously. One replica leads (a Lease); the others wait and report ready.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo"
)

func main() {
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.String("afs-reconciler"))
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, os.Getenv); err != nil {
		log.Error("afs-reconciler failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, env func(string) string) error {
	s, err := settingsFrom(env, os.Hostname)
	if err != nil {
		return err
	}
	log.Info("afs-reconciler starting", "version", buildinfo.Version, "identity", s.PodName)
	r, err := wire(ctx, log, s, env)
	if err != nil {
		return err
	}
	defer r.Close()

	srv := &http.Server{Addr: s.MetricsAddr, Handler: probeHandler(r.probes()), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("probe server", "err", err)
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	// Leadership: the engine and the drift watch run only while this replica holds the Lease; losing it
	// cancels them, and the elector campaigns again.
	err = r.Elector.Run(ctx, func(lctx context.Context) {
		log.Info("leading")
		go func() {
			if err := r.Watcher.Run(lctx); err != nil && lctx.Err() == nil {
				log.Error("drift watch", "err", err)
			}
		}()
		if err := r.Engine.Run(lctx); err != nil && lctx.Err() == nil {
			log.Error("engine", "err", err)
		}
		log.Info("leadership ended")
	})
	log.Info("afs-reconciler stopped")
	return err
}

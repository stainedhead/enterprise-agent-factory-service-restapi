// Command afs-gateway is the Bedrock proxy that meters tokens per team and enforces hard budget caps.
//
// Scaffold only: the service loop is specified in the solution design and built under specs/.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo"
)

func main() {
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.String("afs-gateway"))
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// JSON logs with redaction, /metrics, /healthz and /readyz on AFS_METRICS_ADDR, OTLP export.
	rt, err := observability.Start(ctx, observability.EnvConfig("afs-gateway", buildinfo.Version, os.Getenv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "afs-gateway:", err)
		os.Exit(1)
	}
	defer func() { _ = rt.Shutdown(context.Background()) }()
	log := rt.Log

	log.Info("afs-gateway starting", "version", buildinfo.Version)
	<-ctx.Done()
	log.Info("afs-gateway stopped")
}

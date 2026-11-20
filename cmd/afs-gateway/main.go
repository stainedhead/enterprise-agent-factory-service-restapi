// Command afs-gateway is the Bedrock proxy that meters tokens per team and enforces hard budget caps.
//
// Scaffold only: the service loop is specified in the solution design and built under specs/.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo"
)

func main() {
	version := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *version {
		fmt.Println(buildinfo.String("afs-gateway"))
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("afs-gateway starting", "version", buildinfo.Version)
	<-ctx.Done()
	log.Info("afs-gateway stopped")
}

// Command afs-mcp-bridge runs a stdio MCP server as a child process and serves it as a Streamable HTTP
// endpoint for the team gateway (MCP PRD §10.2). It runs as a sidecar in the hosted server's pod:
//
//	afs-mcp-bridge --listen :8080 -- /app/server --flag
//
// The child gets only the environment variables named by --pass-env (the hosted spec's env_names) plus
// PATH and HOME, so nothing else of the pod's environment reaches the server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpbridge"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo"
)

const (
	exitFailed = 1
	exitUsage  = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil)
	stop()
	os.Exit(code)
}

// run parses flags, starts the bridge and serves until ctx ends. ready, when set, receives the bound
// address (for tests).
func run(ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer, ready chan<- string) int {
	fs := flag.NewFlagSet("afs-mcp-bridge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.Bool("version", false, "print version and exit")
	listen := fs.String("listen", ":8080", "address to serve /mcp and /healthz on")
	timeout := fs.Duration("timeout", 60*time.Second, "per-request timeout")
	maxRequest := fs.Int64("max-request-bytes", 1<<20, "largest request body accepted")
	passEnv := fs.String("pass-env", "", "comma-separated environment variable names passed to the server")
	dir := fs.String("dir", "", "working directory of the server")
	install := fs.String("install", "", "copy this binary into the directory and exit (the init container of a hosted server's pod)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *version {
		_, _ = fmt.Fprintln(stdout, buildinfo.String("afs-mcp-bridge"))
		return 0
	}
	if *install != "" {
		if err := installSelf(*install); err != nil {
			_, _ = fmt.Fprintln(stderr, "afs-mcp-bridge:", err)
			return exitFailed
		}
		return 0
	}
	command := fs.Args()
	if len(command) == 0 {
		_, _ = fmt.Fprintln(stderr, "afs-mcp-bridge: the server command is required after --")
		return exitUsage
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil)).With("component", "afs-mcp-bridge")
	b, err := mcpbridge.New(mcpbridge.Config{
		Command: command, Env: childEnv(*passEnv, env), Dir: *dir,
		Timeout: *timeout, MaxRequest: *maxRequest, Log: log,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "afs-mcp-bridge:", err)
		return exitUsage
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *listen)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "afs-mcp-bridge:", err)
		return exitFailed
	}
	srv := &http.Server{Handler: b, ReadHeaderTimeout: 10 * time.Second}
	runDone := make(chan struct{})
	go func() { _ = b.Run(ctx); close(runDone) }()
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	if ready != nil {
		ready <- ln.Addr().String()
	}
	log.Info("serving", "addr", ln.Addr().String(), "command", command[0])
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			_, _ = fmt.Fprintln(stderr, "afs-mcp-bridge:", err)
			<-runDone
			return exitFailed
		}
	}
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	<-runDone
	return 0
}

// childEnv builds the server's environment: PATH, HOME and the named variables that are set.
func childEnv(names string, env func(string) string) []string {
	out := []string{}
	for _, n := range append([]string{"PATH", "HOME"}, strings.Split(names, ",")...) {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if v := env(n); v != "" {
			out = append(out, n+"="+v)
		}
	}
	return out
}

// installSelf copies the running executable to dir/afs-mcp-bridge, executable by all, so the server container
// of a hosted pod can run the bridge from a shared volume.
func installSelf(dir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(self) //nolint:gosec // the running executable
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(filepath.Join(dir, "afs-mcp-bridge"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755) //nolint:gosec // an executable
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

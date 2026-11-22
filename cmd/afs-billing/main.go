// Command afs-billing is the billing job runner: one Kubernetes CronJob per job, each running
// afs-billing --job <name> and exiting. Jobs (cur-ingest, reconcile, price-sync, and the report and
// showback jobs of M4-E) read and write the database as the afs_billing role and call AWS (Athena,
// the Price List, S3) with the pod's own identity.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo"
)

// Exit codes: 0 success, 1 the job failed, 2 usage.
const (
	exitFailed = 1
	exitUsage  = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run parses the command line, validates the job name, builds the app and runs the job.
func run(ctx context.Context, args []string, env func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("afs-billing", flag.ContinueOnError)
	fs.SetOutput(stderr)
	version := fs.Bool("version", false, "print version and exit")
	job := fs.String("job", "", "job to run: "+strings.Join(jobNames(), ", "))
	day := fs.String("day", "", "UTC day to process, YYYY-MM-DD (default: the lookback days ending yesterday)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *version {
		_, _ = fmt.Fprintln(stdout, buildinfo.String("afs-billing"))
		return 0
	}
	if *job == "" {
		_, _ = fmt.Fprintf(stderr, "afs-billing: --job is required (jobs: %s)\n", strings.Join(jobNames(), ", "))
		return exitUsage
	}
	if _, ok := jobs[*job]; !ok {
		_, _ = fmt.Fprintf(stderr, "afs-billing: unknown job %q (jobs: %s)\n", *job, strings.Join(jobNames(), ", "))
		return exitUsage
	}
	var in jobInput
	if *day != "" {
		d, err := time.Parse(time.DateOnly, *day)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "afs-billing: --day %q is not a date (YYYY-MM-DD)\n", *day)
			return exitUsage
		}
		if !d.Before(time.Now().UTC().Truncate(24 * time.Hour)) {
			_, _ = fmt.Fprintf(stderr, "afs-billing: --day %s is not complete yet\n", *day)
			return exitUsage
		}
		in.Day = &d
	}

	a, closeApp, err := open(ctx, env)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "afs-billing:", err)
		return exitFailed
	}
	defer closeApp()
	if err := execute(ctx, a, *job, in); err != nil {
		if errors.Is(err, context.Canceled) {
			a.log.Warn("job stopped by a signal", "job", *job)
		}
		return exitFailed
	}
	return 0
}

// open builds the app: telemetry (JSON logs with redaction, /metrics, /healthz and /readyz on
// AFS_METRICS_ADDR, OTLP export), the database pool with the schema version check, and the lazy AWS
// configuration. It is the composition root of afs-billing.
func open(ctx context.Context, env func(string) string) (*app, func(), error) {
	s, err := settingsFrom(env)
	if err != nil {
		return nil, nil, err
	}
	m := observability.NewMetrics("afs-billing", buildinfo.Version)
	a := &app{settings: s, env: env, metrics: m, now: func() time.Time { return time.Now().UTC() }}
	a.awsConfig = func(ctx context.Context) (awssdk.Config, error) {
		as, err := aws.SettingsFrom(env)
		if err != nil {
			return awssdk.Config{}, err
		}
		return aws.Load(ctx, as)
	}

	cfg := observability.EnvConfig("afs-billing", buildinfo.Version, env)
	cfg.Metrics = m
	// Readiness: the database answers and the schema is not older than this binary needs.
	cfg.Checks = []observability.Check{{Name: "database", Fn: func(ctx context.Context) error {
		if a.pool == nil {
			return errors.New("database: not open")
		}
		if err := a.pool.Ping(ctx); err != nil {
			return errors.New("database: not reachable") // never the driver's text
		}
		return nil
	}}}
	rt, err := observability.Start(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	a.log = rt.Log
	shutdown := func() { _ = rt.Shutdown(context.WithoutCancel(ctx)) }

	dbCfg := postgres.Config{URL: s.DatabaseURL, Service: "afs-billing", SystemActor: true, MaxConns: 4, StatementTimeout: 5 * time.Minute}
	if s.DBIAMAuth {
		awsCfg, err := a.aws(ctx)
		if err != nil {
			shutdown()
			return nil, nil, err
		}
		region := awsCfg.Region
		if dbCfg.Password, err = postgres.IAMPassword(s.DatabaseURL, region, awsCfg.Credentials); err != nil {
			shutdown()
			return nil, nil, err
		}
	}
	pool, err := postgres.OpenPool(ctx, dbCfg)
	if err != nil {
		shutdown()
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	a.pool = pool
	closeApp := func() { pool.Close(); shutdown() }
	if have, want, err := postgres.CheckSchema(ctx, pool); err != nil {
		closeApp()
		return nil, nil, fmt.Errorf("schema version %d, this binary needs %d: %w", have, want, err)
	}
	a.runner = postgres.NewTxRunner(pool)
	a.store = postgres.NewBillingStore(a.runner)
	a.log.Info("afs-billing ready", "version", buildinfo.Version)
	return a, closeApp, nil
}

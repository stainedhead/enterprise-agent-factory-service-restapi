package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur"
)

// A job is one run of a Helm CronJob: afs-billing --job <name>. Each job file registers its jobs in an
// init function, so a stream adds a job with a new file and no edit here.
//
//	cur-ingest        M4-C  CUR through Athena into cost_daily
//	reconcile         M4-C  gateway estimate against CUR, variance events
//	price-sync        M4-D  AWS Price List into price proposals
//	report-schedules  M4-E  due scheduled reports to the reports bucket and SES/SNS (job_reports.go)
//	showback          M4-E  monthly showback files (job_reports.go)
type jobFunc func(ctx context.Context, a *app, in jobInput) error

var jobs = map[string]jobFunc{}

// register adds a job. Registering a name twice panics: each job has one owner.
func register(name string, f jobFunc) {
	if _, dup := jobs[name]; dup {
		panic("afs-billing: job " + name + " registered twice")
	}
	jobs[name] = f
}

func jobNames() []string {
	out := make([]string, 0, len(jobs))
	for n := range jobs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// jobInput is what the command line asks of a job.
type jobInput struct {
	// Day, when set, is the one UTC day to process (--day); otherwise Lookback days ending yesterday.
	Day *time.Time
}

// app is what a job runs on: logging, metrics, the database and, lazily, AWS.
type app struct {
	log      *slog.Logger
	metrics  *observability.Metrics
	settings settings
	env      func(string) string
	now      func() time.Time

	pool   *pgxpool.Pool
	runner *postgres.TxRunner
	store  *postgres.BillingStore

	// awsConfig builds the SDK configuration on first use; a job that does not call AWS never needs it.
	awsConfig func(ctx context.Context) (awssdk.Config, error)
	awsMu     sync.Mutex
	awsDone   bool
	awsCfg    awssdk.Config
	awsErr    error
}

// days returns the UTC days a CUR job covers.
func (a *app) days(in jobInput) []time.Time {
	if in.Day != nil {
		return []time.Time{in.Day.UTC().Truncate(24 * time.Hour)}
	}
	return cur.Days(a.now(), a.settings.Lookback)
}

// aws returns the SDK configuration, built once.
func (a *app) aws(ctx context.Context) (awssdk.Config, error) {
	a.awsMu.Lock()
	defer a.awsMu.Unlock()
	if a.awsDone {
		return a.awsCfg, a.awsErr
	}
	a.awsCfg, a.awsErr = a.awsConfig(ctx)
	a.awsDone = true
	return a.awsCfg, a.awsErr
}

// execute runs the named job and logs its outcome.
func execute(ctx context.Context, a *app, name string, in jobInput) error {
	f, ok := jobs[name]
	if !ok {
		return fmt.Errorf("unknown job %q (jobs: %s)", name, strings.Join(jobNames(), ", "))
	}
	start := a.now()
	a.log.Info("job started", "job", name)
	if err := f(ctx, a, in); err != nil {
		a.log.Error("job failed", "job", name, "err", err, "seconds", a.now().Sub(start).Seconds())
		return err
	}
	a.log.Info("job finished", "job", name, "seconds", a.now().Sub(start).Seconds())
	return nil
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

type ulidGen struct{}

func (ulidGen) NewID(p domain.IDPrefix) (domain.ID, error) {
	return domain.NewID(p, time.Now(), rand.Reader)
}

package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	awsx "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws"
	awss3 "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/s3"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/schedules"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/showback"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Job names of the report jobs (M4-E): the stream that owns the main loop selects them by flag.
const (
	jobReportSchedules = "report-schedules"
	jobShowback        = "showback"
)

// reportJobSet is the scheduled report runner (BI-5) and the showback generator (BI-4) with their
// dependencies. They need the database as afs_billing and the reports bucket.
type reportJobSet struct {
	Schedules *schedules.Runner
	Showback  *showback.Generator
	// ShowbackTagKey is the tag key of the second showback dimension (AFS_SHOWBACK_TAG_KEY); empty
	// writes cost centers only.
	ShowbackTagKey string
	Clock          port.Clock
	Log            *slog.Logger
}

// newReportJobs builds the report jobs from the environment: AFS_REPORTS_BUCKET (required),
// AFS_REPORTS_KMS_KEY_ID, AFS_SHOWBACK_TAG_KEY and the AWS settings of adapter/aws (region, optional
// endpoint). pool is the afs_billing pool.
func newReportJobs(ctx context.Context, pool *pgxpool.Pool, env func(string) string, log *slog.Logger) (*reportJobSet, error) {
	bucket := env("AFS_REPORTS_BUCKET")
	if bucket == "" {
		return nil, errors.New("afs-billing: AFS_REPORTS_BUCKET is not set")
	}
	as, err := awsx.SettingsFrom(env)
	if err != nil {
		return nil, err
	}
	cfg, err := awsx.Load(ctx, as)
	if err != nil {
		return nil, err
	}
	objs, err := awss3.New(cfg, awss3.Options{Bucket: bucket, KMSKeyID: env("AFS_REPORTS_KMS_KEY_ID"), PathStyle: as.Endpoint != ""})
	if err != nil {
		return nil, err
	}
	return newReportJobsWith(postgres.NewTxRunner(pool), objs, env("AFS_SHOWBACK_TAG_KEY"), log), nil
}

// newReportJobsWith builds the jobs on a transaction runner and an object store (tests give fakes).
func newReportJobsWith(tx port.TxRunner, objs port.ObjectStore, tagKey string, log *slog.Logger) *reportJobSet {
	clock := reportClock{}
	repo := postgres.NewReportSchedulesRepo()
	return &reportJobSet{
		Schedules: &schedules.Runner{Tx: tx, Store: repo, Reports: postgres.NewReportsRepo(), Objects: objs, Outbox: postgres.NewOutboxWriter(),
			Clock: clock, IDs: reportIDs{}, Render: httpapi.ReportCSV, Log: log},
		Showback:       &showback.Generator{Tx: tx, Source: postgres.NewShowbackSource(), Objects: objs, Clock: clock},
		ShowbackTagKey: tagKey, Clock: clock, Log: log,
	}
}

// RunSchedules runs every due report schedule once (job report-schedules); run it every few minutes.
func (j *reportJobSet) RunSchedules(ctx context.Context) error {
	sum, err := j.Schedules.RunDue(ctx)
	j.Log.Info("report schedules run", "due", sum.Due, "delivered", sum.Delivered, "failed", sum.Failed, "skipped", sum.Skipped)
	return err
}

// RunShowback writes the showback of the previous month and the current month to date (job showback);
// run it daily. Writing a month again replaces its file, so the job is idempotent.
func (j *reportJobSet) RunShowback(ctx context.Context) error {
	now := j.Clock.Now().UTC()
	this := showback.MonthOf(now)
	var errs []error
	for _, month := range []time.Time{this.AddDate(0, -1, 0), this} {
		res, err := j.Showback.Run(ctx, month, j.ShowbackTagKey)
		if err != nil {
			errs = append(errs, fmt.Errorf("showback %s: %w", month.Format("2006-01"), err))
			continue
		}
		j.Log.Info("showback written", "month", month.Format("2006-01"), "key", res.Key)
	}
	return errors.Join(errs...)
}

type reportClock struct{}

func (reportClock) Now() time.Time { return time.Now() }

type reportIDs struct{}

func (reportIDs) NewID(p domain.IDPrefix) (domain.ID, error) {
	return domain.NewID(p, time.Now(), crand.Reader)
}

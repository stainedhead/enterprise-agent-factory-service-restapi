package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/schedules"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/schedules/schedulestest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/showback"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/showback/showbacktest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

func TestNewReportJobsNeedsABucketAndARegion(t *testing.T) {
	env := func(k string) string { return map[string]string{"AFS_AWS_REGION": "us-east-1"}[k] }
	if _, err := newReportJobs(context.Background(), nil, env, slog.Default()); err == nil || !strings.Contains(err.Error(), "AFS_REPORTS_BUCKET") {
		t.Fatalf("err = %v", err)
	}
	env = func(k string) string { return map[string]string{"AFS_REPORTS_BUCKET": "example-afs-reports"}[k] }
	if _, err := newReportJobs(context.Background(), nil, env, slog.Default()); err == nil || !strings.Contains(err.Error(), "AFS_AWS_REGION") {
		t.Fatalf("a missing region must be reported: %v", err)
	}
}

func fakeJobs(t *testing.T, now time.Time) (*reportJobSet, *fakes.ObjectStore, *schedulestest.Store) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clock := &fakes.Clock{T: now}
	objs := &fakes.ObjectStore{Clock: clock}
	tx := &fakes.TxRunner{}
	src := &showbacktest.Source{Tags: map[string][]showback.Line{}}
	store := schedulestest.NewStore()
	return &reportJobSet{
		Schedules: &schedules.Runner{Tx: tx, Store: store, Reports: &schedulestest.Reports{}, Objects: objs, Outbox: &fakes.Outbox{}, Clock: clock,
			IDs: &fakes.IDGen{}, Render: func(domain.Report) ([]byte, error) { return []byte("x\n"), nil }, Log: log},
		Showback:       &showback.Generator{Tx: tx, Source: src, Objects: objs, Clock: clock},
		ShowbackTagKey: "", Clock: clock, Log: log,
	}, objs, store
}

func TestRunShowbackWritesLastMonthAndThisMonthToDate(t *testing.T) {
	jobs, objs, _ := fakeJobs(t, time.Date(2027, 1, 9, 3, 0, 0, 0, time.UTC))
	if err := jobs.RunShowback(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"showback/2026-12/showback-2026-12.csv", "showback/2027-01/showback-2027-01.csv"} {
		if _, err := objs.Body(key); err != nil {
			t.Errorf("%s: %v (have %v)", key, err, objs.Objects)
		}
	}
	// A second run replaces the files, it does not add any.
	if err := jobs.RunShowback(t.Context()); err != nil || len(objs.Objects) != 2 {
		t.Errorf("rerun: %v, %d objects", err, len(objs.Objects))
	}
	boom := errors.New("store down")
	objs.PutErr = boom
	if err := jobs.RunShowback(t.Context()); !errors.Is(err, boom) || !strings.Contains(err.Error(), "2026-12") || !strings.Contains(err.Error(), "2027-01") {
		t.Errorf("both months must be tried and reported: %v", err)
	}
}

func TestRunSchedulesReportsACancelledCycle(t *testing.T) {
	jobs, _, _ := fakeJobs(t, time.Date(2027, 1, 9, 3, 0, 0, 0, time.UTC))
	if err := jobs.RunSchedules(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := jobs.RunSchedules(ctx); err == nil {
		t.Error("a cancelled cycle reported no error")
	}
}

func TestNewReportJobsWithBuildsTheJobs(t *testing.T) {
	j := newReportJobsWith(&fakes.TxRunner{}, &fakes.ObjectStore{}, "business-unit", slog.Default())
	if j.Schedules == nil || j.Showback == nil || j.ShowbackTagKey != "business-unit" || j.Schedules.Render == nil {
		t.Errorf("%+v", j)
	}
	if id, err := (reportIDs{}).NewID(domain.PrefixReportSchedule); err != nil || !strings.HasPrefix(id.String(), "rsc_") {
		t.Errorf("id %v %v", id, err)
	}
	if time.Since((reportClock{}).Now()) > time.Minute {
		t.Error("clock")
	}
}

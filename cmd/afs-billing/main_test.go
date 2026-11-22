package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur/curtest"
)

func noEnv(string) string { return "" }

func TestRunUsage(t *testing.T) {
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format(time.DateOnly)
	for _, tc := range []struct {
		name string
		args []string
		code int
		out  string
	}{
		{"version", []string{"--version"}, 0, "afs-billing"},
		{"no job", nil, exitUsage, "--job is required"},
		{"unknown job", []string{"--job", "nope"}, exitUsage, `unknown job "nope"`},
		{"names the jobs", []string{"--job", "nope"}, exitUsage, "cur-ingest"},
		{"bad flag", []string{"--bogus"}, exitUsage, "flag provided but not defined"},
		{"bad day", []string{"--job", "reconcile", "--day", "yesterday"}, exitUsage, "not a date"},
		{"day not complete", []string{"--job", "reconcile", "--day", time.Now().UTC().Format(time.DateOnly)}, exitUsage, "not complete yet"},
		// A valid job with no configuration stops at settings, before any connection.
		{"missing database url", []string{"--job", "reconcile", "--day", yesterday}, exitFailed, "AFS_DATABASE_URL is not set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			got := run(t.Context(), tc.args, noEnv, &out, &errb)
			if got != tc.code || !strings.Contains(out.String()+errb.String(), tc.out) {
				t.Fatalf("code=%d out=%q err=%q", got, out.String(), errb.String())
			}
		})
	}
}

func TestEveryHelmJobOfThisStreamIsRegistered(t *testing.T) {
	for _, name := range []string{"cur-ingest", "reconcile", "price-sync"} {
		if _, ok := jobs[name]; !ok {
			t.Errorf("job %s is not registered (deploy/helm/afs/values/billing.yaml)", name)
		}
	}
}

func TestRegisterTwicePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want a panic")
		}
	}()
	register("reconcile", reconcileJob)
}

func TestSettings(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := settingsFrom(noEnv); err == nil || !strings.Contains(err.Error(), "AFS_DATABASE_URL") {
		t.Fatalf("err = %v", err)
	}
	base := map[string]string{"AFS_DATABASE_URL": "postgres://u@h/db"}
	s, err := settingsFrom(env(base))
	if err != nil || s.Lookback != 3 || s.PricingRegion != "us-east-1" || s.DBIAMAuth || s.PriceListSync != nil {
		t.Fatalf("defaults = %+v err=%v", s, err)
	}
	if _, err := s.athenaConfig(); err == nil || !strings.Contains(err.Error(), "AFS_CUR_DATABASE") {
		t.Fatalf("athena config without CUR settings: %v", err)
	}
	full := map[string]string{"AFS_DATABASE_URL": "x", "AFS_CUR_DATABASE": "d", "AFS_CUR_TABLE": "t", "AFS_ATHENA_WORKGROUP": "w",
		"AFS_ATHENA_OUTPUT": "s3://b/p/", "AFS_BILLING_LOOKBACK_DAYS": "7", "AFS_DB_IAM_AUTH": "true", "AFS_PRICING_PRICE_LIST_SYNC": "false"}
	s, err = settingsFrom(env(full))
	if err != nil || s.Lookback != 7 || !s.DBIAMAuth || s.PriceListSync == nil || *s.PriceListSync {
		t.Fatalf("settings = %+v err=%v", s, err)
	}
	if c, err := s.athenaConfig(); err != nil || c.Database != "d" || c.OutputLocation != "s3://b/p/" {
		t.Fatalf("athena config = %+v err=%v", c, err)
	}
	for k, v := range map[string]string{"AFS_BILLING_LOOKBACK_DAYS": "0", "AFS_DB_IAM_AUTH": "maybe", "AFS_PRICING_PRICE_LIST_SYNC": "x"} {
		bad := map[string]string{"AFS_DATABASE_URL": "x", k: v}
		if _, err := settingsFrom(env(bad)); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
	}
}

func testApp(buf io.Writer) *app {
	return &app{
		log:      slog.New(slog.NewJSONHandler(buf, nil)),
		metrics:  observability.NewMetrics("afs-billing", "test"),
		settings: settings{Lookback: 3},
		now:      func() time.Time { return time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC) },
	}
}

func TestExecute(t *testing.T) {
	var buf bytes.Buffer
	a := testApp(&buf)
	var got jobInput
	jobs["test-ok"] = func(_ context.Context, _ *app, in jobInput) error { got = in; return nil }
	jobs["test-fail"] = func(context.Context, *app, jobInput) error { return errors.New("boom") }
	t.Cleanup(func() { delete(jobs, "test-ok"); delete(jobs, "test-fail") })

	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	if err := execute(t.Context(), a, "test-ok", jobInput{Day: &day}); err != nil || got.Day == nil {
		t.Fatalf("err=%v in=%+v", err, got)
	}
	if err := execute(t.Context(), a, "test-fail", jobInput{}); err == nil || !strings.Contains(buf.String(), "job failed") {
		t.Fatalf("err=%v log=%s", err, buf.String())
	}
	if err := execute(t.Context(), a, "nope", jobInput{}); err == nil {
		t.Fatal("want an error")
	}
}

func TestDays(t *testing.T) {
	a := testApp(io.Discard)
	if d := a.days(jobInput{}); len(d) != 3 || d[2].Format(time.DateOnly) != "2026-11-21" {
		t.Fatalf("lookback days = %v", d)
	}
	one := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	if d := a.days(jobInput{Day: &one}); len(d) != 1 || d[0].Format(time.DateTime) != "2026-11-10 00:00:00" {
		t.Fatalf("--day = %v", d)
	}
}

func TestIngestDaysFailsWhenADayFails(t *testing.T) {
	var buf bytes.Buffer
	a := testApp(&buf)
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	arn := "arn:aws:bedrock:us-east-1:111122223333:application-inference-profile/a"
	line := cur.Line{Kind: cur.KindBedrock, Resource: arn, Cost: domain.MustMoney("1")}
	f := &curtest.Fetcher{Lines: map[string][]cur.Line{day.Format(time.DateOnly): {line}}}
	s := &curtest.Store{Dir: cur.Directory{Teams: map[string]bool{"team_1": true}, Profiles: map[string]cur.ProfileRef{arn: {TeamID: "team_1", ModelID: "m"}}}}
	ing := &cur.Ingestor{Fetch: f, Store: s, Log: a.log}
	if err := ingestDays(t.Context(), a, ing, []time.Time{day}); err != nil {
		t.Fatal(err)
	}
	s.ReplaceErr = errors.New("db down")
	if err := ingestDays(t.Context(), a, ing, []time.Time{day}); err == nil || !strings.Contains(err.Error(), "1 of 1 days failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestCURIngestNeedsItsSettings(t *testing.T) {
	a := testApp(io.Discard)
	if err := curIngest(t.Context(), a, jobInput{}); err == nil || !strings.Contains(err.Error(), "AFS_CUR_DATABASE") {
		t.Fatalf("err = %v", err)
	}
}

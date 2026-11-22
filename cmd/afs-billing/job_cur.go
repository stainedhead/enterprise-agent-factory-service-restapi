package main

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/athena"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/cur"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/reconcile"
)

func init() {
	register("cur-ingest", curIngest)
	register("reconcile", reconcileJob)
}

// curIngest reads the CUR days through Athena into cost_daily (M4-C2, M4-C3).
func curIngest(ctx context.Context, a *app, in jobInput) error {
	cfg, err := a.settings.athenaConfig()
	if err != nil {
		return err
	}
	awsCfg, err := a.aws(ctx)
	if err != nil {
		return err
	}
	src, err := athena.New(awsCfg, cfg)
	if err != nil {
		return err
	}
	alerts := prometheus.NewCounter(prometheus.CounterOpts{Name: "afs_billing_cur_ingest_alerts_total", Help: "Runs of cur-ingest that failed for three or more days."})
	_ = a.metrics.Registry.Register(alerts)
	ing := &cur.Ingestor{Fetch: src, Store: a.store, Log: a.log, Retry: cur.DefaultBackoff, OnAlert: func(cur.Alert) { alerts.Inc() }}
	return ingestDays(ctx, a, ing, a.days(in))
}

// ingestDays runs the ingestor and logs each day; the run fails when any day failed.
func ingestDays(ctx context.Context, a *app, ing *cur.Ingestor, days []time.Time) error {
	res := ing.Run(ctx, days)
	for _, d := range res.Days {
		a.log.Info("CUR day ingested", "day", d.Day.Format(time.DateOnly), "rows", d.Rows, "costUsd", d.Cost.String(),
			"unattributedUsd", d.Unattributed.String(), "noData", d.NoData, "failed", d.Err != nil)
	}
	if err := res.Err(); err != nil {
		return fmt.Errorf("%d of %d days failed: %w", res.Failed, len(res.Days), err)
	}
	return nil
}

// reconcileJob compares the gateway estimate with the billed Bedrock cost per team and day (M4-C4).
func reconcileJob(ctx context.Context, a *app, in jobInput) error {
	r := &reconcile.Reconciler{
		Tx: a.runner, Outbox: postgres.NewOutboxWriter(), Clock: realClock{}, IDs: ulidGen{}, Repo: a.store,
		Log: a.log, Session: postgres.BillingSession(),
		OnVariance: func(ratio float64) { a.metrics.CURVariance.Set(ratio) },
	}
	results, err := r.Run(ctx, a.days(in))
	for _, d := range results {
		a.log.Info("reconciled day", "day", d.Day.Format(time.DateOnly), "skipped", d.Skipped, "teams", d.Teams,
			"overThreshold", d.OverThreshold, "events", d.Events, "maxVarianceRatio", d.MaxVarianceRatio)
	}
	return err
}

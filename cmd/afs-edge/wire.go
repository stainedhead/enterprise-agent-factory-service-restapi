package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/secretsmanager"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/edgedb"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/edgehttp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/edgelog"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/edgesession"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/oidc"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// edgeApp is everything afs-edge is built from.
type edgeApp struct {
	Handler http.Handler
	Service *edge.Service
	Checks  []observability.Check
	// Run keeps the route table fresh and flushes access counts until ctx ends.
	Run func(ctx context.Context)
	// Feed listens for database notifications until ctx ends.
	Feed  func(ctx context.Context) error
	Close func()
}

// secretReader adapts the Secrets Manager reader to the OIDC client's port.
type secretReader struct{ r *secretsmanager.Reader }

func (s secretReader) Secret(ctx context.Context, arn string) (string, error) {
	c, err := s.r.Read(ctx, arn)
	return string(c), err
}

// metricsObserver reports what the edge measures to Prometheus.
type metricsObserver struct{ m *observability.Metrics }

func (o metricsObserver) Request(c collab.Class, status int, d time.Duration) {
	surface := string(c.Surface())
	o.m.EdgeRequests.WithLabelValues(surface, fmt.Sprint(status)).Inc()
	o.m.EdgeRequestSecs.WithLabelValues(surface).Observe(d.Seconds())
}

// wire builds afs-edge. It is the only place adapters are connected.
func wire(ctx context.Context, log *slog.Logger, s settings, env func(string) string, m *observability.Metrics) (*edgeApp, error) {
	as, err := aws.SettingsFrom(env)
	if err != nil {
		return nil, err
	}
	awsCfg, err := aws.Load(ctx, as)
	if err != nil {
		return nil, err
	}
	dbCfg := postgres.Config{URL: s.DatabaseURL, Service: "afs-edge", SystemActor: true, StatementTimeout: 5 * time.Second, MaxConns: 10}
	if s.DBIAMAuth {
		if dbCfg.Password, err = postgres.IAMPassword(s.DatabaseURL, as.Region, awsCfg.Credentials); err != nil {
			return nil, err
		}
	}
	pool, err := postgres.OpenPool(ctx, dbCfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	fail := func(err error) (*edgeApp, error) { pool.Close(); return nil, err }
	if _, _, err := postgres.CheckSchema(ctx, pool); err != nil {
		return fail(err)
	}
	sealer, err := edgesession.New(s.StateKeys...)
	if err != nil {
		return fail(err)
	}
	db := edgedb.New(pool)
	table := collab.NewTable(nil)
	svc := &edge.Service{
		Table: table, Routes: db, Principals: db, Sessions: db, Sink: db, Alerts: db,
		Identity: oidc.NewCodeFlow(oidc.NewVerifier(oidc.Options{}), secretReader{secretsmanager.NewReader(awsCfg)}),
		Sealer:   sealer, Access: collab.NewAccessBuffer(), Records: edgelog.New(os.Stdout, nil),
		Limiter: collab.NewLimiter(20, 40, time.Now), Rand: rand.Reader,
	}
	changes := make(chan struct{}, 1)
	feed := postgres.NewChangeFeed(dbCfg, log)
	want, err := postgres.ExpectedSchemaVersion()
	if err != nil {
		return fail(err)
	}
	app := &edgeApp{
		Service: svc,
		Handler: edgehttp.New(edgehttp.Config{Service: svc, IdleTimeout: s.IdleTimeout, Log: log, Observer: metricsObserver{m}}).Handler(),
		Close:   pool.Close,
		Run: func(ctx context.Context) {
			sweep := time.NewTicker(time.Minute)
			defer sweep.Stop()
			go func() {
				var last int64
				for {
					select {
					case <-ctx.Done():
						return
					case <-sweep.C:
						svc.Limiter.Sweep(10 * time.Minute)
						d := svc.Access.Dropped()
						m.EdgeAccessDropped.Add(float64(d - last))
						last = d
					}
				}
			}()
			svc.Run(ctx, edge.RunConfig{RefreshEvery: s.RefreshInterval, FlushEvery: s.FlushInterval, Changes: changes,
				OnError: func(op string, err error) {
					if op == "refresh" {
						m.EdgeRefreshFailed.Inc()
					}
					log.Warn("edge "+op+" failed", "err", err)
				}})
		},
		Feed: func(ctx context.Context) error {
			return feed.Listen(ctx, engine.FeedHandler{
				Change:    func(engine.Change) { notify(changes) },
				Connected: func() { notify(changes) },
			})
		},
		Checks: []observability.Check{
			{Name: "database", Fn: func(ctx context.Context) error {
				if err := pool.Ping(ctx); err != nil {
					return errors.New("database: not reachable") // never the driver's text
				}
				return nil
			}},
			observability.SchemaCheck(schemaVersion(pool), want),
			{Name: "routes", Fn: func(context.Context) error {
				if !table.Fresh() {
					return errors.New("routes: not loaded or stale")
				}
				return nil
			}},
		},
	}
	return app, nil
}

// notify wakes the refresh loop without blocking; a burst merges into one refresh.
func notify(c chan<- struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func schemaVersion(pool *pgxpool.Pool) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		var v int
		return v, pool.QueryRow(ctx, `SELECT afs.schema_version()`).Scan(&v)
	}
}

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/sts"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/gatewayhttp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/podcache"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/tokenreview"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

type ulidGen struct{}

func (ulidGen) NewID(p domain.IDPrefix) (domain.ID, error) {
	return domain.NewID(p, time.Now(), rand.Reader)
}

// gatewayApp is everything afs-gateway is built from.
type gatewayApp struct {
	Handler *gatewayhttp.Server
	Service *gateway.Service
	// Background run until ctx ends: the pod cache and the change feed.
	Background []func(ctx context.Context) error
	Usage      *postgres.UsageWriter
	Checks     []observability.Check
	Close      func()
}

// metricsObserver reports what the gateway measures to Prometheus.
type metricsObserver struct{ m *observability.Metrics }

func (o metricsObserver) Done(_ string, _ int, added time.Duration) {
	o.m.GatewayAddedLatency.Observe(added.Seconds())
}

func (o metricsObserver) ReserveFailed(reason string) {
	o.m.GatewayReservationFailures.WithLabelValues(reason).Inc()
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// bedrockEndpoint is the configured VPC endpoint, else the regional Bedrock Runtime endpoint.
func bedrockEndpoint(s settings, region string) string {
	if s.BedrockURL != "" {
		return s.BedrockURL
	}
	return "https://bedrock-runtime." + region + ".amazonaws.com"
}

// wire builds afs-gateway. It is the only place adapters are connected.
func wire(ctx context.Context, log *slog.Logger, s settings, env func(string) string, m *observability.Metrics) (*gatewayApp, error) {
	as, err := aws.SettingsFrom(env)
	if err != nil {
		return nil, err
	}
	account := aws.AccountOfARN(s.RoleARN)
	if account == "" {
		return nil, errors.New("AFS_GATEWAY_ROLE_ARN is not an IAM role ARN")
	}
	awsCfg, err := aws.Load(ctx, as)
	if err != nil {
		return nil, err
	}
	dbCfg := postgres.Config{URL: s.DatabaseURL, Service: "afs-gateway", SystemActor: true, StatementTimeout: 10 * time.Second}
	if s.DBIAMAuth {
		if dbCfg.Password, err = postgres.IAMPassword(s.DatabaseURL, as.Region, awsCfg.Credentials); err != nil {
			return nil, err
		}
	}
	pool, err := postgres.OpenPool(ctx, dbCfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	app := &gatewayApp{Close: pool.Close}
	fail := func(err error) (*gatewayApp, error) { pool.Close(); return nil, err }
	if _, _, err := postgres.CheckSchema(ctx, pool); err != nil {
		return fail(err)
	}

	rc, err := restConfig(s.Kubeconfig)
	if err != nil {
		return fail(fmt.Errorf("kubernetes config: %w", err))
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return fail(err)
	}
	pc := podcache.Config{Client: cs, Log: log}
	if s.CiliumCheck {
		if pc.Dynamic, err = dynamic.NewForConfig(rc); err != nil {
			return fail(err)
		}
	}
	pods, err := podcache.New(pc)
	if err != nil {
		return fail(err)
	}

	rt, err := bedrock.NewRuntime(bedrock.RuntimeConfig{Endpoint: bedrockEndpoint(s, as.Region), Region: as.Region, Credentials: sts.NewAssumer(awsCfg)})
	if err != nil {
		return fail(err)
	}

	store := gateway.NewCachedStore(postgres.NewGatewayStore(pool), s.CacheTTL, time.Now)
	runner := postgres.NewTxRunner(pool)
	ctr := newGatewayCounters(m)
	app.Usage = postgres.NewUsageWriter(pool, log, postgres.UsageWriterConfig{Recorder: usageDropRecorder{ctr.UsageRowsDropped}})
	svc := gateway.NewService(gateway.Deps{
		Pods: pods, Tokens: tokenreview.New(cs, time.Now), Store: store, Spend: postgres.NewSpendStore(pool),
		Thresholds: postgres.NewThresholdStore(runner, ulidGen{}, realClock{}), Usage: app.Usage,
		SettleRecorder: settleRecorder{ctr.SettleFailures},
	}, gateway.Config{Partition: as.Partition, AccountID: account, AsyncSettle: true, Now: time.Now, Log: log})
	app.Service = svc
	app.Handler = gatewayhttp.New(gatewayhttp.Config{Service: svc, Upstream: rt, MaxBody: s.MaxBody, Log: log, Observer: metricsObserver{m}})

	// A change to prices, models, budgets, teams or tags is seen within a second, not a TTL later; the TTL
	// is the fallback when a notification is lost.
	feed := postgres.NewChangeFeed(dbCfg, log)
	app.Background = []func(context.Context) error{
		pods.Run,
		func(ctx context.Context) error {
			return feed.Listen(ctx, engine.FeedHandler{
				Change:    func(c engine.Change) { store.Invalidate(c.Table, c.TeamID) },
				Connected: store.InvalidateAll,
			})
		},
	}
	want, err := postgres.ExpectedSchemaVersion()
	if err != nil {
		return fail(err)
	}
	app.Checks = []observability.Check{
		{Name: "database", Fn: func(ctx context.Context) error {
			if err := pool.Ping(ctx); err != nil {
				return errors.New("database: not reachable") // never the driver's text
			}
			return nil
		}},
		observability.SchemaCheck(schemaVersion(pool), want),
		{Name: "podcache", Fn: func(context.Context) error {
			if !pods.Ready() {
				return errors.New("pod cache: initial sync not finished")
			}
			return nil
		}},
	}
	return app, nil
}

func schemaVersion(pool *pgxpool.Pool) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) {
		var v int
		return v, pool.QueryRow(ctx, `SELECT afs.schema_version()`).Scan(&v)
	}
}

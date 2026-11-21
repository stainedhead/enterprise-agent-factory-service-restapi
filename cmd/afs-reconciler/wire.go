package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/bedrock"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/iam"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/admission"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// realClock and ulidGen are the production port implementations; they live here because cmd is the
// composition root.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

type ulidGen struct{}

func (ulidGen) NewID(p domain.IDPrefix) (domain.ID, error) {
	return domain.NewID(p, time.Now(), rand.Reader)
}

// reconciler is everything afs-reconciler is built from.
type reconciler struct {
	Pool    *pgxpool.Pool
	Engine  *engine.Engine
	Watcher *k8s.Watcher
	Elector *k8s.Elector
	Close   func()
}

// routes is the table → controller registry (architecture §3): a row of teams is the team itself, a
// row of team_models belongs to its team, and a service-level model change reconciles every team.
func routes() (*engine.Registry, error) {
	return engine.NewRegistry(
		engine.Route{Table: "teams", Targets: []engine.Target{{Kind: engine.KindTeam, Self: true}}},
		engine.Route{Table: "team_models", Targets: []engine.Target{{Kind: engine.KindTeam}}},
		engine.Route{Table: "models", FanOut: []engine.Kind{engine.KindTeam}},
	)
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// checkSchema refuses to run on a schema older than this build (database contract §1). It prefers
// afs.schema_version() and falls back to reading the tracking table on databases without it.
func checkSchema(ctx context.Context, pool *pgxpool.Pool) error {
	want, err := postgres.ExpectedSchemaVersion()
	if err != nil {
		return err
	}
	var have int
	if err := pool.QueryRow(ctx, `SELECT afs.schema_version()`).Scan(&have); err != nil {
		h, w, cerr := postgres.CheckSchema(ctx, pool)
		if cerr != nil {
			return cerr
		}
		have, want = h, w
	}
	if have < want {
		return fmt.Errorf("%w: have %06d, want %06d", postgres.ErrSchemaTooOld, have, want)
	}
	return nil
}

// wire builds afs-reconciler. It is the only place adapters are connected.
func wire(ctx context.Context, log *slog.Logger, s settings, env func(string) string) (*reconciler, error) {
	r := &reconciler{Close: func() {}}
	dbCfg := postgres.Config{URL: s.DatabaseURL, Service: "afs-reconciler", SystemActor: true, StatementTimeout: 30 * time.Second}
	pool, err := postgres.OpenPool(ctx, dbCfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	r.Pool, r.Close = pool, pool.Close
	fail := func(err error) (*reconciler, error) { pool.Close(); return nil, err }
	if err := checkSchema(ctx, pool); err != nil {
		return fail(err)
	}

	as, err := aws.SettingsFrom(env)
	if err != nil {
		return fail(err)
	}
	awsCfg, err := aws.Load(ctx, as)
	if err != nil {
		return fail(err)
	}
	account := aws.AccountOfARN(s.GatewayRoleARN)
	if account == "" {
		return fail(errors.New("AFS_GATEWAY_ROLE_ARN is not an IAM role ARN"))
	}
	var clusterARN string
	if s.ClusterName != "" {
		clusterARN = fmt.Sprintf("arn:%s:eks:%s:%s:cluster/%s", as.Partition, as.Region, account, s.ClusterName)
	}

	rc, err := restConfig(s.Kubeconfig)
	if err != nil {
		return fail(fmt.Errorf("kubernetes config: %w", err))
	}
	kc, err := k8s.NewClient(rc)
	if err != nil {
		return fail(err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return fail(err)
	}
	r.Elector, err = k8s.NewElector(k8s.ElectorConfig{Client: cs, Namespace: s.LeaseNamespace, Name: "afs-reconciler", Identity: s.PodName, ReleaseOnCancel: true})
	if err != nil {
		return fail(err)
	}

	runner := postgres.NewTxRunner(pool)
	clock, ids := realClock{}, ulidGen{}
	outbox := postgres.NewOutboxWriter()
	audit := postgres.NewAuditWriter(runner)

	gateway := team.PlatformSpec{GatewayNamespace: s.GatewayNamespace, GatewayName: s.GatewayName}
	cluster := k8s.NewTeamCluster(kc, k8s.GatewayCASource{Namespace: s.GatewayNamespace, Name: s.GatewayCAConfigMap})
	teams, err := team.NewController(team.Config{
		Source: postgres.NewTeamSource(runner), Cluster: cluster, IAM: iam.New(awsCfg), Profiles: bedrock.NewControl(awsCfg),
		AWS: team.AWSConfig{Partition: as.Partition, Region: as.Region, AccountID: account, GatewayRoleARN: s.GatewayRoleARN,
			PermissionsBoundaryARN: s.PermissionsBoundaryARN, ClusterARN: clusterARN, ESORoleARN: s.ESORoleARN, SecretsPrefix: s.SecretsPrefix},
		Gateway: gateway, Now: clock.Now,
	})
	if err != nil {
		return fail(err)
	}
	platform, err := team.NewPlatformController(cluster, gateway)
	if err != nil {
		return fail(err)
	}
	adm, err := admission.NewController(k8s.NewAdmissionCluster(kc), admission.Spec{ReconcilerUsers: s.ReconcilerUsers})
	if err != nil {
		return fail(err)
	}
	reg, err := routes()
	if err != nil {
		return fail(err)
	}
	r.Engine, err = engine.New(engine.Config{
		Feed: postgres.NewChangeFeed(dbCfg, log), Lister: postgres.NewLister(pool), Registry: reg,
		Controllers:  []engine.Controller{teams, platform, adm},
		Status:       postgres.NewStatusWriter(runner, outbox, ids, clock),
		Drift:        postgres.NewDriftRecorder(runner, audit, outbox, ids),
		Singletons:   []engine.Ref{team.PlatformRef, admission.Ref},
		PollInterval: s.PollInterval, ResyncInterval: s.ResyncInterval, Now: clock.Now, Log: log,
	})
	if err != nil {
		return fail(err)
	}
	r.Watcher, err = k8s.NewWatcher(kc, func(ref engine.Ref, object string) { r.Engine.NoteDrift(ref, object) }, log, k8s.OwnedKinds()...)
	if err != nil {
		return fail(err)
	}
	return r, nil
}

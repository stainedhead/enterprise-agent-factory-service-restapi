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
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/eks"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/eventbridge"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/iam"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/aws/secretsmanager"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/admission"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/housekeeping"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/lifecycle"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/tags"
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
	Metrics *observability.Metrics
	// Checks are the readiness checks served on /readyz: database, schema version and leadership.
	Checks []observability.Check
	Close  func()
}

// routes is the table → controller registry (architecture §3): a row of teams is the team itself, a
// row of team_models belongs to its team, and a service-level model change reconciles every team.
func routes(extra ...engine.Route) (*engine.Registry, error) {
	base := []engine.Route{
		{Table: "teams", Targets: []engine.Target{{Kind: engine.KindTeam, Self: true}}},
		{Table: "team_models", Targets: []engine.Target{{Kind: engine.KindTeam}}},
		{Table: "models", FanOut: []engine.Kind{engine.KindTeam}},
	}
	// Several controllers route the same tables (teams, agents, ...); every route list goes through the one
	// mergeRoutes so the registry sees each table once.
	return engine.NewRegistry(mergeRoutes(append(append(base, agentRoutes()...), extra...))...)
}

func restConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// checkSchema refuses to run on a schema older than this build (database contract §1).
func checkSchema(ctx context.Context, pool *pgxpool.Pool) error {
	_, _, err := postgres.CheckSchema(ctx, pool)
	return err
}

// wire builds afs-reconciler. It is the only place adapters are connected.
func wire(ctx context.Context, log *slog.Logger, s settings, env func(string) string, m *observability.Metrics) (*reconciler, error) {
	r := &reconciler{Metrics: m, Close: func() {}}
	as, err := aws.SettingsFrom(env)
	if err != nil {
		return nil, err
	}
	awsCfg, err := aws.Load(ctx, as)
	if err != nil {
		return nil, err
	}
	dbCfg := postgres.Config{URL: s.DatabaseURL, Service: "afs-reconciler", SystemActor: true, StatementTimeout: 30 * time.Second}
	if s.DBIAMAuth {
		if dbCfg.Password, err = postgres.IAMPassword(s.DatabaseURL, as.Region, awsCfg.Credentials); err != nil {
			return nil, err
		}
	}
	pool, err := postgres.OpenPool(ctx, dbCfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	r.Pool, r.Close = pool, pool.Close
	fail := func(err error) (*reconciler, error) { pool.Close(); return nil, err }
	if err := checkSchema(ctx, pool); err != nil {
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
	iamClient, profiles := iam.New(awsCfg), bedrock.NewControl(awsCfg)
	status := postgres.NewStatusWriter(runner, outbox, ids, clock)

	// The ordered team delete (M1-I1): secrets scheduled for deletion before the namespace goes, then IAM,
	// inference profiles, the access entry and the namespace. Agent and pool steps (M2, M5) and the
	// suspend scale hooks plug into lifecycle.Hooks; Hooks.Suspended is set once the first ScaleHook and a
	// PriorStateStore exist.
	lc := postgres.NewTeamLifecycle(runner, audit)
	sm := secretsmanager.New(awsCfg)
	cstatus := postgres.NewConditionStore(status, runner)
	agents, err := buildAgentStream(agentStreamDeps{
		Client: kc, Runner: runner, Audit: audit, Status: cstatus, IAM: iamClient,
		AWS: team.AWSConfig{Partition: as.Partition, Region: as.Region, AccountID: account, GatewayRoleARN: s.GatewayRoleARN,
			PermissionsBoundaryARN: s.PermissionsBoundaryARN, ClusterARN: clusterARN, ESORoleARN: s.ESORoleARN, SecretsPrefix: s.SecretsPrefix},
		Gateway: agent.Gateway{Namespace: s.GatewayNamespace, Name: s.GatewayName},
		Cluster: s.ClusterName, AWSCfg: awsCfg, NodeClassJSON: env("AFS_NODECLASS_JSON"), LinkVerifierARN: env("AFS_RECONCILER_ROLE_ARN"),
	}, log)
	if err != nil {
		return fail(err)
	}
	pools, err := buildPoolStream(agentStreamDeps{
		Client: kc, Runner: runner, Audit: audit, Status: cstatus, IAM: iamClient,
		AWS: team.AWSConfig{Partition: as.Partition, Region: as.Region, AccountID: account, GatewayRoleARN: s.GatewayRoleARN,
			PermissionsBoundaryARN: s.PermissionsBoundaryARN, ClusterARN: clusterARN, ESORoleARN: s.ESORoleARN, SecretsPrefix: s.SecretsPrefix},
		Gateway: agent.Gateway{Namespace: s.GatewayNamespace, Name: s.GatewayName},
		Cluster: s.ClusterName, AWSCfg: awsCfg, NodeClassJSON: env("AFS_NODECLASS_JSON"), LinkVerifierARN: env("AFS_RECONCILER_ROLE_ARN"),
	}, env("AFS_EFS_STORAGE_CLASS"))
	if err != nil {
		return fail(err)
	}
	finalizer := lifecycle.NewTeamFinalizer(lifecycle.TeamDeps{
		Secrets: lifecycle.UnionSecrets{Sources: []interface {
			ListTeamSecrets(ctx context.Context, teamID string) ([]string, error)
		}{lc, sm}, Scheduler: sm},
		IAM: iamClient, Profiles: profiles, Access: eks.New(awsCfg, s.ClusterName), NS: cluster,
	}, lifecycle.Hooks{Agents: agents.TeamSteps, Pools: pools.TeamSteps}, cstatus, lc)

	teams, err := team.NewController(team.Config{
		Source: postgres.NewTeamSource(runner), Cluster: cluster, IAM: iamClient, Profiles: profiles,
		Hooks: team.Hooks{Deleting: lifecycle.TeamDeleting(finalizer)},
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
	lg, err := wireLogs(kc, runner, status, pool, env, clock.Now, log)
	if err != nil {
		return fail(err)
	}
	eg, err := wireEgress(kc, runner, pool, clock.Now)
	if err != nil {
		return fail(err)
	}
	nt, err := wireNotify(notifyDeps{AWS: awsCfg, Bus: s.EventBusName, Runner: runner, Status: status, Outbox: outbox, IDs: ids, Pool: pool, Env: env, Now: clock.Now, Log: log})
	if err != nil {
		return fail(err)
	}
	lk, err := wireLinks(awsCfg, runner, pool, audit, outbox, ids, team.AWSConfig{Partition: as.Partition, Region: as.Region, AccountID: account},
		env("AFS_RECONCILER_ROLE_ARN"), clock.Now)
	if err != nil {
		return fail(err)
	}
	kt, err := wireKubectl(kc, awsCfg, s.ClusterName, runner, pool, env, clock.Now)
	if err != nil {
		return fail(err)
	}
	ts, err := wireTagSync(runner, awsCfg, as.Partition, as.Region, account, pool)
	if err != nil {
		return fail(err)
	}
	controllers := []engine.Controller{teams, platform, adm, agents.Controller, pools.Controller, eg.Controller, lk.Controller, kt.Controller, ts.Controller}
	if lg.Controller != nil {
		controllers = append(controllers, lg.Controller)
	}
	if nt.Controller != nil {
		controllers = append(controllers, nt.Controller)
	}
	reg, err := routes(append(append(append(append(lg.Routes, eg.Routes...), nt.Routes...), lk.Routes...), append(append(append(kt.Routes, tags.Routes()...), pools.Routes...), ts.Routes...)...)...)
	if err != nil {
		return fail(err)
	}
	r.Engine, err = engine.New(engine.Config{
		Feed: postgres.NewChangeFeed(dbCfg, log), Lister: ts.WrapLister(kt.WrapLister(lk.WrapLister(nt.WrapLister(lg.WrapLister(eg.WrapLister(postgres.NewLister(pool))))))), Registry: reg,
		Controllers:  controllers,
		Status:       status,
		Drift:        postgres.NewDriftRecorder(runner, audit, outbox, ids),
		Singletons:   []engine.Ref{team.PlatformRef, admission.Ref},
		PollInterval: s.PollInterval, ResyncInterval: s.ResyncInterval, Now: clock.Now, Log: log,
	})
	if err != nil {
		return fail(err)
	}
	r.Engine.Every("agent-snapshots", snapshotEvery, agents.Snapshots.Run)
	nt.Register(r.Engine)
	hk := postgres.NewHousekeeping(runner)
	housekeeping.Register(r.Engine, housekeeping.Jobs(housekeeping.Deps{
		Outbox: hk, Sink: eventbridge.New(awsCfg, s.EventBusName), Partitions: hk, Purger: hk, Recorder: m, Clock: clock.Now, Log: log,
	}))
	wireRollup(r.Engine, hk, m, log)
	registerOrphanSweep(r.Engine, sm, runner, audit, clock.Now, log)
	registerOrgMirror(r.Engine, awsCfg, runner, env, clock.Now, log)
	if err := wireTraffic(r.Engine, runner, outbox, ids, env, clock.Now, log); err != nil {
		return fail(err)
	}
	wireTeardown(r.Engine, runner, audit, outbox, ids, clock.Now, log)
	if err := wireWorkOrders(r.Engine, awsCfg, runner, audit, outbox, ids, env, clock.Now, log); err != nil {
		return fail(err)
	}
	wireBudget(r.Engine, runner, audit, outbox, ids, iamClient, m, clock.Now, log)
	if err := wireActivity(r.Engine, kc, runner, env, clock.Now, log); err != nil {
		return fail(err)
	}
	blob, err := exportBlob(awsCfg, env, as.Endpoint != "")
	if err != nil {
		return fail(err)
	}
	wireExport(r.Engine, runner, audit, outbox, ids, blob, clock.Now, log)
	registerEngineStats(m, r.Engine.Stats)
	schemaVersion := func(ctx context.Context) (int, error) {
		var v int
		return v, pool.QueryRow(ctx, `SELECT afs.schema_version()`).Scan(&v)
	}
	want, err := postgres.ExpectedSchemaVersion()
	if err != nil {
		return fail(err)
	}
	r.Checks = []observability.Check{
		{Name: "database", Fn: func(ctx context.Context) error {
			if err := pool.Ping(ctx); err != nil {
				return errors.New("database: not reachable") // never the driver's text
			}
			return nil
		}},
		observability.SchemaCheck(schemaVersion, want),
		observability.LeaderCheck(r.Elector.IsLeader),
	}
	r.Watcher, err = k8s.NewWatcher(kc, func(ref engine.Ref, object string) { r.Engine.NoteDrift(ref, object) }, log, driftKinds(kt)...)
	if err != nil {
		return fail(err)
	}
	return r, nil
}

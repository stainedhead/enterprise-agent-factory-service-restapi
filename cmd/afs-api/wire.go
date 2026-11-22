package main

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/middleware"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/buildinfo"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// components is everything afs-api is built from. wire is the only place adapters are connected.
type components struct {
	Pool   *pgxpool.Pool
	Runner *postgres.TxRunner
	Audit  *postgres.AuditWriter
	Outbox *postgres.OutboxWriter
	Idem   *postgres.IdempotencyStore
	Creds  *postgres.Credentials
	// Settings reads service settings (CORS origins, ...).
	Settings *postgres.SettingsReader
	// Auth authenticates bearer credentials (wireAuth).
	Auth    *auth.Service
	Handler http.Handler
	// Metrics is the registry the operations server exposes; Checks are its readiness checks
	// (database and schema version) on /readyz of AFS_METRICS_ADDR.
	Metrics *observability.Metrics
	Checks  []observability.Check
	Close   func()
}

// minKeyBytes is the shortest accepted HMAC or page-token key.
const minKeyBytes = 32

// settings are read from the environment (user-docs/configuration.md).
type settings struct {
	DatabaseURL      string // AFS_DATABASE_URL: the afs_app login
	OwnerURL         string // AFS_DATABASE_OWNER_URL: the afs_owner login, migrations only
	MigrationTimeout time.Duration
	// APIReplicas divides the rate limits among replicas (AFS_API_REPLICAS, default 1).
	APIReplicas int
	// RatePerSecond and RateBurst override the per-principal limit (tests only; zero keeps the contract default).
	RatePerSecond, RateBurst float64
	// TrustedProxyHops is how many reverse proxies in front of afs-api append to X-Forwarded-For
	// (AFS_TRUSTED_PROXY_HOPS, default 0: only the socket peer is trusted as the client address).
	TrustedProxyHops int
	// DBIAMAuth signs an RDS IAM token as the database password of every new connection
	// (AFS_DB_IAM_AUTH); AWSRegion (AFS_AWS_REGION, AWS_REGION as fallback) is then required.
	DBIAMAuth bool
	AWSRegion string
	// PageTokenKey signs nextToken values (AFS_PAGE_TOKEN_KEY, at least 32 bytes). Every replica must
	// hold the same key, so more than one replica refuses to start without it. Empty with one replica:
	// wire draws a random per-process key.
	PageTokenKey []byte
	// IdempotencyHMACKey keys the HMAC of secret values in the idempotency fingerprint
	// (AFS_IDEMPOTENCY_HMAC_KEY, at least 32 bytes, the same on every replica). Empty with one
	// replica: wire draws a random per-process key.
	IdempotencyHMACKey []byte
	// HTTPReadTimeout bounds reading a whole request, body included (AFS_HTTP_READ_TIMEOUT, default
	// 60s); HTTPIdleTimeout bounds an idle keep-alive connection (AFS_HTTP_IDLE_TIMEOUT, default 120s).
	// CreateImport extends its own read deadline.
	HTTPReadTimeout, HTTPIdleTimeout time.Duration
	// OIDCHTTP fetches identity provider discovery documents and JWKS; nil uses a client with a 5 s
	// timeout. Tests point it at a fake provider.
	OIDCHTTP *http.Client
	// ClusterOIDCIssuer is the EKS cluster's service account issuer URL (AFS_CLUSTER_OIDC_ISSUER):
	// workload tokens (RA-2b) are validated against its keys. Empty: workload tokens are refused.
	ClusterOIDCIssuer string
}

func settingsFrom(env func(string) string) (settings, error) {
	s := settings{DatabaseURL: env("AFS_DATABASE_URL"), OwnerURL: env("AFS_DATABASE_OWNER_URL"), MigrationTimeout: 10 * time.Second}
	if v := env("AFS_MIGRATION_LOCK_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return s, fmt.Errorf("AFS_MIGRATION_LOCK_TIMEOUT: %q is not a duration", v)
		}
		s.MigrationTimeout = d
	}
	s.HTTPReadTimeout, s.HTTPIdleTimeout = 60*time.Second, 120*time.Second
	for _, v := range []struct {
		name string
		dst  *time.Duration
	}{{"AFS_HTTP_READ_TIMEOUT", &s.HTTPReadTimeout}, {"AFS_HTTP_IDLE_TIMEOUT", &s.HTTPIdleTimeout}} {
		if raw := env(v.name); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d <= 0 {
				return s, fmt.Errorf("%s: %q is not a positive duration", v.name, raw)
			}
			*v.dst = d
		}
	}
	s.APIReplicas = 1
	for _, v := range []struct {
		name string
		dst  *int
		min  int
	}{{"AFS_API_REPLICAS", &s.APIReplicas, 1}, {"AFS_TRUSTED_PROXY_HOPS", &s.TrustedProxyHops, 0}} {
		if raw := env(v.name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < v.min || n > 1000 {
				return s, fmt.Errorf("%s: %q is not an integer from %d to 1000", v.name, raw, v.min)
			}
			*v.dst = n
		}
	}
	if k := env("AFS_PAGE_TOKEN_KEY"); len(k) >= minKeyBytes {
		s.PageTokenKey = []byte(k)
	}
	if s.APIReplicas > 1 && s.PageTokenKey == nil {
		return s, fmt.Errorf("AFS_API_REPLICAS=%d needs AFS_PAGE_TOKEN_KEY of at least %d bytes, the same on every replica", s.APIReplicas, minKeyBytes)
	}
	if k := env("AFS_IDEMPOTENCY_HMAC_KEY"); len(k) >= minKeyBytes {
		s.IdempotencyHMACKey = []byte(k)
	}
	if s.APIReplicas > 1 && s.IdempotencyHMACKey == nil {
		return s, fmt.Errorf("AFS_API_REPLICAS=%d needs AFS_IDEMPOTENCY_HMAC_KEY of at least %d bytes, the same on every replica", s.APIReplicas, minKeyBytes)
	}
	s.AWSRegion = env("AFS_AWS_REGION")
	if s.AWSRegion == "" {
		s.AWSRegion = env("AWS_REGION")
	}
	if v := env("AFS_DB_IAM_AUTH"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return s, fmt.Errorf("AFS_DB_IAM_AUTH: %q is not a boolean", v)
		}
		s.DBIAMAuth = b
	}
	if s.DBIAMAuth && s.AWSRegion == "" {
		return s, fmt.Errorf("AFS_DB_IAM_AUTH needs AFS_AWS_REGION")
	}
	if s.OwnerURL != "" && s.DatabaseURL == "" {
		return s, fmt.Errorf("AFS_DATABASE_OWNER_URL is set without AFS_DATABASE_URL")
	}
	if err := workloadSettings(env, &s); err != nil {
		return s, err
	}
	return s, nil
}

// wire builds afs-api. Without AFS_DATABASE_URL it serves health, the OpenAPI document and
// placeholders only (development); with it, it migrates (when an owner URL is given), opens the
// pool and wires the database-backed components.
func wire(ctx context.Context, log *slog.Logger, s settings) (*components, error) {
	c := &components{Close: func() {}, Metrics: observability.NewMetrics("afs-api", buildinfo.Version)}
	setPageTokenKey(s.PageTokenKey, log)
	if len(s.IdempotencyHMACKey) < minKeyBytes {
		log.Warn("AFS_IDEMPOTENCY_HMAC_KEY is unset; using a random per-process key (single replica only)")
		s.IdempotencyHMACKey = make([]byte, minKeyBytes)
		_, _ = crand.Read(s.IdempotencyHMACKey)
	}
	cfg := httpapi.Config{
		// RED metrics outermost, labelled by the operationId the router reports.
		Outer:   []httpapi.Middleware{func(next http.Handler) http.Handler { return middleware.Metrics(c.Metrics, next) }},
		OnRoute: middleware.SetRoute,
		// Keyed fingerprint of secret values in idempotency claims (FR-R14).
		IdempotencyHMACKey: s.IdempotencyHMACKey,
	}
	if s.DatabaseURL != "" {
		if s.OwnerURL != "" {
			n, err := postgres.Migrate(ctx, s.OwnerURL, s.MigrationTimeout, nil)
			if err != nil {
				return nil, err
			}
			log.Info("migrations applied", "count", n)
		}
		dbCfg := postgres.Config{URL: s.DatabaseURL, Service: "afs-api", StatementTimeout: 30 * time.Second}
		if s.DBIAMAuth {
			// Plain SDK configuration, not adapter/aws: afs-api builds no AWS service client but
			// Secrets Manager, and signs the RDS token locally.
			awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(s.AWSRegion))
			if err != nil {
				return nil, fmt.Errorf("load AWS configuration: %w", err)
			}
			if dbCfg.Password, err = postgres.IAMPassword(s.DatabaseURL, s.AWSRegion, awsCfg.Credentials); err != nil {
				return nil, err
			}
		}
		pool, err := postgres.OpenPool(ctx, dbCfg)
		if err != nil {
			return nil, fmt.Errorf("open database: %w", err)
		}
		c.Pool, c.Close = pool, pool.Close
		c.Runner = postgres.NewTxRunner(pool)
		c.Audit = postgres.NewAuditWriter(c.Runner)
		c.Outbox = postgres.NewOutboxWriter()
		c.Idem = postgres.NewIdempotencyStore()
		c.Creds = postgres.NewCredentials(c.Runner)
		c.Settings = postgres.NewSettingsReader(c.Runner)
		want, err := postgres.ExpectedSchemaVersion()
		if err != nil {
			pool.Close()
			return nil, err
		}
		c.Checks = []observability.Check{
			{Name: "database", Fn: func(ctx context.Context) error {
				if err := pool.Ping(ctx); err != nil {
					return errors.New("database: not reachable") // never the driver's text
				}
				return nil
			}},
			observability.SchemaCheck(func(ctx context.Context) (int, error) {
				var v int
				return v, pool.QueryRow(ctx, `SELECT afs.schema_version()`).Scan(&v)
			}, want),
		}
		cfg.Ready = map[string]httpapi.ReadinessCheck{
			"database": func(r *http.Request) error { return pool.Ping(r.Context()) },
		}
		p, err := wirePipeline(c, &cfg, s, log)
		if err != nil {
			pool.Close()
			return nil, err
		}
		wireAuth(c, &cfg, p, s)
		wireM1W2(c, &cfg, log)
		wireM2W1(c, log)
		wireAgents(c, log)
		if err := wireSecrets(ctx, c, s, log); err != nil {
			pool.Close()
			return nil, err
		}
		wireLogDest(c, log)
		wireBudgets(c)
		wireReports(c, log)
		wireCosts(c, log)
		wirePricing(c, log)
		wireChannels(c, log)
		wireIdentity(c, s, log)
		wireSchedules(c, log)
		wireExports(c, log)
		wireRestore(c)
		wirePools(c, log)
		wireKubeconfig(c, s)
		wireCostTags(c, log)
		wireSecOps(c, log)
		wireWorkload(c, s)
		wireWorkOrders(c, log)
		// An explicit Registrars list replaces the default one, so the areas bound by the streams
		// (wireM1W2 and later) are added next to the sign-in handlers wireAuth registered.
		for _, a := range httpapi.Areas {
			cfg.Registrars = append(cfg.Registrars, a.Register)
		}
	}
	if cfg.Hooks.CrossTeamNotFound == nil {
		cfg.Hooks.CrossTeamNotFound = func(ctx context.Context, op string) {
			log.InfoContext(ctx, "cross-team not found", "metric", "afs_api_cross_team_not_found_total", "op", op)
		}
	}
	counted := cfg.Hooks.CrossTeamNotFound
	cfg.Hooks.CrossTeamNotFound = func(ctx context.Context, op string) {
		c.Metrics.APICrossTeamNotFound.Inc()
		counted(ctx, op)
	}
	h, err := httpapi.New(cfg)
	if err != nil {
		c.Close()
		return nil, err
	}
	c.Handler = h
	return c, nil
}

// sessionOf turns a principal into the database session of database contract §3.
func sessionOf(p authz.Principal, requestID string) port.Session {
	return auth.SessionOf(p, requestID)
}

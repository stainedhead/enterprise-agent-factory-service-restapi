package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
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
	// Tx is the transaction runner use cases are built on. It claims an Idempotency-Key inside the use
	// case's own transaction (wirePipeline); use cases of creating POSTs and :verb actions must not be
	// given Runner directly.
	Tx port.TxRunner
	// Auth authenticates bearer credentials (wireAuth).
	Auth    *auth.Service
	Handler http.Handler
	Close   func()
}

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
	// OIDCHTTP fetches identity provider discovery documents and JWKS; nil uses a client with a 5 s
	// timeout. Tests point it at a fake provider.
	OIDCHTTP *http.Client
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
	if s.OwnerURL != "" && s.DatabaseURL == "" {
		return s, fmt.Errorf("AFS_DATABASE_OWNER_URL is set without AFS_DATABASE_URL")
	}
	return s, nil
}

// wire builds afs-api. Without AFS_DATABASE_URL it serves health, the OpenAPI document and
// placeholders only (development); with it, it migrates (when an owner URL is given), opens the
// pool and wires the database-backed components.
func wire(ctx context.Context, log *slog.Logger, s settings) (*components, error) {
	c := &components{Close: func() {}}
	cfg := httpapi.Config{}
	if s.DatabaseURL != "" {
		if s.OwnerURL != "" {
			n, err := postgres.Migrate(ctx, s.OwnerURL, s.MigrationTimeout, nil)
			if err != nil {
				return nil, err
			}
			log.Info("migrations applied", "count", n)
		}
		pool, err := postgres.OpenPool(ctx, postgres.Config{URL: s.DatabaseURL, Service: "afs-api", StatementTimeout: 30 * time.Second})
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
		cfg.Ready = map[string]httpapi.ReadinessCheck{
			"database": func(r *http.Request) error { return pool.Ping(r.Context()) },
		}
		p, err := wirePipeline(c, &cfg, s, log)
		if err != nil {
			pool.Close()
			return nil, err
		}
		wireAuth(c, &cfg, p, s)
	}
	if cfg.Hooks.CrossTeamNotFound == nil {
		cfg.Hooks.CrossTeamNotFound = func(ctx context.Context, op string) {
			log.InfoContext(ctx, "cross-team not found", "metric", "afs_api_cross_team_not_found_total", "op", op)
		}
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

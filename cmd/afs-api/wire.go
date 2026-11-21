package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// components is everything afs-api is built from. wire is the only place adapters are connected.
type components struct {
	Pool    *pgxpool.Pool
	Runner  *postgres.TxRunner
	Audit   *postgres.AuditWriter
	Outbox  *postgres.OutboxWriter
	Idem    *postgres.IdempotencyStore
	Creds   *postgres.Credentials
	Handler http.Handler
	Close   func()
}

// settings are read from the environment (user-docs/configuration.md).
type settings struct {
	DatabaseURL      string // AFS_DATABASE_URL: the afs_app login
	OwnerURL         string // AFS_DATABASE_OWNER_URL: the afs_owner login, migrations only
	MigrationTimeout time.Duration
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
		cfg.Ready = map[string]httpapi.ReadinessCheck{
			"database": func(r *http.Request) error { return pool.Ping(r.Context()) },
		}
		cfg.Hooks.Denied = func(ctx context.Context, d httpapi.Denial) {
			s := sessionOf(d.Principal, httpapi.RequestIDFrom(ctx))
			err := c.Audit.WriteDenied(ctx, s, port.AuditRecord{Action: d.OperationID, TeamID: d.TeamID,
				After: map[string]any{"requiredPermission": d.RequiredPermission}})
			if err != nil {
				log.ErrorContext(ctx, "denied audit not written", "op", d.OperationID, "err", err)
			}
		}
	}
	cfg.Hooks.CrossTeamNotFound = func(ctx context.Context, op string) {
		log.InfoContext(ctx, "cross-team not found", "metric", "afs_api_cross_team_not_found_total", "op", op)
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
	s := port.Session{Actor: p.ID, ServiceAdmin: p.ServiceAdmin, SecurityOperator: p.SecurityOperator,
		TeamIDs: p.TeamIDs(), RequestID: requestID}
	switch {
	case p.Workload != nil && p.Workload.PoolID != "":
		s.ActorType = port.ActorPool
	case p.Workload != nil:
		s.ActorType = port.ActorAgent
	case p.Type == authz.PrincipalAPIKey:
		s.ActorType = port.ActorAPIKey
	default:
		s.ActorType = port.ActorUser
	}
	return s
}

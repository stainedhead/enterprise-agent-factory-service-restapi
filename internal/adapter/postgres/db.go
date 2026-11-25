package postgres

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config configures a service's connection pool (database contract §2).
type Config struct {
	// URL is the connection string of the service's LOGIN user, without a password on RDS.
	URL string
	// Service names the service (afs-api, afs-reconciler, ...); it becomes application_name.
	Service string
	// SystemActor sets afs.actor = 'system:<Service>' on every connection, as service roles must.
	// afs-api leaves it false: it sets the actor per transaction.
	SystemActor bool
	// MaxConns bounds the pool; zero keeps the pgx default.
	MaxConns int32
	// StatementTimeout is set as statement_timeout on every connection; zero leaves it unset.
	StatementTimeout time.Duration
	// Password, when set, supplies a password for each new connection, for example an RDS IAM
	// authentication token. It is never logged.
	Password func(ctx context.Context) (string, error)
	// TeamIDs, when set, is afs.team_ids on every connection: a team gateway (afs_gateway with
	// gateway_deployment per_namespace) then reads service rows and its own team's rows only.
	TeamIDs string
}

// poolConfig turns c into a pgxpool configuration.
func (c Config) poolConfig() (*pgxpool.Config, error) {
	pc, err := pgxpool.ParseConfig(c.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse connection string: %w", err)
	}
	rp := pc.ConnConfig.RuntimeParams
	if c.Service != "" {
		rp["application_name"] = c.Service
	}
	if c.SystemActor {
		if c.Service == "" {
			return nil, fmt.Errorf("postgres: SystemActor needs Service")
		}
		rp["afs.actor"] = "system:" + c.Service
	}
	if c.TeamIDs != "" {
		rp["afs.team_ids"] = c.TeamIDs
	}
	if c.StatementTimeout > 0 {
		rp["statement_timeout"] = strconv.FormatInt(c.StatementTimeout.Milliseconds(), 10)
	}
	if c.MaxConns > 0 {
		pc.MaxConns = c.MaxConns
	}
	if c.Password != nil {
		pw := c.Password
		pc.BeforeConnect = func(ctx context.Context, cc *pgx.ConnConfig) error {
			p, err := pw(ctx)
			if err != nil {
				return fmt.Errorf("postgres: connection password: %w", err)
			}
			cc.Password = p
			return nil
		}
	}
	return pc, nil
}

// OpenPool opens and pings a pool.
func OpenPool(ctx context.Context, c Config) (*pgxpool.Pool, error) {
	pc, err := c.poolConfig()
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, MapError(err, OpRead, "")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, MapError(err, OpRead, "")
	}
	return pool, nil
}

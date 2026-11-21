package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stainedhead/enterprise-agent-factory-service-db/migrate"
	"github.com/stainedhead/enterprise-agent-factory-service-db/migrations"
)

// ExpectedSchemaVersion is the highest migration embedded in the -db module this build depends on.
func ExpectedSchemaVersion() (int, error) {
	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		return 0, err
	}
	if len(migs) == 0 {
		return 0, fmt.Errorf("postgres: no migrations embedded")
	}
	return migs[len(migs)-1].Version, nil
}

// Migrate applies pending migrations on a dedicated owner connection (afs_owner, database contract
// §2) under the advisory lock of the migrate package, so concurrent starts are safe. It returns the
// number applied. lockTimeout is passed to migrate.WithLockTimeout (0 keeps the default).
func Migrate(ctx context.Context, ownerURL string, lockTimeout time.Duration, password func(context.Context) (string, error)) (int, error) {
	cc, err := pgx.ParseConfig(ownerURL)
	if err != nil {
		return 0, fmt.Errorf("postgres: parse owner connection string: %w", err)
	}
	if password != nil {
		p, err := password(ctx)
		if err != nil {
			return 0, fmt.Errorf("postgres: owner password: %w", err)
		}
		cc.Password = p
	}
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return 0, MapError(err, OpRead, "")
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		return 0, err
	}
	var opts []migrate.Option
	if lockTimeout > 0 {
		opts = append(opts, migrate.WithLockTimeout(lockTimeout))
	}
	n, err := migrate.Up(ctx, conn, migs, opts...)
	if err != nil {
		return n, fmt.Errorf("postgres: migrate: %w", err)
	}
	return n, nil
}

// Querier is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ErrSchemaTooOld is returned by CheckSchema when the database is older than this build.
var ErrSchemaTooOld = fmt.Errorf("postgres: database schema is older than this build expects")

// CheckSchema refuses to start on a schema older than ExpectedSchemaVersion (database contract
// §1). It calls afs.schema_version() (-db 000017), which every service role may execute, because
// migrate.Version creates the tracking table first and service roles may not.
func CheckSchema(ctx context.Context, q Querier) (have, want int, err error) {
	want, err = ExpectedSchemaVersion()
	if err != nil {
		return 0, 0, err
	}
	err = q.QueryRow(ctx, `SELECT afs.schema_version()`).Scan(&have)
	if err != nil {
		return 0, want, MapError(err, OpRead, "schema version")
	}
	if have < want {
		return have, want, fmt.Errorf("%w: have %06d, want %06d", ErrSchemaTooOld, have, want)
	}
	return have, want, nil
}

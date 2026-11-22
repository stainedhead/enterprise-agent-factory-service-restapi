package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// maxRetries is how often a serialization failure or deadlock is retried (database contract §5.6).
const maxRetries = 3

// Tx is the PostgreSQL implementation of port.Tx. Repositories reach the pgx transaction with
// Unwrap.
type Tx struct {
	pgx.Tx
	session port.Session
}

// Session implements port.Tx.
func (t *Tx) Session() port.Session { return t.session }

// Unwrap returns the pgx transaction behind a port.Tx opened by a TxRunner of this package.
func Unwrap(tx port.Tx) (*Tx, error) {
	t, ok := tx.(*Tx)
	if !ok || t == nil {
		return nil, fmt.Errorf("postgres: transaction %T was not opened by postgres.TxRunner", tx)
	}
	return t, nil
}

// TxRunner implements port.TxRunner on a pool.
type TxRunner struct {
	pool *pgxpool.Pool
	// backoff returns the wait before retry n (1-based); jittered by default.
	backoff func(n int) time.Duration
}

// NewTxRunner returns a TxRunner on pool.
func NewTxRunner(pool *pgxpool.Pool) *TxRunner {
	return &TxRunner{pool: pool, backoff: jitter}
}

func jitter(n int) time.Duration {
	base := time.Duration(n) * 10 * time.Millisecond
	return base + rand.N(base) //nolint:gosec // jitter, not a secret
}

// InTx implements port.TxRunner.
func (r *TxRunner) InTx(ctx context.Context, s port.Session, fn func(context.Context, port.Tx) error) error {
	return r.run(ctx, s, pgx.ReadWrite, fn)
}

// ReadTx implements port.TxRunner.
func (r *TxRunner) ReadTx(ctx context.Context, s port.Session, fn func(context.Context, port.Tx) error) error {
	return r.run(ctx, s, pgx.ReadOnly, fn)
}

func (r *TxRunner) run(ctx context.Context, s port.Session, mode pgx.TxAccessMode, fn func(context.Context, port.Tx) error) error {
	if _, err := sessionArgs(s); err != nil {
		return MapError(err, OpRead, "")
	}
	var err error
	for attempt := 0; ; attempt++ {
		err = r.once(ctx, s, mode, fn)
		if err == nil || !retryable(err) || attempt == maxRetries {
			break
		}
		select {
		case <-ctx.Done():
			return MapError(ctx.Err(), OpRead, "")
		case <-time.After(r.backoff(attempt + 1)):
		}
	}
	return MapError(err, OpWrite, "")
}

func (r *TxRunner) once(ctx context.Context, s port.Session, mode pgx.TxAccessMode, fn func(context.Context, port.Tx) error) (err error) {
	ptx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: mode})
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rbErr := ptx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, rbErr)
			}
		}
	}()
	if err = applySession(ctx, ptx, s); err != nil {
		return err
	}
	if err = fn(ctx, &Tx{Tx: ptx, session: s}); err != nil {
		return err
	}
	return ptx.Commit(ctx)
}

var _ port.TxRunner = (*TxRunner)(nil)

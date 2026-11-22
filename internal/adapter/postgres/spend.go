package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// SpendStore implements gateway.Spend over the -db spend functions (database contract §4.2), connected
// as afs_gateway. The functions are the only way the gateway changes a counter.
type SpendStore struct{ pool *pgxpool.Pool }

// NewSpendStore returns a SpendStore.
func NewSpendStore(pool *pgxpool.Pool) *SpendStore { return &SpendStore{pool: pool} }

var _ gateway.Spend = (*SpendStore)(nil)

// EnsureCounter implements gateway.Spend: afs.ensure_spend_counter.
func (s *SpendStore) EnsureCounter(ctx context.Context, budgetID string, period time.Time) error {
	var limit string
	if err := s.pool.QueryRow(ctx, `SELECT afs.ensure_spend_counter($1, $2)::text`, budgetID, period).Scan(&limit); err != nil {
		return fmt.Errorf("ensure spend counter: %w", err)
	}
	return nil
}

const counterSQL = `SELECT limit_usd::text, spent_usd::text, reserved_usd::text
FROM afs.spend_counters WHERE budget_id = $1 AND period_start = $2`

// Reserve implements gateway.Spend: afs.reserve_spend_held. A NULL answer is a refusal, and the
// counter is read for the 402's remainingUsd.
func (s *SpendStore) Reserve(ctx context.Context, budgetID string, period time.Time, amount domain.Money, holder string) (gateway.Hold, error) {
	var id *string
	if err := s.pool.QueryRow(ctx, `SELECT afs.reserve_spend_held($1, $2, $3::numeric, $4)::text`,
		budgetID, period, amount.String(), holder).Scan(&id); err != nil {
		return gateway.Hold{}, fmt.Errorf("reserve spend: %w", err)
	}
	if id != nil {
		return gateway.Hold{ID: *id}, nil
	}
	c, err := s.counter(ctx, s.pool, budgetID, period)
	if err != nil {
		return gateway.Hold{}, err
	}
	return gateway.Hold{Limit: c.Limit, Spent: c.Spent, Reserved: c.Reserved}, nil
}

type rower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *SpendStore) counter(ctx context.Context, q rower, budgetID string, period time.Time) (gateway.Counter, error) {
	var l, sp, r string
	if err := q.QueryRow(ctx, counterSQL, budgetID, period).Scan(&l, &sp, &r); err != nil {
		return gateway.Counter{}, fmt.Errorf("read spend counter: %w", err)
	}
	var c gateway.Counter
	var err error
	for _, f := range []struct {
		dst *domain.Money
		src string
	}{{&c.Limit, l}, {&c.Spent, sp}, {&c.Reserved, r}} {
		if *f.dst, err = domain.ParseMoney("counter", f.src); err != nil {
			return gateway.Counter{}, fmt.Errorf("read spend counter: %w", err)
		}
	}
	return c, nil
}

// Settle implements gateway.Spend: afs.settle_reservation with the budget and period the hold was made
// against. An empty holdID names no hold: the function then adds the actual cost to the period and
// answers false, which is what a notify-only budget needs.
func (s *SpendStore) Settle(ctx context.Context, holdID, budgetID string, period time.Time, actual domain.Money) (gateway.Counter, error) {
	var settled bool
	var err error
	if holdID == "" {
		err = s.pool.QueryRow(ctx, `SELECT afs.settle_reservation(gen_random_uuid(), $1, $2, $3::numeric)`,
			budgetID, period, actual.String()).Scan(&settled)
	} else {
		err = s.pool.QueryRow(ctx, `SELECT afs.settle_reservation($1::uuid, $2, $3, $4::numeric)`,
			holdID, budgetID, period, actual.String()).Scan(&settled)
	}
	if err != nil {
		return gateway.Counter{}, fmt.Errorf("settle reservation: %w", err)
	}
	return s.counter(ctx, s.pool, budgetID, period)
}

// ThresholdStore implements gateway.Thresholds: the insert-once row of budget_threshold_events and the
// outbox event, in one transaction, so a crossing is announced exactly once per period.
type ThresholdStore struct {
	runner *TxRunner
	outbox OutboxWriter
	ids    port.IDGen
	clock  port.Clock
}

// NewThresholdStore returns a ThresholdStore.
func NewThresholdStore(runner *TxRunner, ids port.IDGen, clock port.Clock) *ThresholdStore {
	return &ThresholdStore{runner: runner, ids: ids, clock: clock}
}

var _ gateway.Thresholds = (*ThresholdStore)(nil)

const insertThresholdSQL = `INSERT INTO afs.budget_threshold_events (budget_id, period_start, threshold)
VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`

// Record implements gateway.Thresholds.
func (t *ThresholdStore) Record(ctx context.Context, c gateway.Crossing) (bool, error) {
	first := false
	err := t.runner.InTx(ctx, port.SystemSession("afs-gateway"), func(ctx context.Context, tx port.Tx) error {
		pt, err := Unwrap(tx)
		if err != nil {
			return err
		}
		ct, err := pt.Exec(ctx, insertThresholdSQL, c.BudgetID, c.Period, int16(c.Threshold)) //nolint:gosec // percentages are 1 to 101
		if err != nil {
			return MapError(err, OpWrite, "budget threshold")
		}
		if ct.RowsAffected() == 0 {
			first = false
			return nil
		}
		id, err := t.ids.NewID("evt")
		if err != nil {
			return err
		}
		ev, err := domain.NewEvent(c.Event, id.String(), t.clock.Now(), tx.Session().Actor, c.TeamID, "budget", c.BudgetID, c.Data)
		if err != nil {
			return err
		}
		if err := t.outbox.Publish(ctx, tx, ev); err != nil {
			return err
		}
		first = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("record threshold: %w", err)
	}
	return first, nil
}

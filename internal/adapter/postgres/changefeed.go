package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// ChannelDesiredState is the NOTIFY channel the -db triggers use (database contract §4).
const ChannelDesiredState = "afs_desired_state"

// ConnConfig turns c into the configuration of one dedicated connection (the change feed listens on
// its own connection, outside the pool).
func (c Config) ConnConfig(ctx context.Context) (*pgx.ConnConfig, error) {
	pc, err := c.poolConfig()
	if err != nil {
		return nil, err
	}
	cc := pc.ConnConfig.Copy()
	if c.Password != nil {
		pw, err := c.Password(ctx)
		if err != nil {
			return nil, fmt.Errorf("postgres: connection password: %w", err)
		}
		cc.Password = pw
	}
	return cc, nil
}

// ChangeFeed implements engine.ChangeFeed with LISTEN afs_desired_state on a dedicated connection. It
// reconnects with backoff and calls Connected after each connection, so the engine polls for what
// was missed (RC-4).
type ChangeFeed struct {
	cfg     Config
	log     *slog.Logger
	backoff engine.Backoff
	// connect opens the listening connection; tests replace it.
	connect func(ctx context.Context) (*pgx.Conn, error)
}

// NewChangeFeed returns a ChangeFeed on the connection settings of cfg (the afs_reconciler login).
func NewChangeFeed(cfg Config, log *slog.Logger) *ChangeFeed {
	if log == nil {
		log = slog.Default()
	}
	f := &ChangeFeed{cfg: cfg, log: log, backoff: engine.Backoff{Base: time.Second, Max: 30 * time.Second}}
	f.connect = func(ctx context.Context) (*pgx.Conn, error) {
		cc, err := cfg.ConnConfig(ctx)
		if err != nil {
			return nil, err
		}
		return pgx.ConnectConfig(ctx, cc)
	}
	return f
}

// Listen implements engine.ChangeFeed.
func (f *ChangeFeed) Listen(ctx context.Context, h engine.FeedHandler) error {
	failures := 0
	for ctx.Err() == nil {
		err := f.listenOnce(ctx, h, func() { failures = 0 })
		if ctx.Err() != nil {
			return nil
		}
		failures++
		wait := f.backoff.Delay(failures)
		f.log.Warn("change feed disconnected; reconnecting", slog.Any("err", err), slog.Duration("in", wait))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
	return nil
}

func (f *ChangeFeed) listenOnce(ctx context.Context, h engine.FeedHandler, connected func()) error {
	conn, err := f.connect(ctx)
	if err != nil {
		return MapError(err, OpRead, "change feed")
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if _, err := conn.Exec(ctx, "LISTEN "+ChannelDesiredState); err != nil {
		return MapError(err, OpRead, "change feed")
	}
	connected()
	if h.Connected != nil {
		h.Connected()
	}
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		ch, perr := engine.ParseChange(n.Payload)
		if perr != nil {
			f.log.Warn("ignored change notification", slog.Any("err", perr))
			continue
		}
		if h.Change != nil {
			h.Change(ch)
		}
	}
}

// Lister implements engine.Lister with the reconciler's read access to the desired-state tables.
type Lister struct {
	pool Querier2
}

// Querier2 is the pgx query surface Lister needs; *pgxpool.Pool satisfies it.
type Querier2 interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// NewLister returns a Lister on pool (the afs_reconciler login).
func NewLister(pool Querier2) *Lister { return &Lister{pool: pool} }

// listSQL returns, per kind, the query listing (id, team_id) of live resources; stale adds the
// generation test. A team's own team_id is its id.
func listSQL(kind engine.Kind, stale bool) (string, bool) {
	var table, id, team, live string
	switch kind {
	case engine.KindTeam:
		table, id, team, live = "afs.teams", "r.id", "r.id", "r.state <> 'deleted'"
	case engine.KindAgent:
		table, id, team, live = "afs.agents", "r.id", "r.team_id", "r.state <> 'deleted'"
	case engine.KindPool:
		table, id, team, live = "afs.agent_pools", "r.id", "r.team_id", "r.state <> 'deleted'"
	default:
		return "", false
	}
	q := fmt.Sprintf("SELECT %s, %s FROM %s r", id, team, table)
	if stale {
		q += fmt.Sprintf(" LEFT JOIN afs.resource_status s ON s.resource_kind = '%s' AND s.resource_id = r.id", kind)
	}
	q += " WHERE " + live
	if stale {
		q += " AND (s.resource_id IS NULL OR s.observed_generation <> r.generation)"
	}
	return q + " ORDER BY 1", true
}

// List implements engine.Lister.
func (l *Lister) List(ctx context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
	return l.query(ctx, kinds, false)
}

// Stale implements engine.Lister.
func (l *Lister) Stale(ctx context.Context, kinds []engine.Kind) ([]engine.Ref, error) {
	return l.query(ctx, kinds, true)
}

func (l *Lister) query(ctx context.Context, kinds []engine.Kind, stale bool) ([]engine.Ref, error) {
	var out []engine.Ref
	for _, k := range kinds {
		q, ok := listSQL(k, stale)
		if !ok {
			continue // a kind without a table (singletons)
		}
		rows, err := l.pool.Query(ctx, q)
		if err != nil {
			return nil, MapError(err, OpRead, strings.ToLower(string(k)))
		}
		for rows.Next() {
			var id, team string
			if err := rows.Scan(&id, &team); err != nil {
				rows.Close()
				return nil, MapError(err, OpRead, string(k))
			}
			out = append(out, engine.NewRef(k, id, team))
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, MapError(err, OpRead, string(k))
		}
	}
	return out, nil
}

var (
	_ engine.ChangeFeed = (*ChangeFeed)(nil)
	_ engine.Lister     = (*Lister)(nil)
)

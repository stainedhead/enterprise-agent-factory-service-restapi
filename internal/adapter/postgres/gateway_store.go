package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// GatewayStore implements gateway.Store with the read access of afs_gateway (database contract §2).
type GatewayStore struct{ pool *pgxpool.Pool }

// NewGatewayStore returns a GatewayStore on the afs_gateway pool.
func NewGatewayStore(pool *pgxpool.Pool) *GatewayStore { return &GatewayStore{pool: pool} }

var _ gateway.Store = (*GatewayStore)(nil)

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return gateway.ErrNotFound
	}
	return err
}

// Team implements gateway.Store.
func (g *GatewayStore) Team(ctx context.Context, id string) (gateway.TeamRecord, error) {
	t := gateway.TeamRecord{ID: id}
	err := g.pool.QueryRow(ctx, `SELECT namespace, state FROM afs.teams WHERE id = $1`, id).Scan(&t.Namespace, &t.State)
	if err != nil {
		return gateway.TeamRecord{}, notFound(fmt.Errorf("read team: %w", err))
	}
	return t, nil
}

// TeamModel implements gateway.Store: only the team's own profile row (team_inference_profiles), so no other
// team's profile is reachable. A profile still being created has no ARN; a retiring one still serves. Whether the
// model may be called at all is ModelAdmission's question, so Enabled is true for any row found.
func (g *GatewayStore) TeamModel(ctx context.Context, teamID, modelID string) (gateway.ModelAccess, error) {
	var arn *string
	var maxOut *int32
	var state string
	err := g.pool.QueryRow(ctx, `SELECT p.inference_profile_arn, p.state, m.max_output_tokens
FROM afs.team_inference_profiles p JOIN afs.models m ON m.id = p.model_id
WHERE p.team_id = $1 AND p.model_id = $2`, teamID, modelID).Scan(&arn, &state, &maxOut)
	if err != nil {
		return gateway.ModelAccess{}, notFound(fmt.Errorf("read team inference profile: %w", err))
	}
	m := gateway.ModelAccess{Enabled: true}
	if arn != nil && state != "creating" {
		m.ProfileARN = *arn
	}
	if maxOut != nil {
		m.MaxOutputTokens = int(*maxOut)
	}
	return m, nil
}

// ModelAdmission implements gateway.Store with afs.model_admission: every catalog model with the reason it is
// refused for the agent or pool, or an empty reason. A subject that does not exist (a deleted agent whose pod
// is going away) answers no rows, which is ErrNotFound.
func (g *GatewayStore) ModelAdmission(ctx context.Context, kind, subjectID string) (map[string]string, error) {
	rows, err := g.pool.Query(ctx, `SELECT model_id, coalesce(reason, '') FROM afs.model_admission($1, $2)`, kind, subjectID)
	if err != nil {
		return nil, fmt.Errorf("read model admission: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return nil, fmt.Errorf("read model admission: %w", err)
		}
		out[id] = reason
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read model admission: %w", err)
	}
	if len(out) == 0 {
		return nil, gateway.ErrNotFound
	}
	return out, nil
}

// Prices implements gateway.Store.
func (g *GatewayStore) Prices(ctx context.Context, modelID string) ([]gateway.Price, error) {
	rows, err := g.pool.Query(ctx, `SELECT effective_from, input_usd_per_mtok::text, output_usd_per_mtok::text,
cache_read_usd_per_mtok::text, cache_write_usd_per_mtok::text
FROM afs.model_prices WHERE model_id = $1 ORDER BY effective_from DESC`, modelID)
	if err != nil {
		return nil, fmt.Errorf("read prices: %w", err)
	}
	defer rows.Close()
	var out []gateway.Price
	for rows.Next() {
		var p gateway.Price
		var s [4]string
		if err := rows.Scan(&p.EffectiveFrom, &s[0], &s[1], &s[2], &s[3]); err != nil {
			return nil, fmt.Errorf("read prices: %w", err)
		}
		for i, dst := range []*domain.Money{&p.Input, &p.Output, &p.CacheRead, &p.CacheWrite} {
			if *dst, err = domain.ParseMoney("price", s[i]); err != nil {
				return nil, fmt.Errorf("read prices: %w", err)
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Budget implements gateway.Store.
func (g *GatewayStore) Budget(ctx context.Context, teamID string) (gateway.BudgetRecord, error) {
	var b gateway.BudgetRecord
	var th []int16
	var amount string
	var startsAt time.Time
	err := g.pool.QueryRow(ctx, `SELECT id, period, starts_at, thresholds, on_exhaust, amount_usd::text
FROM afs.budgets WHERE team_id = $1`, teamID).Scan(&b.ID, &b.Period, &startsAt, &th, &b.OnExhaust, &amount)
	if err != nil {
		return gateway.BudgetRecord{}, notFound(fmt.Errorf("read budget: %w", err))
	}
	b.StartsAt = startsAt
	for _, t := range th {
		b.Thresholds = append(b.Thresholds, int(t))
	}
	if b.Amount, err = domain.ParseMoney("budget", amount); err != nil {
		return gateway.BudgetRecord{}, fmt.Errorf("read budget: %w", err)
	}
	return b, nil
}

// Tags implements gateway.Store: the team's effective tags with a value.
func (g *GatewayStore) Tags(ctx context.Context, teamID string) ([]gateway.Tag, error) {
	rows, err := g.pool.Query(ctx, `SELECT e.key, e.value, e.source,
       EXISTS (SELECT 1 FROM afs.service_tags s WHERE s.key = e.key)
  FROM afs.team_effective_tags e
 WHERE e.team_id = $1 AND e.value IS NOT NULL ORDER BY e.key`, teamID)
	if err != nil {
		return nil, fmt.Errorf("read tags: %w", err)
	}
	defer rows.Close()
	var out []gateway.Tag
	for rows.Next() {
		var t gateway.Tag
		if err := rows.Scan(&t.Key, &t.Value, &t.Source, &t.Service); err != nil {
			return nil, fmt.Errorf("read tags: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TagSetID implements gateway.Store: afs.current_team_tag_set creates the set on first use.
func (g *GatewayStore) TagSetID(ctx context.Context, teamID string) (string, error) {
	var id string
	if err := g.pool.QueryRow(ctx, `SELECT afs.current_team_tag_set($1)`, teamID).Scan(&id); err != nil {
		return "", fmt.Errorf("current tag set: %w", err)
	}
	return id, nil
}

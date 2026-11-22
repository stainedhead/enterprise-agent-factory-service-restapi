package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pricing"
)

// PricingRepo implements pricing.Repository on afs.model_price_proposals as afs_app. The database
// refuses a decision by anyone but a service admin and, on approval, writes the model_prices row at
// greatest(effective_from, now()).
type PricingRepo struct{}

// NewPricingRepo returns a PricingRepo.
func NewPricingRepo() *PricingRepo { return &PricingRepo{} }

var _ pricing.Repository = (*PricingRepo)(nil)

const proposalCols = `id, model_id, effective_from, input_usd_per_mtok::text, output_usd_per_mtok::text,
cache_read_usd_per_mtok::text, cache_write_usd_per_mtok::text, fetched_at, status, note, decided_at, decided_by`

func scanProposal(row pgx.Row) (pricing.Proposal, error) {
	var p pricing.Proposal
	var s [4]string
	var eff, fetched time.Time
	if err := row.Scan(&p.ID, &p.ModelID, &eff, &s[0], &s[1], &s[2], &s[3], &fetched, &p.Status, &p.Note, &p.DecidedAt, &p.DecidedBy); err != nil {
		return p, err
	}
	p.EffectiveFrom, p.FetchedAt = eff.UTC(), fetched.UTC()
	if p.DecidedAt != nil {
		t := p.DecidedAt.UTC()
		p.DecidedAt = &t
	}
	for i, dst := range []*domain.Money{&p.Input, &p.Output, &p.CacheRead, &p.CacheWrite} {
		m, err := domain.ParseMoney("price", s[i])
		if err != nil {
			return p, err
		}
		*dst = m
	}
	return p, nil
}

// List implements pricing.Repository.
func (*PricingRepo) List(ctx context.Context, tx port.Tx, status, after string, limit int) ([]pricing.Proposal, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT `+proposalCols+` FROM afs.model_price_proposals
WHERE ($1 = '' OR status = $1) AND ($2 = '' OR id < $2) ORDER BY id DESC LIMIT $3`, status, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "price proposals")
	}
	defer rows.Close()
	out := []pricing.Proposal{}
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "price proposals")
		}
		out = append(out, p)
	}
	return out, MapError(rows.Err(), OpRead, "price proposals")
}

// Get implements pricing.Repository.
func (*PricingRepo) Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (pricing.Proposal, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return pricing.Proposal{}, err
	}
	q := `SELECT ` + proposalCols + ` FROM afs.model_price_proposals WHERE id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	p, err := scanProposal(t.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, domain.NotFound("price proposal")
	}
	return p, MapError(err, OpRead, "price proposal")
}

// Decide implements pricing.Repository.
func (*PricingRepo) Decide(ctx context.Context, tx port.Tx, id, status, note string) (pricing.Proposal, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return pricing.Proposal{}, err
	}
	p, err := scanProposal(t.QueryRow(ctx, `UPDATE afs.model_price_proposals
SET status = $2, note = coalesce(nullif($3, ''), note)
WHERE id = $1 AND status = 'pending' RETURNING `+proposalCols, id, status, note))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, domain.Conflict("invalid_state", "proposal is no longer pending", nil)
	}
	return p, MapError(err, OpWrite, "price proposal")
}

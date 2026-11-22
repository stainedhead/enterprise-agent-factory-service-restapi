// Package pricingtest is an in-memory pricing.Repository that behaves as the database does on a
// decision: only a pending proposal moves, and approval records a price effective at the later of the
// proposal's effectiveFrom and now.
package pricingtest

import (
	"context"
	"sort"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pricing"
)

// Price is a model_prices row written by an approval.
type Price struct {
	ModelID       string
	EffectiveFrom time.Time
	Input, Output domain.Money
}

// Repo is the store.
type Repo struct {
	Proposals map[string]pricing.Proposal
	// Prices are the rows approvals wrote.
	Prices []Price
	// Now is the decision time; Actor the deciding user.
	Now   time.Time
	Actor string
}

var _ pricing.Repository = (*Repo)(nil)

// List implements pricing.Repository.
func (r *Repo) List(_ context.Context, _ port.Tx, status, after string, limit int) ([]pricing.Proposal, error) {
	var out []pricing.Proposal
	for _, p := range r.Proposals {
		if (status == "" || p.Status == status) && (after == "" || p.ID < after) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Get implements pricing.Repository.
func (r *Repo) Get(_ context.Context, _ port.Tx, id string, _ bool) (pricing.Proposal, error) {
	p, ok := r.Proposals[id]
	if !ok {
		return pricing.Proposal{}, domain.NotFound("price proposal")
	}
	return p, nil
}

// Decide implements pricing.Repository.
func (r *Repo) Decide(_ context.Context, _ port.Tx, id, status, note string) (pricing.Proposal, error) {
	p, ok := r.Proposals[id]
	if !ok {
		return pricing.Proposal{}, domain.NotFound("price proposal")
	}
	if p.Status != pricing.Pending {
		return pricing.Proposal{}, domain.Conflict("invalid_state", "proposal is already "+p.Status, nil)
	}
	p.Status = status
	if note != "" {
		p.Note = note
	}
	now, actor := r.Now, r.Actor
	p.DecidedAt, p.DecidedBy = &now, &actor
	r.Proposals[id] = p
	if status == pricing.Approved {
		eff := p.EffectiveFrom
		if now.After(eff) {
			eff = now
		}
		r.Prices = append(r.Prices, Price{ModelID: p.ModelID, EffectiveFrom: eff, Input: p.Input, Output: p.Output})
	}
	return p, nil
}

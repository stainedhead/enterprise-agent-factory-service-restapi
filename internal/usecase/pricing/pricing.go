// Package pricing implements ListModelPriceProposals, ApproveModelPriceProposal and
// RejectModelPriceProposal (API contract §4.11, M4-D2, RA-7): the service admin's decision on the model
// prices the billing job proposes from the AWS Price List.
//
// Only a pending proposal can be decided (a decided or superseded one is a Conflict). Approval makes the
// database write the price, effective at the later of the proposal's effectiveFrom and the approval
// time, so usage before that moment is never repriced; the gateway reads the new row on its next price
// refresh. Both decisions publish pricing.proposal_decided in the transaction that records them.
package pricing

import (
	"context"
	"fmt"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kindProposal = "model_price_proposals"

// Proposal statuses.
const (
	Pending    = "pending"
	Approved   = "approved"
	Rejected   = "rejected"
	Superseded = "superseded"
)

// maxNote is the length the database allows for a proposal's note.
const maxNote = 500

// Proposal is a model_price_proposals row.
type Proposal struct {
	ID            string
	ModelID       string
	EffectiveFrom time.Time
	Input         domain.Money
	Output        domain.Money
	CacheRead     domain.Money
	CacheWrite    domain.Money
	FetchedAt     time.Time
	Status        string
	Note          string
	DecidedAt     *time.Time
	DecidedBy     *string
}

// Version is the proposal's contract version: 1 while it is open, 2 once it is no longer pending.
// The table has no version column; a proposal changes state once.
func (p Proposal) Version() domain.Version {
	if p.Status == Pending {
		return 1
	}
	return 2
}

// EffectiveAt is when an approved proposal's price starts: the later of effectiveFrom and the decision.
func (p Proposal) EffectiveAt() time.Time {
	if p.DecidedAt != nil && p.DecidedAt.After(p.EffectiveFrom) {
		return p.DecidedAt.UTC()
	}
	return p.EffectiveFrom.UTC()
}

// Repository is the proposal store, used inside the transaction it is given. The database enforces who
// decides (service admins) and writes the price on approval.
type Repository interface {
	// List returns proposals (of status when not empty), newest first, with an id below after, at most limit.
	List(ctx context.Context, tx port.Tx, status, after string, limit int) ([]Proposal, error)
	// Get returns a proposal, forUpdate locking it; NotFound when absent.
	Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (Proposal, error)
	// Decide moves a pending proposal to approved or rejected, sets its note when note is not empty and
	// returns the row as stored.
	Decide(ctx context.Context, tx port.Tx, id, status, note string) (Proposal, error)
}

// Service is the pricing use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// Decided is the result of a decision, or the stored response of a replayed one.
type Decided struct {
	Proposal Proposal
	Replay   *port.StoredResponse
}

func (s *Service) check(ctx context.Context, c acting.Caller, action string) error {
	if !authz.Authorize(c.Principal, authz.AdminPricing, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return s.Denied(ctx, c, action, "", authz.AdminPricing)
	}
	return nil
}

func validStatus(s string) bool {
	switch s {
	case "", Pending, Approved, Rejected, Superseded:
		return true
	}
	return false
}

// List implements ListModelPriceProposals, newest first; the cursor is the id of the last proposal.
func (s *Service) List(ctx context.Context, c acting.Caller, status string, p acting.Page) ([]Proposal, string, error) {
	if err := s.check(ctx, c, "ListModelPriceProposals"); err != nil {
		return nil, "", err
	}
	if !validStatus(status) {
		return nil, "", domain.Validation(domain.FieldError{Field: "status", Reason: "must be pending, approved, rejected or superseded"})
	}
	p = p.Normalized()
	var rows []Proposal
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, status, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(r Proposal) string { return r.ID })
	return page, next, nil
}

// Approve implements ApproveModelPriceProposal.
func (s *Service) Approve(ctx context.Context, c acting.Caller, id, note string, idem *acting.Idem) (Decided, error) {
	return s.decide(ctx, c, "ApproveModelPriceProposal", id, Approved, note, idem)
}

// Reject implements RejectModelPriceProposal.
func (s *Service) Reject(ctx context.Context, c acting.Caller, id, note string, idem *acting.Idem) (Decided, error) {
	return s.decide(ctx, c, "RejectModelPriceProposal", id, Rejected, note, idem)
}

func audit(p Proposal) map[string]any {
	return map[string]any{"modelId": p.ModelID, "status": p.Status, "effectiveFrom": p.EffectiveFrom.UTC().Format(time.RFC3339),
		"inputUsdPerMtok": p.Input.String(), "outputUsdPerMtok": p.Output.String(),
		"cacheReadUsdPerMtok": p.CacheRead.String(), "cacheWriteUsdPerMtok": p.CacheWrite.String()}
}

func (s *Service) decide(ctx context.Context, c acting.Caller, action, id, status, note string, idem *acting.Idem) (Decided, error) {
	var res Decided
	if err := s.check(ctx, c, action); err != nil {
		return res, err
	}
	if len(note) > maxNote {
		return res, domain.Validation(domain.FieldError{Field: "note", Reason: fmt.Sprintf("must be at most %d characters", maxNote)})
	}
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Decided{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		before, err := s.Repo.Get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if before.Status != Pending {
			return domain.Conflict("invalid_state", fmt.Sprintf("proposal %s is already %s", id, before.Status), map[string]any{"status": before.Status})
		}
		after, err := s.Repo.Decide(ctx, tx, id, status, note)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindProposal, id, "", audit(before), audit(after))); err != nil {
			return err
		}
		data := map[string]any{"proposalId": id, "modelId": after.ModelID, "decision": status}
		if status == Approved {
			data["effectiveFrom"] = after.EffectiveAt().Format(time.RFC3339)
		}
		if err := s.Emit(ctx, tx, c, domain.EventPricingProposalDecided, "", kindProposal, id, data); err != nil {
			return err
		}
		res.Proposal = after
		return idem.Complete(ctx, s.Idem, tx, after)
	})
	return res, err
}

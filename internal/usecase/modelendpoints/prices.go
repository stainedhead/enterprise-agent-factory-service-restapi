package modelendpoints

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// maxNote is the longest proposal note the database keeps.
const maxNote = 500

func auditPrice(p PriceRow) map[string]any {
	m := map[string]any{"endpointId": p.EndpointID, "modelName": p.ModelName, "effectiveFrom": p.EffectiveFrom.UTC().Format(time.RFC3339),
		"inputUsdPerMtok": p.Price.Input.String(), "outputUsdPerMtok": p.Price.Output.String(), "requestFeeUsd": p.Price.Fee.String(),
		"source": p.Source}
	if p.Price.CachedInput != nil {
		m["cachedInputUsdPerMtok"] = p.Price.CachedInput.String()
	}
	return m
}

// CreatePrice implements CreateEndpointModelPrice: an effective-dated price for a model of the endpoint.
func (s *Service) CreatePrice(ctx context.Context, c acting.Caller, endpointID string, p PriceRow, idem *acting.Idem) (Created[PriceRow], error) {
	var res Created[PriceRow]
	if err := s.serviceCheck(ctx, c, "CreateEndpointModelPrice", authz.AdminPricing); err != nil {
		return res, err
	}
	if p.Source == "" {
		p.Source = "manual"
	}
	if p.Source != "manual" && p.Source != "transfer" {
		return res, refused("source", "must be manual or transfer", "")
	}
	if p.EffectiveFrom.IsZero() {
		return res, refused("effectiveFrom", "is required", "")
	}
	if err := validPrice(p.Price, "price"); err != nil {
		return res, err
	}
	p.EndpointID = endpointID
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Created[PriceRow]{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		e, err := s.Repo.GetEndpoint(ctx, tx, endpointID, false)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(e.Models, func(m Model) bool { return m.ModelName == p.ModelName }) {
			return refused("modelName", domain.ReasonModelNotOnRoute, "the endpoint does not list model "+p.ModelName)
		}
		out, err := s.Repo.InsertPrice(ctx, tx, p)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateEndpointModelPrice", kindPrice, endpointID+"/"+p.ModelName, e.TeamID, nil, auditPrice(out))); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventModelEndpointUpdated, e.TeamID, kindEndpoint, endpointID,
			eventData(e.ModelEndpoint, map[string]any{"priceModel": p.ModelName, "effectiveFrom": out.EffectiveFrom.UTC().Format(time.RFC3339)})); err != nil {
			return err
		}
		res.Value = out
		return idem.Complete(ctx, s.Idem, tx, out)
	})
	return res, err
}

// ListProposals implements ListEndpointPriceProposals, newest first.
func (s *Service) ListProposals(ctx context.Context, c acting.Caller, state, endpointID string, p acting.Page) ([]Proposal, string, error) {
	if err := s.serviceCheck(ctx, c, "ListEndpointPriceProposals", authz.AdminPricing); err != nil {
		return nil, "", err
	}
	if state != "" && !slices.Contains([]string{ProposalPending, ProposalApproved, ProposalRejected, ProposalSuperseded}, state) {
		return nil, "", refused("status", "must be pending, approved, rejected or superseded", "")
	}
	p = p.Normalized()
	var rows []Proposal
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.ListProposals(ctx, tx, state, endpointID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(r Proposal) string { return r.ID })
	return page, next, nil
}

// Decided is a decision's result, or the stored response of a replayed one.
type Decided = Created[Proposal]

// Approve implements ApproveEndpointPriceProposal; the database writes the price.
func (s *Service) Approve(ctx context.Context, c acting.Caller, id, note string, idem *acting.Idem) (Decided, error) {
	return s.decide(ctx, c, "ApproveEndpointPriceProposal", id, ProposalApproved, note, idem)
}

// Reject implements RejectEndpointPriceProposal.
func (s *Service) Reject(ctx context.Context, c acting.Caller, id, note string, idem *acting.Idem) (Decided, error) {
	return s.decide(ctx, c, "RejectEndpointPriceProposal", id, ProposalRejected, note, idem)
}

func auditProposal(p Proposal) map[string]any {
	return map[string]any{"endpointId": p.EndpointID, "modelName": p.ModelName, "status": p.State,
		"effectiveFrom": p.EffectiveFrom.UTC().Format(time.RFC3339), "inputUsdPerMtok": p.Price.Input.String(),
		"outputUsdPerMtok": p.Price.Output.String()}
}

func (s *Service) decide(ctx context.Context, c acting.Caller, action, id, state, note string, idem *acting.Idem) (Decided, error) {
	var res Decided
	if err := s.serviceCheck(ctx, c, action, authz.AdminPricing); err != nil {
		return res, err
	}
	if len(note) > maxNote {
		return res, refused("note", fmt.Sprintf("must be at most %d characters", maxNote), "")
	}
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Decided{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		before, err := s.Repo.GetProposal(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if before.State != ProposalPending {
			return domain.Conflict("invalid_state", fmt.Sprintf("proposal %s is already %s", id, before.State), map[string]any{"status": before.State})
		}
		after, err := s.Repo.DecideProposal(ctx, tx, id, state, note)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kindProposal, id, after.TeamID, auditProposal(before), auditProposal(after))); err != nil {
			return err
		}
		data := map[string]any{"proposalId": id, "endpointId": after.EndpointID, "modelName": after.ModelName, "decision": state}
		if err := s.Emit(ctx, tx, c, domain.EventPricingProposalDecided, after.TeamID, kindProposal, id, data); err != nil {
			return err
		}
		res.Value = after
		return idem.Complete(ctx, s.Idem, tx, after)
	})
	return res, err
}

// ListEndpointTeams implements ListEndpointTeams.
func (s *Service) ListEndpointTeams(ctx context.Context, c acting.Caller, endpointID string, p acting.Page) ([]EndpointTeam, string, error) {
	if err := s.serviceCheck(ctx, c, "ListEndpointTeams", authz.AdminModelEndpoints); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []EndpointTeam
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		e, err := s.Repo.GetEndpoint(ctx, tx, endpointID, false)
		if err != nil {
			return err
		}
		rows, err = s.Repo.ListEndpointTeams(ctx, tx, e, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(r EndpointTeam) string { return r.TeamID })
	return page, next, nil
}

// statementProvider is the provider each statement source belongs to.
var statementProvider = map[string]string{SourceNousStatement: domain.ProviderNous, SourceOpenCodeCSV: domain.ProviderOpenCode}

// maxStatementRows is the most rows one statement holds.
const maxStatementRows = 10000

// ParsePeriod reads a statement period, YYYY-MM, as the first of the month.
func ParsePeriod(s string) (time.Time, error) {
	t, err := time.Parse("2006-01", s)
	if err != nil || t.Format("2006-01") != s {
		return time.Time{}, refused("period", "must be a month, YYYY-MM", "")
	}
	return t.UTC(), nil
}

func validStatement(st Statement) error {
	if _, ok := statementProvider[st.Source]; !ok {
		return refused("source", "must be nous_statement or opencode_csv", "")
	}
	if len(st.Rows) == 0 || len(st.Rows) > maxStatementRows {
		return refused("rows", "1 to 10000 rows", "")
	}
	var fe []domain.FieldError
	for i, r := range st.Rows {
		f := fmt.Sprintf("rows[%d]", i)
		if !domain.ValidTeamID(r.TeamID) {
			fe = append(fe, domain.FieldError{Field: f + ".teamId", Reason: "must be a team id"})
		}
		if r.Model == "" || len(r.Model) > 200 {
			fe = append(fe, domain.FieldError{Field: f + ".model", Reason: "1 to 200 characters"})
		}
		if r.AmountUSD.IsNegative() {
			fe = append(fe, domain.FieldError{Field: f + ".amountUsd", Reason: "must not be negative"})
		}
		if r.Tokens != nil && *r.Tokens < 0 {
			fe = append(fe, domain.FieldError{Field: f + ".tokens", Reason: "must not be negative"})
		}
	}
	if len(fe) > 0 {
		return domain.Validation(fe...)
	}
	return nil
}

// ImportStatement implements ImportProviderStatement: one statement per endpoint, period and source.
func (s *Service) ImportStatement(ctx context.Context, c acting.Caller, endpointID string, st Statement, idem *acting.Idem) (Created[Statement], error) {
	var res Created[Statement]
	if err := s.serviceCheck(ctx, c, "ImportProviderStatement", authz.AdminPricing); err != nil {
		return res, err
	}
	if err := validStatement(st); err != nil {
		return res, err
	}
	st.EndpointID = endpointID
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Created[Statement]{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		e, err := s.Repo.GetEndpoint(ctx, tx, endpointID, false)
		if err != nil {
			return err
		}
		if want := statementProvider[st.Source]; e.Provider != want {
			return refused("source", fmt.Sprintf("a %s statement is for a %s endpoint", st.Source, want), "")
		}
		id, err := s.IDs.NewID("pst")
		if err != nil {
			return err
		}
		st.ID = id.String()
		out, err := s.Repo.InsertStatement(ctx, tx, st)
		if err != nil {
			return err
		}
		aud := map[string]any{"endpointId": endpointID, "period": out.Period.Format("2006-01"), "source": out.Source,
			"rowCount": len(out.Rows), "totalUsd": out.Total().String()}
		if err := s.Audit.Write(ctx, tx, c.Record("ImportProviderStatement", kindStatement, out.ID, "", nil, aud)); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventModelEndpointUpdated, "", kindEndpoint, endpointID,
			eventData(e.ModelEndpoint, map[string]any{"statementId": out.ID, "period": out.Period.Format("2006-01")})); err != nil {
			return err
		}
		res.Value = out
		return idem.Complete(ctx, s.Idem, tx, out)
	})
	return res, err
}

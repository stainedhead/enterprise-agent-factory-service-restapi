package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pricing"
)

const pricingAreaName = "pricing"

func init() { Areas = append(Areas, Area{pricingAreaName, none}) }

// PricingAPI serves ListModelPriceProposals, ApproveModelPriceProposal and RejectModelPriceProposal
// (M4-D2, RA-7).
type PricingAPI struct {
	Svc   *pricing.Service
	Pages PageTokens
}

// BindPricing binds the pricing handlers as the pricing area.
func BindPricing(a *PricingAPI) { w2SetArea(pricingAreaName, a.Register) }

// Register binds the pricing operations.
func (a *PricingAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListModelPriceProposals":   a.list,
		"ApproveModelPriceProposal": a.approve,
		"RejectModelPriceProposal":  a.reject,
	})
}

func (a *PricingAPI) list(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]pricing.Proposal, string, error) {
		return a.Svc.List(r.Context(), c, status, p)
	}, toPriceProposalBody)
}

func (a *PricingAPI) approve(w http.ResponseWriter, r *http.Request) {
	a.decide(w, r, "ApproveModelPriceProposal", a.Svc.Approve)
}

func (a *PricingAPI) reject(w http.ResponseWriter, r *http.Request) {
	a.decide(w, r, "RejectModelPriceProposal", a.Svc.Reject)
}

type decideFunc func(ctx context.Context, c acting.Caller, id, note string, idem *acting.Idem) (pricing.Decided, error)

func (a *PricingAPI) decide(w http.ResponseWriter, r *http.Request, op string, do decideFunc) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Note *string `json:"note"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	note := ""
	if req.Note != nil {
		note = *req.Note
	}
	id := r.PathValue("proposalId")
	// The proposal is part of the request the key claims (the operation name takes letters only): one key
	// reused on another proposal is a different request.
	idem, err := w2Idem(r, op, "", append([]byte(id+"\n"), raw...), func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusOK, toPriceProposalBody(v.(pricing.Proposal)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := do(r.Context(), c, id, note, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Proposal.Version())
	WriteJSON(w, http.StatusOK, toPriceProposalBody(res.Proposal))
}

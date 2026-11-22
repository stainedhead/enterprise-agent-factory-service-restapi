package httpapi

import (
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pricing"
)

// priceProposalBody is the PriceProposal of API contract §5. The id is the proposal's prefixed id
// (mpp_…); version is 1 while the proposal is pending and 2 once decided or superseded; note is null
// when empty.
type priceProposalBody struct {
	ID                   string       `json:"id"`
	ModelID              string       `json:"modelId"`
	EffectiveFrom        string       `json:"effectiveFrom"`
	InputUsdPerMtok      domain.Money `json:"inputUsdPerMtok"`
	OutputUsdPerMtok     domain.Money `json:"outputUsdPerMtok"`
	CacheReadUsdPerMtok  domain.Money `json:"cacheReadUsdPerMtok"`
	CacheWriteUsdPerMtok domain.Money `json:"cacheWriteUsdPerMtok"`
	Status               string       `json:"status"`
	FetchedAt            string       `json:"fetchedAt"`
	DecidedAt            *string      `json:"decidedAt"`
	DecidedBy            *string      `json:"decidedBy"`
	Note                 *string      `json:"note"`
	Version              int64        `json:"version"`
}

func toPriceProposalBody(p pricing.Proposal) priceProposalBody {
	var note *string
	if p.Note != "" {
		note = &p.Note
	}
	return priceProposalBody{ID: p.ID, ModelID: p.ModelID, EffectiveFrom: w2Time(p.EffectiveFrom), InputUsdPerMtok: p.Input,
		OutputUsdPerMtok: p.Output, CacheReadUsdPerMtok: p.CacheRead, CacheWriteUsdPerMtok: p.CacheWrite, Status: p.Status,
		FetchedAt: w2Time(p.FetchedAt), DecidedAt: w2TimePtr(p.DecidedAt), DecidedBy: p.DecidedBy, Note: note, Version: int64(p.Version())}
}

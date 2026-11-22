package matrix

import (
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
)

// Cases of M4-D2 (price proposals). The list is a read. Approve and reject each find a pending
// proposal of a model of their own, made again before every call, so every caller meets an open one.

func init() {
	pending := func(t *testing.T, w *World) map[string]string {
		model := "anthropic.matrix-" + strings.ToLower(pgtest.NewID("m")[2:]) + "-v1:0"
		id := pgtest.NewID("mpp")
		w.exec(t, `INSERT INTO afs.models (id, provider, display_name, context_window, max_output_tokens) VALUES ($1, 'Example', 'Matrix', 1000, 100)`, model)
		w.exec(t, `INSERT INTO afs.model_prices (model_id, effective_from, input_usd_per_mtok, output_usd_per_mtok, source) VALUES ($1, now() - interval '30 days', 3, 15, 'manual')`, model)
		w.exec(t, `INSERT INTO afs.model_price_proposals (id, model_id, effective_from, input_usd_per_mtok, output_usd_per_mtok)
VALUES ($1, $2, now() + interval '1 day', 3.3, 16.5)`, id, model)
		return map[string]string{"pid": id}
	}
	Register(Case{Op: "ListModelPriceProposals", Path: "/v1/admin/model-price-proposals?status=pending"})
	Register(Case{Op: "ApproveModelPriceProposal", Path: "/v1/admin/model-price-proposals/{pid}:approve",
		Body: `{"note":"matches the AWS price list"}`, Prepare: pending})
	Register(Case{Op: "RejectModelPriceProposal", Path: "/v1/admin/model-price-proposals/{pid}:reject",
		Body: `{"note":"waiting for the contract rate"}`, Prepare: pending})
}

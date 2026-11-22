package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pricing"
)

type pricingRig struct {
	svc   *pricing.Service
	db    *pgtest.DB
	model string
}

func newPricingRig(t *testing.T) *pricingRig {
	t.Helper()
	db := pgtest.Get(t)
	runner := postgres.NewTxRunner(appPool(t, db))
	r := &pricingRig{db: db, model: "anthropic.price-model-" + strings.ToLower(pgtest.NewID("x")[2:]) + "-v1:0"}
	r.svc = &pricing.Service{Deps: acting.Deps{Tx: runner, Audit: postgres.NewAuditWriter(runner), Outbox: postgres.NewOutboxWriter(),
		Idem: postgres.NewIdempotencyStore(), Clock: &fakes.Clock{T: time.Now().UTC()}, IDs: billingIDs{}}, Repo: postgres.NewPricingRepo()}
	admin := db.Admin(t)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM afs.outbox_events WHERE event_type = 'pricing.proposal_decided'`)
	})
	for _, q := range []string{
		`INSERT INTO afs.models (id, provider, display_name, context_window, max_output_tokens) VALUES ($1, 'Example', 'Price', 1000, 100)`,
		`INSERT INTO afs.model_prices (model_id, effective_from, input_usd_per_mtok, output_usd_per_mtok, source) VALUES ($1, now() - interval '30 days', 3, 15, 'manual')`,
	} {
		if _, err := admin.Exec(t.Context(), q, r.model); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (r *pricingRig) propose(t *testing.T, in string, effective time.Time) string {
	t.Helper()
	id := pgtest.NewID("mpp")
	if _, err := r.db.Admin(t).Exec(t.Context(), `INSERT INTO afs.model_price_proposals (id, model_id, effective_from, input_usd_per_mtok, output_usd_per_mtok)
VALUES ($1, $2, $3, $4::numeric, 18)`, id, r.model, effective, in); err != nil {
		t.Fatal(err)
	}
	return id
}

func svcAdmin() acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: user, Type: authz.PrincipalUser, ServiceAdmin: true}, RequestID: "req_price"}
}

func gatewayPrices(t *testing.T, db *pgtest.DB, model string) []gateway.Price {
	t.Helper()
	pool, err := postgres.OpenPool(t.Context(), postgres.Config{URL: db.URLs["afs_gateway"], Service: "afs-gateway", SystemActor: true, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ps, err := postgres.NewGatewayStore(pool).Prices(t.Context(), model)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// RA-7: approval writes the price from max(effectiveFrom, approval): a past effective time starts now,
// so usage before the approval keeps its price, and the gateway reads the new row.
func TestApproveWritesThePriceAndTheGatewayUsesItFromThen(t *testing.T) {
	r := newPricingRig(t)
	id := r.propose(t, "3.5", time.Now().UTC().Add(-72*time.Hour))
	start := time.Now()
	res, err := r.svc.Approve(t.Context(), svcAdmin(), id, "matches the list", nil)
	if err != nil {
		t.Fatal(err)
	}
	p := res.Proposal
	if p.Status != pricing.Approved || p.Note != "matches the list" || p.DecidedBy == nil || *p.DecidedBy != user || p.DecidedAt == nil {
		t.Fatalf("proposal = %+v", p)
	}
	ps := gatewayPrices(t, r.db, r.model)
	if len(ps) != 2 {
		t.Fatalf("prices = %+v", ps)
	}
	if ps[0].Input.String() != "3.50" || ps[0].EffectiveFrom.Before(start.Add(-time.Minute)) {
		t.Fatalf("new price = %+v, approved around %v", ps[0], start)
	}
	// The gateway's lookup: before the effective time the old price, after it the new.
	if got, ok := gateway.EffectivePrice(ps, ps[0].EffectiveFrom.Add(-time.Hour)); !ok || got.Input.String() != "3.00" {
		t.Fatalf("an hour earlier = %+v", got)
	}
	if got, ok := gateway.EffectivePrice(ps, time.Now().Add(time.Minute)); !ok || got.Input.String() != "3.50" {
		t.Fatalf("after = %+v", got)
	}
	// A decided proposal cannot be decided again.
	if _, err := r.svc.Reject(t.Context(), svcAdmin(), id, "", nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("reject after approve: %v", err)
	}
	admin := r.db.Admin(t)
	var audits, events int
	if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.audit_log WHERE action = 'ApproveModelPriceProposal' AND resource_id = $1`, id).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit rows = %d, %v", audits, err)
	}
	if err := admin.QueryRow(t.Context(), `SELECT count(*) FROM afs.outbox_events WHERE event_type = 'pricing.proposal_decided' AND aggregate_id = $1`, id).Scan(&events); err != nil || events != 1 {
		t.Fatalf("events = %d, %v", events, err)
	}
}

func TestApproveKeepsAFutureEffectiveTimeAndRejectWritesNothing(t *testing.T) {
	r := newPricingRig(t)
	future := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	id := r.propose(t, "4", future)
	if _, err := r.svc.Approve(t.Context(), svcAdmin(), id, "", nil); err != nil {
		t.Fatal(err)
	}
	ps := gatewayPrices(t, r.db, r.model)
	if len(ps) != 2 || !ps[0].EffectiveFrom.Equal(future) {
		t.Fatalf("prices = %+v", ps)
	}
	// Until then the gateway still charges the old price.
	if got, ok := gateway.EffectivePrice(ps, time.Now()); !ok || got.Input.String() != "3.00" {
		t.Fatalf("now = %+v", got)
	}

	rej := r.propose(t, "9", future.Add(time.Hour))
	res, err := r.svc.Reject(t.Context(), svcAdmin(), rej, "too high", nil)
	if err != nil || res.Proposal.Status != pricing.Rejected {
		t.Fatalf("reject: %+v %v", res, err)
	}
	if len(gatewayPrices(t, r.db, r.model)) != 2 {
		t.Fatal("a rejection wrote a price")
	}
}

func TestListFiltersByStatusNewestFirst(t *testing.T) {
	r := newPricingRig(t)
	a := r.propose(t, "5", time.Now().Add(time.Hour))
	if _, err := r.svc.Reject(t.Context(), svcAdmin(), a, "", nil); err != nil {
		t.Fatal(err)
	}
	b := r.propose(t, "6", time.Now().Add(time.Hour))
	// One pending proposal per model: the first is decided, so the second is allowed.
	rows, _, err := r.svc.List(t.Context(), svcAdmin(), "pending", acting.Page{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range rows {
		if p.Status != pricing.Pending {
			t.Fatalf("status filter leaked %+v", p)
		}
		found = found || p.ID == b
	}
	if !found {
		t.Fatalf("pending proposal %s not listed", b)
	}
	rows, _, err = r.svc.List(t.Context(), svcAdmin(), "rejected", acting.Page{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range rows {
		if i > 0 && rows[i-1].ID < p.ID {
			t.Fatal("not newest first")
		}
	}
}

// Defense in depth: the database refuses a decision by a caller who is not a service admin, whatever
// the use case did.
func TestDatabaseRefusesADecisionByANonAdmin(t *testing.T) {
	r := newPricingRig(t)
	id := r.propose(t, "3.1", time.Now().Add(time.Hour))
	runner := postgres.NewTxRunner(appPool(t, r.db))
	err := runner.InTx(t.Context(), userSession(), func(ctx context.Context, tx port.Tx) error {
		_, err := postgres.NewPricingRepo().Decide(ctx, tx, id, pricing.Approved, "")
		return err
	})
	if domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("err = %v", err)
	}
	var n int
	if err := r.db.Admin(t).QueryRow(t.Context(), `SELECT count(*) FROM afs.model_prices WHERE model_id = $1`, r.model).Scan(&n); err != nil || n != 1 {
		t.Fatalf("prices = %d, %v", n, err)
	}
}

package postgres_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/prices"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/prices/pricestest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

// BI-3: the sync as afs_billing against a real database: one pending proposal per changed model, the
// older one superseded, an unchanged price left alone, one event per proposal.
func TestPriceSyncProposesSupersedesAndStaysQuiet(t *testing.T) {
	g := newBillingRigAs(t, "afs_billing")
	team := g.db.SeedTeam(t)
	model := "anthropic.sync-model-" + strings.ToLower(team.Slug) + "-v1:0"
	name := "Sync Model " + strings.ToLower(team.Slug)
	g.exec(`INSERT INTO afs.models (id, provider, display_name, context_window, max_output_tokens) VALUES ($1, 'Example', 'Sync', 1000, 100)`, model)
	g.exec(`INSERT INTO afs.model_prices (model_id, effective_from, input_usd_per_mtok, output_usd_per_mtok, cache_read_usd_per_mtok, cache_write_usd_per_mtok, source)
VALUES ($1, now() - interval '30 days', 3, 15, 0.3, 3.75, 'manual')`, model)
	from := g.scalar(`SELECT coalesce(max(id), 0)::text FROM afs.outbox_events`)

	on := true
	list := &pricestest.List{}
	set := func(in, out string) {
		eff := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
		list.Prices = []prices.ListPrice{
			{Product: name, Kind: prices.Input, USDPerMtok: domain.MustMoney(in), EffectiveFrom: eff},
			{Product: name, Kind: prices.Output, USDPerMtok: domain.MustMoney(out), EffectiveFrom: eff},
		}
	}
	s := &prices.Syncer{List: list, Store: postgres.NewPriceStore(g.runner, &on), Tx: g.runner, Outbox: postgres.NewOutboxWriter(),
		Clock: &fakes.Clock{T: time.Now().UTC()}, IDs: billingIDs{}, Session: postgres.BillingSession()}

	count := func(q string) string { return g.scalar(q, model) }
	set("3", "15") // equal to the current price
	if res, err := s.Run(t.Context()); err != nil || res.Proposed != 0 || res.Unchanged < 1 {
		t.Fatalf("unchanged: %+v, %v", res, err)
	}
	set("3.3", "16.5")
	for i := range 2 { // the second run finds the same proposal open
		res, err := s.Run(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if want := 1 - i; res.Proposed != want {
			t.Fatalf("run %d: %+v", i, res)
		}
	}
	if n := count(`SELECT count(*)::text FROM afs.model_price_proposals WHERE model_id = $1 AND status = 'pending'`); n != "1" {
		t.Fatalf("pending = %s", n)
	}
	// The cache prices are kept from the current price; the note says what changed.
	if v := count(`SELECT cache_read_usd_per_mtok::text || '/' || cache_write_usd_per_mtok::text FROM afs.model_price_proposals WHERE model_id = $1`); v != "0.300000/3.750000" {
		t.Fatalf("cache prices = %s", v)
	}
	set("3.6", "18")
	if res, err := s.Run(t.Context()); err != nil || res.Proposed != 1 || res.Superseded != 1 {
		t.Fatalf("newer: %+v, %v", res, err)
	}
	if v := count(`SELECT string_agg(status || ':' || input_usd_per_mtok::text, ',' ORDER BY status) FROM afs.model_price_proposals WHERE model_id = $1`); v != "pending:3.600000,superseded:3.300000" {
		t.Fatalf("proposals = %s", v)
	}
	if n := g.scalar(`SELECT count(*)::text FROM afs.outbox_events WHERE id > $1::bigint AND event_type = 'pricing.proposal_created' AND aggregate_id IN
(SELECT id FROM afs.model_price_proposals WHERE model_id = $2)`, from, model); n != "2" {
		t.Fatalf("events = %s", n)
	}
	// The sync never writes a price: approval does.
	if n := count(`SELECT count(*)::text FROM afs.model_prices WHERE model_id = $1`); n != "1" {
		t.Fatalf("prices = %s", n)
	}
}

// The billing role cannot read service_settings yet (-db gap X-DB-9): the sync then uses the configured
// value, and fails closed without one. The test skips once the role can read the setting.
func TestPriceSyncEnabledFallsBackWhenTheSettingIsRefused(t *testing.T) {
	g := newBillingRigAs(t, "afs_billing")
	probe := postgres.NewPriceStore(g.runner, nil)
	_, err := probe.SyncEnabled(t.Context())
	if err == nil {
		t.Skip("afs_billing can read pricing.price_list_sync: the env fallback is no longer needed")
	}
	if !errors.Is(err, postgres.ErrSyncSettingUnavailable) {
		t.Fatalf("err = %v", err)
	}
	for _, v := range []bool{true, false} {
		on, err := postgres.NewPriceStore(g.runner, &v).SyncEnabled(t.Context())
		if err != nil || on != v {
			t.Fatalf("fallback %v: %v, %v", v, on, err)
		}
	}
}

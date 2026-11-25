package postgres_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/routebill"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

type fixedPuller struct {
	provider string
	figs     []routebill.Figure
}

func (p fixedPuller) Provider() string { return p.provider }
func (p fixedPuller) Pull(context.Context, routebill.Endpoint, time.Time) ([]routebill.Figure, domain.Money, error) {
	return p.figs, domain.Money{}, nil
}

// TC-BL-01..06 as afs_billing against the schema: a provider pull and a statement compared with the route's
// settled gateway cost, a statement route without a statement, capacity allocated by token share into
// cost_daily, and route price proposals.
func TestRouteBillingAgainstTheSchema(t *testing.T) {
	g := newBillingRig(t)
	ctx := t.Context()
	tm := g.db.SeedTeam(t)
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -2)
	at := day.Add(3 * time.Hour)
	month := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
	endpoint := func(prefix, url, pricing string) (string, string) {
		slug, id := mepSlug(prefix), pgtest.NewID("mep")
		g.exec(`INSERT INTO afs.model_endpoints (id, slug, display_name, location, base_url, auth_kind, pricing_mode, enabled)
VALUES ($1, $2, 'Billing', 'external', $3, 'bearer', $4, true)`, id, slug, url, pricing)
		return slug, id
	}
	_, oai := endpoint("oai", "https://api.openai.com/v1", "per_token")
	g.exec(`INSERT INTO afs.endpoint_models (endpoint_id, model_name, max_output_tokens) VALUES ($1, 'gpt-x', 1000), ($1, 'gpt-y', 1000)`, oai)
	g.exec(`INSERT INTO afs.endpoint_model_prices (endpoint_id, model_name, effective_from, input_per_mtok_usd, output_per_mtok_usd)
VALUES ($1, 'gpt-x', now() - interval '1 day', 1.25, 10), ($1, 'gpt-y', now() - interval '1 day', 1, 2)`, oai)
	_, nous := endpoint("nous", "https://inference-api.nousresearch.com/v1", "per_token")
	ocSlug, _ := endpoint("oc", "https://opencode.ai/zen/v1", "per_token")
	capSlug, capEP := endpoint("cap", "https://llm.example.com/v1", "capacity")
	agent1, agent2 := pgtest.NewID("agt"), pgtest.NewID("agt")
	g.exec(`INSERT INTO afs.usage_events (occurred_at, team_id, agent_id, model_id, endpoint_id, model_name, request_id, outcome,
  input_tokens, output_tokens, cost_usd) VALUES
 ($1, $2, $3, NULL, $4, 'gpt-x', $6, 'ok', 100, 0, 6.00),
 ($1, $2, $3, NULL, $4, 'gpt-x', $7, 'rejected_budget', 100, 0, 9.00),
 ($1, $2, $3, NULL, $5, 'm', $8, 'ok', 100, 0, 0),
 ($1, $2, $9, NULL, $5, 'm', $10, 'ok', 200, 100, 0)`,
		at, tm.ID, agent1, oai, capEP, pgtest.NewID("req"), pgtest.NewID("req"), pgtest.NewID("req"), agent2, pgtest.NewID("req"))
	g.exec(`INSERT INTO afs.usage_events (occurred_at, team_id, agent_id, model_id, endpoint_id, model_name, request_id, outcome,
  input_tokens, output_tokens, cost_usd) VALUES ($1, $2, $3, NULL, $4, 'm', $5, 'ok', 1, 0, 40.00)`,
		month.Add(time.Hour), tm.ID, agent1, nous, pgtest.NewID("req"))
	g.exec(`INSERT INTO afs.provider_statements (id, endpoint_id, period, source, rows) VALUES ($1, $2, $3::date, 'nous_statement',
  jsonb_build_array(jsonb_build_object('teamId', $4::text, 'model', 'm', 'amountUsd', 30), jsonb_build_object('teamId', $4::text, 'model', 'n', 'amountUsd', 20)))`,
		pgtest.NewID("pst"), nous, month.Format(time.DateOnly), tm.ID)
	g.exec(`INSERT INTO afs.endpoint_capacity_costs (endpoint_id, team_id, effective_from, monthly_usd) VALUES
 ($1, $2, $3::date - 40, 999), ($1, $2, $3::date - 1, 300)`, capEP, tm.ID, day.Format(time.DateOnly))
	from := g.scalar(`SELECT coalesce(max(id), 0)::text FROM afs.outbox_events`)

	store := postgres.NewRouteBillStore()
	j := &routebill.Job{Tx: g.runner, Outbox: postgres.NewOutboxWriter(), Clock: &fakes.Clock{T: time.Now().UTC()}, IDs: billingIDs{},
		Store: store, Session: postgres.BillingSession(),
		Pullers: []routebill.Puller{fixedPuller{provider: "openai", figs: []routebill.Figure{{TeamID: tm.ID, AmountUSD: domain.MustMoney("5.00")}}}}}

	// TC-BL-01, TC-BL-04: 6.00 settled (the rejected call is not a call) against 5.00 billed: 20% over.
	res, err := j.PullDay(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(res.Variances, func(v routebill.Variance) bool { return v.EndpointID == oai })
	if i < 0 || res.Variances[i].GatewayUSD.String() != "6.00" || res.Variances[i].Pct != 20 || !res.Variances[i].Over {
		t.Fatalf("pull = %+v", res)
	}
	events := func() string {
		return g.scalar(`SELECT count(*)::text FROM afs.outbox_events WHERE id > $1::bigint AND event_type = 'cost.variance_detected' AND team_id = $2`, from, tm.ID)
	}
	if n := events(); n != "1" {
		t.Fatalf("events = %s", n)
	}

	// TC-BL-05, TC-BL-06: the Nous statement (50.00) against 40.00 settled; OpenCode has no statement.
	res, err = j.ReconcileMonth(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	i = slices.IndexFunc(res.Variances, func(v routebill.Variance) bool { return v.EndpointID == nous })
	if i < 0 || res.Variances[i].ProviderUSD.String() != "50.00" || res.Variances[i].Pct != -20 || !slices.Contains(res.NotReconciled, ocSlug) {
		t.Fatalf("month = %+v", res)
	}
	if n := events(); n != "2" {
		t.Fatalf("events = %s", n)
	}

	// TC-BL-02: 300 a month in force (the older 999 is not), split 100:300 tokens.
	if _, err := j.AllocateCapacity(ctx, day); err != nil {
		t.Fatal(err)
	}
	days := time.Date(day.Year(), day.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	want := domain.MoneyFromMicros(300_000_000 / int64(days))
	if v := g.scalar(`SELECT sum(cost_usd)::text FROM afs.cost_daily WHERE team_id = $1 AND day = $2::date AND source = 'route_capacity' AND model_id = $3`,
		tm.ID, day.Format(time.DateOnly), capSlug); domain.MustMoney(v).Cmp(want) != 0 {
		t.Fatalf("capacity sum = %s, want %s", v, want)
	}
	if v := g.scalar(`SELECT string_agg(agent_id, ',' ORDER BY cost_usd) FROM afs.cost_daily WHERE team_id = $1 AND day = $2::date AND source = 'route_capacity'`,
		tm.ID, day.Format(time.DateOnly)); v != agent1+","+agent2 {
		t.Fatalf("capacity rows = %s", v)
	}
	if _, err := j.AllocateCapacity(ctx, day); err != nil { // a rerun replaces the day
		t.Fatal(err)
	}
	if n := g.scalar(`SELECT count(*)::text FROM afs.cost_daily WHERE team_id = $1 AND source = 'route_capacity'`, tm.ID); n != "2" {
		t.Fatalf("rows after rerun = %s", n)
	}

	// TC-BL-03: gpt-x unchanged, gpt-y changed: one pending proposal, then a newer one supersedes it.
	list := func(in string) []routebill.ListedPrice {
		return []routebill.ListedPrice{
			{Provider: "openai", Model: "gpt-x", Input: domain.MustMoney("1.25"), Output: domain.MustMoney("10")},
			{Provider: "openai", Model: "gpt-y", Input: domain.MustMoney(in), Output: domain.MustMoney("2")},
		}
	}
	proposals := func() string {
		return g.scalar(`SELECT string_agg(model_name || ':' || state || ':' || input_per_mtok_usd::text || ':' || source, ',' ORDER BY fetched_at, state)
  FROM afs.endpoint_price_proposals WHERE endpoint_id = $1`, oai)
	}
	if _, _, err := j.ProposePrices(ctx, list("0.5")); err != nil {
		t.Fatal(err)
	}
	if v := proposals(); v != "gpt-y:pending:0.500000:price_list" {
		t.Fatalf("proposals = %s", v)
	}
	if p, _, err := j.ProposePrices(ctx, list("0.5")); err != nil || p != 0 {
		t.Fatalf("same list again: %d %v", p, err)
	}
	if p, s, err := j.ProposePrices(ctx, list("0.75")); err != nil || p != 1 || s != 1 {
		t.Fatalf("newer: %d %d %v", p, s, err)
	}
	if n := g.scalar(`SELECT count(*)::text FROM afs.endpoint_price_proposals WHERE endpoint_id = $1 AND state = 'superseded'`, oai); n != "1" {
		t.Fatalf("superseded = %s (%s)", n, proposals())
	}
	if th, err := readThreshold(ctx, g, store); err != nil || th != 5 {
		t.Fatalf("threshold = %v %v", th, err)
	}
}

func readThreshold(ctx context.Context, g *billingRig, s *postgres.RouteBillStore) (float64, error) {
	var th float64
	err := g.runner.ReadTx(ctx, postgres.BillingSession(), func(ctx context.Context, tx port.Tx) error {
		var err error
		th, err = s.Threshold(ctx, tx)
		return err
	})
	return th, err
}

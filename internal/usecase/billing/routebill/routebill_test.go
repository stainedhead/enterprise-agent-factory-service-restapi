package routebill_test

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/routebill"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamB = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
)

var (
	day    = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	openai = routebill.Endpoint{ID: "mep_openai", Slug: "openai", Provider: "openai"}
	nous   = routebill.Endpoint{ID: "mep_nous", Slug: "nous-portal", Provider: "nous"}
	vllm   = routebill.Endpoint{ID: "mep_vllm", Slug: "vllm", Provider: "internal"}
	errX   = errors.New("boom")
)

func m(s string) domain.Money { return domain.MustMoney(s) }
func mp(s string) *domain.Money {
	v := m(s)
	return &v
}

type store struct {
	eps        []routebill.Endpoint
	gw         map[string]map[string]domain.Money
	statements []routebill.Statement
	stRoutes   []routebill.Endpoint
	caps       []routebill.Capacity
	shares     map[string][]routebill.Share
	written    []routebill.CostRow
	models     []routebill.RouteModel
	superseded []string
	inserted   []routebill.Proposal
	err        error
}

func (s *store) Threshold(context.Context, port.Tx) (float64, error) { return 5, s.err }
func (s *store) Endpoints(_ context.Context, _ port.Tx, providers []string) ([]routebill.Endpoint, error) {
	var out []routebill.Endpoint
	for _, e := range s.eps {
		if slices.Contains(providers, e.Provider) {
			out = append(out, e)
		}
	}
	return out, nil
}
func (s *store) GatewayCost(_ context.Context, _ port.Tx, id string, _, _ time.Time) (map[string]domain.Money, error) {
	return s.gw[id], nil
}
func (s *store) Statements(context.Context, port.Tx, time.Time) ([]routebill.Statement, error) {
	return s.statements, nil
}
func (s *store) StatementRoutes(context.Context, port.Tx) ([]routebill.Endpoint, error) {
	return s.stRoutes, nil
}
func (s *store) Capacities(context.Context, port.Tx, time.Time) ([]routebill.Capacity, error) {
	return s.caps, nil
}
func (s *store) Shares(_ context.Context, _ port.Tx, ep, team string, _ time.Time) ([]routebill.Share, error) {
	return s.shares[ep+"/"+team], nil
}
func (s *store) ReplaceCapacity(_ context.Context, _ port.Tx, _ time.Time, rows []routebill.CostRow) error {
	s.written = rows
	return nil
}
func (s *store) RouteModels(context.Context, port.Tx) ([]routebill.RouteModel, error) {
	return s.models, s.err
}
func (s *store) Supersede(_ context.Context, _ port.Tx, id string) error {
	s.superseded = append(s.superseded, id)
	return nil
}
func (s *store) Insert(_ context.Context, _ port.Tx, p routebill.Proposal) error {
	s.inserted = append(s.inserted, p)
	return nil
}

type puller struct {
	provider string
	figs     []routebill.Figure
	un       domain.Money
	err      error
}

func (p puller) Provider() string { return p.provider }
func (p puller) Pull(context.Context, routebill.Endpoint, time.Time) ([]routebill.Figure, domain.Money, error) {
	return p.figs, p.un, p.err
}

func job(s *store, ps ...routebill.Puller) (*routebill.Job, *fakes.Outbox) {
	ob := &fakes.Outbox{}
	return &routebill.Job{Tx: &fakes.TxRunner{}, Outbox: ob, Clock: &fakes.Clock{T: day.Add(28 * time.Hour)}, IDs: &fakes.IDGen{},
		Store: s, Pullers: ps, Session: port.SystemSession("afs-billing")}, ob
}

// TC-BL-01, TC-BL-04: the provider's per-team figures against the gateway's; over 5% publishes one event.
func TestPullDay(t *testing.T) {
	s := &store{eps: []routebill.Endpoint{openai, nous}, gw: map[string]map[string]domain.Money{
		openai.ID: {teamA: m("10.00"), teamB: m("10.00")},
	}}
	j, ob := job(s, puller{provider: "openai", un: m("0.40"), figs: []routebill.Figure{
		{TeamID: teamA, AmountUSD: m("10.20")}, {TeamID: teamB, AmountUSD: m("8.00")}}})
	res, err := j.PullDay(t.Context(), day.Add(5*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Variances) != 2 || res.Variances[0].Over || !res.Variances[1].Over || res.Variances[1].Pct != 25 || res.Events != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(ob.Events) != 1 || ob.Events[0].Type != domain.EventCostVarianceDetected || ob.Events[0].Detail.TeamID != teamB ||
		ob.Events[0].Detail.Data["route"] != "openai" || ob.Events[0].Detail.Data["period"] != "2026-11-20" {
		t.Errorf("events = %+v", ob.Events)
	}
	if res.Unattributed["openai"].String() != "0.40" {
		t.Errorf("unattributed = %v", res.Unattributed)
	}
	// A failed pull leaves the route out and is reported.
	j, _ = job(s, puller{provider: "openai", err: errX})
	if res, err := j.PullDay(t.Context(), day); !errors.Is(err, errX) || len(res.Variances) != 0 {
		t.Errorf("failed pull: %+v %v", res, err)
	}
	s.err = errX
	j, _ = job(s, puller{provider: "openai"})
	if _, err := j.PullDay(t.Context(), day); !errors.Is(err, errX) {
		t.Errorf("store error: %v", err)
	}
}

// TC-BL-05, TC-BL-06: statements reconcile monthly; a statement route without one is not reconciled.
func TestReconcileMonth(t *testing.T) {
	opencode := routebill.Endpoint{ID: "mep_oc", Slug: "opencode", Provider: "opencode"}
	s := &store{stRoutes: []routebill.Endpoint{nous, opencode},
		statements: []routebill.Statement{{Endpoint: nous, Source: "nous_statement", Figures: []routebill.Figure{{TeamID: teamA, AmountUSD: m("100.00")}}}},
		gw:         map[string]map[string]domain.Money{nous.ID: {teamA: m("90.00"), teamB: m("1.00")}}}
	j, ob := job(s)
	res, err := j.ReconcileMonth(t.Context(), day)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.NotReconciled, []string{"opencode"}) || len(res.Variances) != 2 || res.Events != 2 {
		t.Fatalf("result = %+v", res)
	}
	// teamB billed nothing for metered cost: over, with no percentage.
	if !math.IsNaN(res.Variances[1].Pct) || ob.Events[1].Detail.Data["variancePct"] != "" || ob.Events[0].Detail.Data["period"] != "2026-11" {
		t.Errorf("events = %+v", ob.Events)
	}
}

// TC-BL-02: a capacity route's day cost split by token share sums to the amount.
func TestAllocateCapacity(t *testing.T) {
	s := &store{caps: []routebill.Capacity{
		{Endpoint: vllm, TeamID: teamA, MonthlyUSD: mp("300.00"), Allocation: "by_tokens"},
		{Endpoint: vllm, TeamID: teamB, HourlyUSD: mp("1.00"), Allocation: "by_agent"},
		{Endpoint: vllm, TeamID: "team_idle", HourlyUSD: mp("0.50")},
	}, shares: map[string][]routebill.Share{
		vllm.ID + "/" + teamA: {{AgentID: "agt_1", Tokens: 100}, {AgentID: "agt_2", Tokens: 200}},
		vllm.ID + "/" + teamB: {{AgentID: "agt_3", Tokens: 1}, {PoolID: "pool_1", Tokens: 999}},
	}}
	j, _ := job(s)
	rows, err := j.AllocateCapacity(t.Context(), day)
	if err != nil {
		t.Fatal(err)
	}
	sum := map[string]int64{}
	for _, r := range rows {
		sum[r.TeamID] += r.AmountUSD.Micros()
	}
	// November has 30 days: 300 / 30 = 10.00 a day; 1.00/h = 24.00; 0.50/h = 12.00.
	if sum[teamA] != 10_000_000 || sum[teamB] != 24_000_000 || sum["team_idle"] != 12_000_000 || len(s.written) != 5 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].AmountUSD.String() != "3.333333" || rows[1].AmountUSD.String() != "6.666667" || rows[2].AmountUSD.String() != "12.00" {
		t.Errorf("shares = %v %v %v", rows[0].AmountUSD, rows[1].AmountUSD, rows[2].AmountUSD)
	}
	if rows[4].AgentID != "" || rows[4].ModelID != "vllm" {
		t.Errorf("idle team row = %+v", rows[4])
	}
	if got := routebill.Allocate(m("1.00"), nil); len(got) != 0 {
		t.Error("allocate with no weights")
	}
}

// TC-BL-03: one pending proposal per model whose listed price differs; none when equal.
func TestProposePrices(t *testing.T) {
	cur := [2]domain.Money{m("1.25"), m("10.00")}
	s := &store{models: []routebill.RouteModel{
		{Endpoint: openai, Model: "gpt-5", Current: &cur},
		{Endpoint: openai, Model: "gpt-5-mini", Current: &cur},
		{Endpoint: openai, Model: "gpt-5-nano", PendingID: "epp_old", Pending: [2]domain.Money{m("0.01"), m("0.02")}},
		{Endpoint: openai, Model: "o4", PendingID: "epp_same", Pending: [2]domain.Money{m("2.00"), m("8.00")}},
		{Endpoint: openai, Model: "unlisted"},
	}}
	j, _ := job(s)
	list := []routebill.ListedPrice{
		{Provider: "openai", Model: "gpt-5", Input: m("1.25"), Output: m("10.00")},
		{Model: "openai/gpt-5-mini", Input: m("0.25"), Output: m("2.00")},
		{Provider: "openai", Model: "GPT-5-nano", Input: m("0.05"), Output: m("0.40")},
		{Provider: "openai", Model: "o4", Input: m("2.00"), Output: m("8.00")},
	}
	p, sup, err := j.ProposePrices(t.Context(), list)
	if err != nil || p != 2 || sup != 1 || !slices.Equal(s.superseded, []string{"epp_old"}) {
		t.Fatalf("proposed %d superseded %d %v %v", p, sup, s.superseded, err)
	}
	if s.inserted[0].Model != "gpt-5-mini" || s.inserted[0].Input.String() != "0.25" || s.inserted[1].Model != "gpt-5-nano" {
		t.Errorf("inserted = %+v", s.inserted)
	}
	s.err = errX
	if _, _, err := j.ProposePrices(t.Context(), list); !errors.Is(err, errX) {
		t.Errorf("store error: %v", err)
	}
}

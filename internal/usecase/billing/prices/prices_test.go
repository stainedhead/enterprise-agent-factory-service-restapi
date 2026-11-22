package prices_test

import (
	"context"
	crand "crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/prices"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/prices/pricestest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const sonnet = "anthropic.claude-sonnet-4-5-v1:0"

var m = domain.MustMoney

func lp(product string, k prices.Kind, usd string) prices.ListPrice {
	return prices.ListPrice{Product: product, Kind: k, USDPerMtok: m(usd), EffectiveFrom: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)}
}

func sonnetList(in, out string) []prices.ListPrice {
	return []prices.ListPrice{lp("Claude Sonnet 4.5", prices.Input, in), lp("Claude Sonnet 4.5", prices.Output, out)}
}

func setup(st *pricestest.Store, list *pricestest.List) (*prices.Syncer, *fakes.Outbox) {
	ob := &fakes.Outbox{}
	return &prices.Syncer{List: list, Store: st, Tx: &fakes.TxRunner{}, Outbox: ob,
		Clock: &fakes.Clock{T: time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)}, IDs: &fakes.IDGen{},
		Session: port.SystemSession("afs-billing")}, ob
}

func cur(in, out, cr, cw string) *prices.Set {
	return &prices.Set{Input: m(in), Output: m(out), CacheRead: m(cr), CacheWrite: m(cw)}
}

func TestSync(t *testing.T) {
	pend := func(id string, s prices.Set) *prices.Pending { return &prices.Pending{ID: id, Prices: s} }
	for _, tc := range []struct {
		name                                 string
		enabled                              bool
		model                                prices.ModelState
		list                                 []prices.ListPrice
		proposed, superseded, unchanged, evs int
		wantIn                               string
		wantCache                            string
	}{
		{name: "changed price is proposed with the current cache prices kept", enabled: true,
			model: prices.ModelState{ID: sonnet, Current: cur("3.00", "15.00", "0.30", "3.75")},
			list:  sonnetList("3.30", "16.50"), proposed: 1, evs: 1, wantIn: "3.30", wantCache: "0.30"},
		{name: "unchanged price proposes nothing", enabled: true,
			model: prices.ModelState{ID: sonnet, Current: cur("3.00", "15.00", "0", "0")},
			list:  sonnetList("3.00", "15.00"), unchanged: 1},
		{name: "sync off reads nothing", enabled: false,
			model: prices.ModelState{ID: sonnet, Current: cur("3.00", "15.00", "0", "0")},
			list:  sonnetList("9.00", "9.00")},
		{name: "older pending is superseded by a newer proposal", enabled: true,
			model: prices.ModelState{ID: sonnet, Current: cur("3.00", "15.00", "0", "0"),
				Pending: pend("mpp_old", prices.Set{Input: m("3.30"), Output: m("16.50")})},
			list: sonnetList("3.60", "18.00"), proposed: 1, superseded: 1, evs: 1, wantIn: "3.60", wantCache: "0.00"},
		{name: "same pending proposal is not proposed again", enabled: true,
			model: prices.ModelState{ID: sonnet, Current: cur("3.00", "15.00", "0", "0"),
				Pending: pend("mpp_old", prices.Set{Input: m("3.30"), Output: m("16.50")})},
			list: sonnetList("3.30", "16.50"), unchanged: 1},
		{name: "price back to current retires the pending proposal", enabled: true,
			model: prices.ModelState{ID: sonnet, Current: cur("3.00", "15.00", "0", "0"),
				Pending: pend("mpp_old", prices.Set{Input: m("3.30"), Output: m("16.50")})},
			list: sonnetList("3.00", "15.00"), superseded: 1, unchanged: 1},
		{name: "a model without a price gets a first proposal", enabled: true,
			model: prices.ModelState{ID: sonnet}, list: append(sonnetList("3.00", "15.00"), lp("Claude Sonnet 4.5", prices.CacheRead, "0.30")),
			proposed: 1, evs: 1, wantIn: "3.00", wantCache: "0.30"},
		{name: "a list with input only proposes nothing", enabled: true,
			model: prices.ModelState{ID: sonnet, Current: cur("3.00", "15.00", "0", "0")},
			list:  []prices.ListPrice{lp("Claude Sonnet 4.5", prices.Input, "4.00")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &pricestest.Store{Enabled: tc.enabled, Models_: []prices.ModelState{tc.model}}
			list := &pricestest.List{Prices: tc.list}
			s, ob := setup(st, list)
			res, err := s.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if res.Proposed != tc.proposed || res.Superseded != tc.superseded || res.Unchanged != tc.unchanged {
				t.Fatalf("result = %+v", res)
			}
			if len(st.Inserted) != tc.proposed || len(st.Superseded) != tc.superseded || len(ob.Events) != tc.evs {
				t.Fatalf("inserted %d superseded %d events %d", len(st.Inserted), len(st.Superseded), len(ob.Events))
			}
			if !tc.enabled && (list.Calls != 0 || !res.Disabled) {
				t.Fatalf("a disabled sync read the list: calls %d, result %+v", list.Calls, res)
			}
			if tc.proposed == 1 {
				p := st.Inserted[0]
				if p.ModelID != sonnet || p.Prices.Input.String() != tc.wantIn || p.Prices.CacheRead.String() != tc.wantCache {
					t.Fatalf("proposal = %+v", p)
				}
				if !p.EffectiveFrom.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
					t.Fatalf("effective = %v", p.EffectiveFrom)
				}
				ev := ob.Events[0]
				if ev.Type != domain.EventPricingProposalCreated || ev.AggregateID != p.ID || ev.Detail.Data["modelId"] != sonnet {
					t.Fatalf("event = %+v", ev)
				}
			}
		})
	}
}

func TestSyncPublishesOneEventPerChangedModelAndNamesUnmatched(t *testing.T) {
	st := &pricestest.Store{Enabled: true, Models_: []prices.ModelState{
		{ID: sonnet, Current: cur("3.00", "15.00", "0", "0")},
		{ID: "amazon.nova-pro-v1:0", Current: cur("0.80", "3.20", "0", "0")},
	}}
	list := &pricestest.List{Prices: append(sonnetList("3.30", "16.50"),
		lp("Nova Pro", prices.Input, "0.90"), lp("Nova Pro", prices.Output, "3.60"),
		lp("Some Other Model", prices.Input, "1.00"))}
	s, ob := setup(st, list)
	res, err := s.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if res.Proposed != 2 || len(ob.Events) != 2 || len(res.Unmatched) != 1 || res.Unmatched[0] != "Some Other Model" {
		t.Fatalf("result = %+v, events %d", res, len(ob.Events))
	}
}

func TestSyncMatchesAnExactModelID(t *testing.T) {
	st := &pricestest.Store{Enabled: true, Models_: []prices.ModelState{{ID: sonnet}}}
	list := &pricestest.List{Prices: []prices.ListPrice{lp(sonnet, prices.Input, "3.00"), lp(sonnet, prices.Output, "15.00")}}
	s, _ := setup(st, list)
	if res, err := s.Run(t.Context()); err != nil || res.Proposed != 1 {
		t.Fatalf("result = %+v, err %v", res, err)
	}
}

func TestSyncFailures(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		st   *pricestest.Store
		list *pricestest.List
	}{
		{"setting unreadable", &pricestest.Store{EnabledErr: boom}, &pricestest.List{}},
		{"list fetch fails", &pricestest.Store{Enabled: true}, &pricestest.List{Err: boom}},
		{"insert fails", &pricestest.Store{Enabled: true, Models_: []prices.ModelState{{ID: sonnet}}, FailInsert: boom}, &pricestest.List{Prices: sonnetList("3", "15")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ob := setup(tc.st, tc.list)
			if _, err := s.Run(t.Context()); !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
			if len(ob.Events) != 0 && tc.name != "insert fails" {
				t.Fatalf("events = %d", len(ob.Events))
			}
		})
	}
}

type failIDs struct{ after int }

func (f *failIDs) NewID(p domain.IDPrefix) (domain.ID, error) {
	if f.after--; f.after < 0 {
		return "", errors.New("no ids")
	}
	return domain.NewID(p, time.Now(), crand.Reader)
}

type failOutbox struct{}

func (failOutbox) Publish(context.Context, port.Tx, domain.Event) error {
	return errors.New("outbox down")
}

// A proposal that cannot get an id, an event id or an outbox row fails the run.
func TestSyncProposeFailures(t *testing.T) {
	for name, mut := range map[string]func(*prices.Syncer){
		"proposal id": func(s *prices.Syncer) { s.IDs = &failIDs{after: 0} },
		"event id":    func(s *prices.Syncer) { s.IDs = &failIDs{after: 1} },
		"outbox":      func(s *prices.Syncer) { s.Outbox = failOutbox{} },
	} {
		t.Run(name, func(t *testing.T) {
			st := &pricestest.Store{Enabled: true, Models_: []prices.ModelState{{ID: sonnet, Current: cur("3.00", "15.00", "0", "0")}}}
			s, _ := setup(st, &pricestest.List{Prices: sonnetList("3.30", "16.50")})
			mut(s)
			if _, err := s.Run(t.Context()); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

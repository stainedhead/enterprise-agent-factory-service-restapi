// Package gatewaytest holds in-memory fakes of the gateway's ports, for the use case's tests and for the
// HTTP adapter's, so no test needs a database, a cluster or AWS.
package gatewaytest

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
)

// Directory is a gateway.PodDirectory.
type Directory struct {
	mu      sync.Mutex
	pods    map[netip.Addr]gateway.PodInfo
	IsReady bool
}

// NewDirectory returns a ready directory.
func NewDirectory() *Directory {
	return &Directory{pods: map[netip.Addr]gateway.PodInfo{}, IsReady: true}
}

// Put adds or replaces a pod.
func (d *Directory) Put(ip string, p gateway.PodInfo) {
	d.mu.Lock()
	d.pods[netip.MustParseAddr(ip)] = p
	d.mu.Unlock()
}

// Delete removes a pod.
func (d *Directory) Delete(ip string) {
	d.mu.Lock()
	delete(d.pods, netip.MustParseAddr(ip))
	d.mu.Unlock()
}

// Lookup implements gateway.PodDirectory.
func (d *Directory) Lookup(ip netip.Addr) (gateway.PodInfo, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.pods[ip]
	return p, ok
}

// Ready implements gateway.PodDirectory.
func (d *Directory) Ready() bool { return d.IsReady }

// Tokens is a gateway.TokenReviewer: token -> identity.
type Tokens struct {
	IDs map[string]gateway.TokenIdentity
	Err error
}

// Review implements gateway.TokenReviewer.
func (t *Tokens) Review(_ context.Context, token string) (gateway.TokenIdentity, error) {
	if t.Err != nil {
		return gateway.TokenIdentity{}, t.Err
	}
	id, ok := t.IDs[token]
	if !ok {
		return gateway.TokenIdentity{}, gateway.ErrTokenInvalid
	}
	return id, nil
}

// Store is a gateway.Store.
type Store struct {
	mu     sync.Mutex
	Teams  map[string]gateway.TeamRecord
	Models map[string]gateway.ModelAccess // team + "\x00" + model
	// Admission holds the reason each model is refused for a subject, keyed "agent\x00<id>" or "pool\x00<id>"
	// (an empty reason admits it). A subject without an entry admits every model.
	Admission map[string]map[string]string
	PriceRows map[string][]gateway.Price
	Budgets   map[string]gateway.BudgetRecord
	TagRows   map[string][]gateway.Tag
	TagSets   map[string]string
	// Err fails every call with it; Block makes calls wait for their context.
	Err   error
	Block bool
	// Reads counts calls per method.
	Reads map[string]int
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{Teams: map[string]gateway.TeamRecord{}, Models: map[string]gateway.ModelAccess{}, Admission: map[string]map[string]string{}, PriceRows: map[string][]gateway.Price{},
		Budgets: map[string]gateway.BudgetRecord{}, TagRows: map[string][]gateway.Tag{}, TagSets: map[string]string{}, Reads: map[string]int{}}
}

func (s *Store) enter(ctx context.Context, name string) error {
	s.mu.Lock()
	s.Reads[name]++
	err, block := s.Err, s.Block
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

// Team implements gateway.Store.
func (s *Store) Team(ctx context.Context, id string) (gateway.TeamRecord, error) {
	if err := s.enter(ctx, "team"); err != nil {
		return gateway.TeamRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.Teams[id]
	if !ok {
		return gateway.TeamRecord{}, gateway.ErrNotFound
	}
	return t, nil
}

// ModelAdmission implements gateway.Store.
func (s *Store) ModelAdmission(ctx context.Context, kind, subjectID string) (map[string]string, error) {
	if err := s.enter(ctx, "modelAdmission"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.Admission[kind+"\x00"+subjectID]
	if !ok {
		// Every model the fake knows of, admitted.
		all := map[string]string{}
		for k := range s.Models {
			if i := strings.IndexByte(k, 0); i >= 0 {
				all[k[i+1:]] = ""
			}
		}
		for id := range s.PriceRows {
			all[id] = ""
		}
		return all, nil
	}
	return m, nil
}

// TeamModel implements gateway.Store.
func (s *Store) TeamModel(ctx context.Context, teamID, modelID string) (gateway.ModelAccess, error) {
	if err := s.enter(ctx, "teamModel"); err != nil {
		return gateway.ModelAccess{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.Models[teamID+"\x00"+modelID]
	if !ok {
		return gateway.ModelAccess{}, gateway.ErrNotFound
	}
	return m, nil
}

// Prices implements gateway.Store.
func (s *Store) Prices(ctx context.Context, modelID string) ([]gateway.Price, error) {
	if err := s.enter(ctx, "prices"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.PriceRows[modelID], nil
}

// Budget implements gateway.Store.
func (s *Store) Budget(ctx context.Context, teamID string) (gateway.BudgetRecord, error) {
	if err := s.enter(ctx, "budget"); err != nil {
		return gateway.BudgetRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Budgets[teamID]
	if !ok {
		return gateway.BudgetRecord{}, gateway.ErrNotFound
	}
	return b, nil
}

// Tags implements gateway.Store.
func (s *Store) Tags(ctx context.Context, teamID string) ([]gateway.Tag, error) {
	if err := s.enter(ctx, "tags"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.TagRows[teamID], nil
}

// TagSetID implements gateway.Store.
func (s *Store) TagSetID(ctx context.Context, teamID string) (string, error) {
	if err := s.enter(ctx, "tagSet"); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.TagSets[teamID]; ok {
		return id, nil
	}
	return "ts_default", nil
}

// Settlement records one Settle call.
type Settlement struct {
	HoldID, BudgetID string
	Period           time.Time
	Actual           domain.Money
}

// Spend is a gateway.Spend with the semantics of the database functions.
type Spend struct {
	mu          sync.Mutex
	Limit       domain.Money
	Spent       domain.Money
	Reserved    domain.Money
	holds       map[string]domain.Money
	seq         int
	Reservs     []domain.Money
	Settlements []Settlement
	Ensures     int
	// Err fails Reserve and EnsureCounter; SettleErr fails Settle; Block makes Reserve wait for ctx.
	Err, SettleErr error
	Block          bool
	// Holders records the holder of every reservation.
	Holders []string
}

// NewSpend returns a counter with the given limit and nothing spent.
func NewSpend(limit domain.Money) *Spend {
	return &Spend{Limit: limit, holds: map[string]domain.Money{}}
}

// EnsureCounter implements gateway.Spend.
func (s *Spend) EnsureCounter(_ context.Context, _ string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Ensures++
	return s.Err
}

// Reserve implements gateway.Spend.
func (s *Spend) Reserve(ctx context.Context, _ string, _ time.Time, amount domain.Money, holder string) (gateway.Hold, error) {
	s.mu.Lock()
	block, err := s.Block, s.Err
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return gateway.Hold{}, ctx.Err()
	}
	if err != nil {
		return gateway.Hold{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := gateway.Hold{Limit: s.Limit, Spent: s.Spent, Reserved: s.Reserved}
	if s.Spent.Add(s.Reserved).Add(amount).Cmp(s.Limit) > 0 {
		return h, nil
	}
	s.seq++
	id := fmt.Sprintf("hold-%d", s.seq)
	s.holds[id] = amount
	s.Reserved = s.Reserved.Add(amount)
	s.Reservs = append(s.Reservs, amount)
	s.Holders = append(s.Holders, holder)
	h.ID = id
	return h, nil
}

// Settle implements gateway.Spend.
func (s *Spend) Settle(_ context.Context, holdID, budgetID string, period time.Time, actual domain.Money) (gateway.Counter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SettleErr != nil {
		return gateway.Counter{}, s.SettleErr
	}
	s.Settlements = append(s.Settlements, Settlement{HoldID: holdID, BudgetID: budgetID, Period: period, Actual: actual})
	if amt, ok := s.holds[holdID]; ok {
		delete(s.holds, holdID)
		s.Reserved = s.Reserved.Sub(amt)
	}
	s.Spent = s.Spent.Add(actual)
	return gateway.Counter{Limit: s.Limit, Spent: s.Spent, Reserved: s.Reserved}, nil
}

// Snapshot returns the counter.
func (s *Spend) Snapshot() gateway.Counter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return gateway.Counter{Limit: s.Limit, Spent: s.Spent, Reserved: s.Reserved}
}

// Held returns the number of live holds.
func (s *Spend) Held() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.holds) }

// Thresholds is a gateway.Thresholds that inserts once per (budget, period, threshold).
type Thresholds struct {
	mu     sync.Mutex
	seen   map[string]bool
	Events []gateway.Crossing
	Err    error
}

// NewThresholds returns an empty recorder.
func NewThresholds() *Thresholds { return &Thresholds{seen: map[string]bool{}} }

// Record implements gateway.Thresholds.
func (t *Thresholds) Record(_ context.Context, c gateway.Crossing) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Err != nil {
		return false, t.Err
	}
	key := c.BudgetID + c.Period.String() + strconv.Itoa(c.Threshold)
	if t.seen[key] {
		return false, nil
	}
	t.seen[key] = true
	t.Events = append(t.Events, c)
	return true, nil
}

// Count returns how many events of the type were recorded.
func (t *Thresholds) Count(typ domain.EventType) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, e := range t.Events {
		if e.Event == typ {
			n++
		}
	}
	return n
}

// Usage is a gateway.UsageSink that keeps what it gets.
type Usage struct {
	mu   sync.Mutex
	Rows []gateway.Record
}

// Record implements gateway.UsageSink.
func (u *Usage) Record(r gateway.Record) { u.mu.Lock(); u.Rows = append(u.Rows, r); u.mu.Unlock() }

// All returns the rows.
func (u *Usage) All() []gateway.Record {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]gateway.Record(nil), u.Rows...)
}

// ErrDB is a generic database failure for injection.
var ErrDB = errors.New("connection refused")

// Settled returns a copy of the settlements so far, safe to call while calls are in flight.
func (s *Spend) Settled() []Settlement {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Settlement(nil), s.Settlements...)
}

// Routes is an in-memory gateway.RouteStore.
type Routes struct {
	mu        sync.Mutex
	Grants    map[string][]gateway.RouteGrant // "agent/<id>" or "pool/<id>"
	Endpoints map[string]gateway.RouteEndpoint
	PriceRows map[string][]gateway.RoutePrice // "<endpoint id>/<model>"
	Err       error
}

// NewRoutes returns an empty Routes.
func NewRoutes() *Routes {
	return &Routes{Grants: map[string][]gateway.RouteGrant{}, Endpoints: map[string]gateway.RouteEndpoint{}, PriceRows: map[string][]gateway.RoutePrice{}}
}

// RouteAdmission implements gateway.RouteStore.
func (r *Routes) RouteAdmission(_ context.Context, kind, id string) ([]gateway.RouteGrant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Err != nil {
		return nil, r.Err
	}
	g, ok := r.Grants[kind+"/"+id]
	if !ok {
		return nil, gateway.ErrNotFound
	}
	return g, nil
}

// RouteEndpoint implements gateway.RouteStore.
func (r *Routes) RouteEndpoint(_ context.Context, _, slug string) (gateway.RouteEndpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.Endpoints[slug]
	if !ok {
		return e, gateway.ErrNotFound
	}
	return e, nil
}

// RoutePrices implements gateway.RouteStore.
func (r *Routes) RoutePrices(_ context.Context, id, model string) ([]gateway.RoutePrice, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.PriceRows[id+"/"+model], nil
}

// Auth is a gateway.UpstreamAuth with fixed answers per route slug.
type Auth struct {
	Creds map[string]gateway.UpstreamCredential
	Errs  map[string]error
}

// Credential implements gateway.UpstreamAuth.
func (a *Auth) Credential(_ context.Context, ep gateway.RouteEndpoint, _ string) (gateway.UpstreamCredential, error) {
	if err := a.Errs[ep.Slug]; err != nil {
		return gateway.UpstreamCredential{}, err
	}
	return a.Creds[ep.Slug], nil
}

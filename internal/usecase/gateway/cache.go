package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// DefaultCacheTTL bounds how stale the gateway's view of desired state can be; the change feed
// invalidates sooner (architecture A-8).
const DefaultCacheTTL = 60 * time.Second

type entry[V any] struct {
	v   V
	err error
	exp time.Time
}

type flight[V any] struct {
	done chan struct{}
	v    V
	err  error
}

// ttlCache is a small TTL cache with one load in flight per key.
type ttlCache[V any] struct {
	mu     sync.Mutex
	m      map[string]entry[V]
	flying map[string]*flight[V]
	ttl    time.Duration
	// negTTL is how long ErrNotFound is remembered.
	negTTL time.Duration
	now    func() time.Time
}

func newTTLCache[V any](ttl time.Duration, now func() time.Time) *ttlCache[V] {
	return &ttlCache[V]{m: map[string]entry[V]{}, flying: map[string]*flight[V]{}, ttl: ttl, negTTL: ttl / 6, now: now}
}

func (c *ttlCache[V]) get(key string, load func() (V, error)) (V, error) {
	c.mu.Lock()
	if e, ok := c.m[key]; ok && c.now().Before(e.exp) {
		c.mu.Unlock()
		return e.v, e.err
	}
	if f, ok := c.flying[key]; ok {
		c.mu.Unlock()
		<-f.done
		return f.v, f.err
	}
	f := &flight[V]{done: make(chan struct{})}
	c.flying[key] = f
	c.mu.Unlock()

	f.v, f.err = load()

	c.mu.Lock()
	delete(c.flying, key)
	switch {
	case f.err == nil:
		c.m[key] = entry[V]{v: f.v, exp: c.now().Add(c.ttl)}
	case errors.Is(f.err, ErrNotFound):
		c.m[key] = entry[V]{err: f.err, exp: c.now().Add(c.negTTL)}
	}
	c.mu.Unlock()
	close(f.done)
	return f.v, f.err
}

func (c *ttlCache[V]) invalidate(match func(key string) bool) {
	c.mu.Lock()
	for k := range c.m {
		if match(k) {
			delete(c.m, k)
		}
	}
	c.mu.Unlock()
}

// CachedStore is a Store that remembers answers for the TTL and forgets them on a change
// notification (LISTEN afs_desired_state, M3-B7). Errors other than ErrNotFound are never cached.
type CachedStore struct {
	inner  Store
	teams  *ttlCache[TeamRecord]
	models *ttlCache[ModelAccess]
	admits *ttlCache[map[string]string]
	prices *ttlCache[[]Price]
	budget *ttlCache[BudgetRecord]
	tags   *ttlCache[[]Tag]
	tagSet *ttlCache[string]
}

// NewCachedStore wraps inner. now is time.Now when nil; ttl is DefaultCacheTTL when zero.
func NewCachedStore(inner Store, ttl time.Duration, now func() time.Time) *CachedStore {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &CachedStore{inner: inner,
		teams: newTTLCache[TeamRecord](ttl, now), models: newTTLCache[ModelAccess](ttl, now), admits: newTTLCache[map[string]string](ttl, now),
		prices: newTTLCache[[]Price](ttl, now), budget: newTTLCache[BudgetRecord](ttl, now),
		tags: newTTLCache[[]Tag](ttl, now), tagSet: newTTLCache[string](ttl, now)}
}

// Team implements Store.
func (c *CachedStore) Team(ctx context.Context, id string) (TeamRecord, error) {
	return c.teams.get(id, func() (TeamRecord, error) { return c.inner.Team(ctx, id) })
}

// TeamModel implements Store.
func (c *CachedStore) TeamModel(ctx context.Context, teamID, modelID string) (ModelAccess, error) {
	return c.models.get(teamID+"\x00"+modelID, func() (ModelAccess, error) { return c.inner.TeamModel(ctx, teamID, modelID) })
}

// ModelAdmission implements Store: one answer per agent or pool, kept for the TTL and dropped when the
// change feed reports a change to models, prices, access lists or settings.
func (c *CachedStore) ModelAdmission(ctx context.Context, kind, subjectID string) (map[string]string, error) {
	return c.admits.get(kind+"\x00"+subjectID, func() (map[string]string, error) { return c.inner.ModelAdmission(ctx, kind, subjectID) })
}

// Prices implements Store.
func (c *CachedStore) Prices(ctx context.Context, modelID string) ([]Price, error) {
	return c.prices.get(modelID, func() ([]Price, error) { return c.inner.Prices(ctx, modelID) })
}

// Budget implements Store.
func (c *CachedStore) Budget(ctx context.Context, teamID string) (BudgetRecord, error) {
	return c.budget.get(teamID, func() (BudgetRecord, error) { return c.inner.Budget(ctx, teamID) })
}

// Tags implements Store.
func (c *CachedStore) Tags(ctx context.Context, teamID string) ([]Tag, error) {
	return c.tags.get(teamID, func() ([]Tag, error) { return c.inner.Tags(ctx, teamID) })
}

// TagSetID implements Store.
func (c *CachedStore) TagSetID(ctx context.Context, teamID string) (string, error) {
	return c.tagSet.get(teamID, func() (string, error) { return c.inner.TagSetID(ctx, teamID) })
}

var _ Store = (*CachedStore)(nil)

// Invalidate forgets what a change notification (table, team id; the team is empty for service-level
// tables) may have changed. Unknown tables are ignored.
func (c *CachedStore) Invalidate(table, teamID string) {
	forTeam := func(key string) bool { return teamID == "" || key == teamID }
	forTeamPrefix := func(key string) bool { return teamID == "" || strings.HasPrefix(key, teamID+"\x00") }
	switch table {
	case "teams":
		c.teams.invalidate(forTeam)
		c.admits.invalidate(func(string) bool { return true })
	case "team_models", "team_inference_profiles":
		c.models.invalidate(forTeamPrefix)
		c.admits.invalidate(func(string) bool { return true })
	case "models":
		c.models.invalidate(func(string) bool { return true })
		c.admits.invalidate(func(string) bool { return true })
	case "model_prices":
		c.prices.invalidate(func(string) bool { return true })
		c.admits.invalidate(func(string) bool { return true })
	case "agent_models", "pool_models", "agents", "agent_pools", "service_settings", "inference_providers":
		c.admits.invalidate(func(string) bool { return true })
	case "budgets":
		c.budget.invalidate(forTeam)
	case "team_tags", "service_tags", "tag_sets":
		c.tags.invalidate(forTeam)
		c.tagSet.invalidate(forTeam)
	}
}

// InvalidateAll forgets everything, after the change feed reconnects and may have missed events.
func (c *CachedStore) InvalidateAll() {
	for _, t := range []string{"teams", "team_models", "team_inference_profiles", "models", "model_prices", "agent_models", "pool_models", "agents", "agent_pools", "service_settings", "budgets", "team_tags"} {
		c.Invalidate(t, "")
	}
}

// RouteTables are the change-feed tables of migration 000023 that change a route answer.
var RouteTables = []string{"inference_providers", "model_endpoints", "endpoint_models", "endpoint_model_prices",
	"endpoint_credentials", "endpoint_federations", "endpoint_capacity_costs", "team_model_endpoints",
	"agent_model_endpoints", "pool_model_endpoints"}

// CachedRoutes is a RouteStore that remembers answers for at most the TTL (60 s) and forgets them on a
// change notification for any route table, or for agents, pools and teams (gateway contract §12.3).
type CachedRoutes struct {
	inner     RouteStore
	admission *ttlCache[[]RouteGrant]
	endpoints *ttlCache[RouteEndpoint]
	prices    *ttlCache[[]RoutePrice]
}

// NewCachedRoutes wraps inner.
func NewCachedRoutes(inner RouteStore, ttl time.Duration, now func() time.Time) *CachedRoutes {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &CachedRoutes{inner: inner, admission: newTTLCache[[]RouteGrant](ttl, now),
		endpoints: newTTLCache[RouteEndpoint](ttl, now), prices: newTTLCache[[]RoutePrice](ttl, now)}
}

var _ RouteStore = (*CachedRoutes)(nil)

// RouteAdmission implements RouteStore.
func (c *CachedRoutes) RouteAdmission(ctx context.Context, kind, id string) ([]RouteGrant, error) {
	return c.admission.get(kind+"\x00"+id, func() ([]RouteGrant, error) { return c.inner.RouteAdmission(ctx, kind, id) })
}

// RouteEndpoint implements RouteStore.
func (c *CachedRoutes) RouteEndpoint(ctx context.Context, teamID, slug string) (RouteEndpoint, error) {
	return c.endpoints.get(teamID+"\x00"+slug, func() (RouteEndpoint, error) { return c.inner.RouteEndpoint(ctx, teamID, slug) })
}

// RoutePrices implements RouteStore.
func (c *CachedRoutes) RoutePrices(ctx context.Context, endpointID, model string) ([]RoutePrice, error) {
	return c.prices.get(endpointID+"\x00"+model, func() ([]RoutePrice, error) { return c.inner.RoutePrices(ctx, endpointID, model) })
}

// Invalidate forgets what a change on table may have changed. Route answers are few and cheap to load,
// so any route-relevant change drops them all.
func (c *CachedRoutes) Invalidate(table, _ string) {
	all := func(string) bool { return true }
	switch table {
	case "endpoint_model_prices":
		c.prices.invalidate(all)
		c.admission.invalidate(all)
	case "agents", "agent_pools", "teams":
		c.admission.invalidate(all)
	default:
		for _, t := range RouteTables {
			if t == table {
				c.admission.invalidate(all)
				c.endpoints.invalidate(all)
				c.prices.invalidate(all)
			}
		}
	}
}

// InvalidateAll forgets everything.
func (c *CachedRoutes) InvalidateAll() { c.Invalidate("model_endpoints", "") }

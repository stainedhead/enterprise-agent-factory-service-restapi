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
		teams: newTTLCache[TeamRecord](ttl, now), models: newTTLCache[ModelAccess](ttl, now),
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
	case "team_models":
		c.models.invalidate(forTeamPrefix)
	case "models":
		c.models.invalidate(func(string) bool { return true })
	case "model_prices":
		c.prices.invalidate(func(string) bool { return true })
	case "budgets":
		c.budget.invalidate(forTeam)
	case "team_tags", "service_tags", "tag_sets":
		c.tags.invalidate(forTeam)
		c.tagSet.invalidate(forTeam)
	}
}

// InvalidateAll forgets everything, after the change feed reconnects and may have missed events.
func (c *CachedStore) InvalidateAll() {
	for _, t := range []string{"teams", "team_models", "models", "model_prices", "budgets", "team_tags"} {
		c.Invalidate(t, "")
	}
}

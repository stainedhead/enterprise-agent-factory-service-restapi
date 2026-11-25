package mcpup

import (
	"context"
	"sync"
	"time"
)

// CachedTargets keeps targets for at most TTL and drops them on a change to mcp_servers.
type CachedTargets struct {
	Next Targets
	TTL  time.Duration
	Now  func() time.Time

	mu sync.Mutex
	m  map[string]cachedTarget
}

type cachedTarget struct {
	t  Target
	at time.Time
}

// Target implements Targets.
func (c *CachedTargets) Target(ctx context.Context, id string) (Target, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	c.mu.Lock()
	e, ok := c.m[id]
	c.mu.Unlock()
	if ok && now().Sub(e.at) < ttl {
		return e.t, nil
	}
	t, err := c.Next.Target(ctx, id)
	if err != nil {
		return Target{}, err
	}
	c.mu.Lock()
	if c.m == nil || len(c.m) > 5000 {
		c.m = map[string]cachedTarget{}
	}
	c.m[id] = cachedTarget{t: t, at: now()}
	c.mu.Unlock()
	return t, nil
}

// InvalidateAll drops every target.
func (c *CachedTargets) InvalidateAll() {
	c.mu.Lock()
	c.m = nil
	c.mu.Unlock()
}

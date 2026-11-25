package mcpgateway

import (
	"context"
	"sync"
	"time"
)

// CachedAdmission keeps each caller's snapshot for at most TTL (gateway contract §13.4: at most 60 s), and drops
// every snapshot when the change feed reports a change to an MCP table, so a block or an approval is seen within a
// second. A failed read is not cached: admission fails closed on every call until the database answers.
type CachedAdmission struct {
	next Admission
	ttl  time.Duration
	now  func() time.Time

	mu      sync.Mutex
	entries map[string]cached
}

type cached struct {
	snap *Snapshot
	at   time.Time
}

// NewCachedAdmission wraps next; ttl is capped at 60 s.
func NewCachedAdmission(next Admission, ttl time.Duration, now func() time.Time) *CachedAdmission {
	if ttl <= 0 || ttl > time.Minute {
		ttl = time.Minute
	}
	if now == nil {
		now = time.Now
	}
	return &CachedAdmission{next: next, ttl: ttl, now: now, entries: map[string]cached{}}
}

// Snapshot implements Admission.
func (a *CachedAdmission) Snapshot(ctx context.Context, c Caller) (*Snapshot, error) {
	key := c.SubjectKind + "/" + c.SubjectID
	a.mu.Lock()
	e, ok := a.entries[key]
	a.mu.Unlock()
	if ok && a.now().Sub(e.at) < a.ttl {
		return e.snap, nil
	}
	snap, err := a.next.Snapshot(ctx, c)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if len(a.entries) > 10000 {
		a.entries = map[string]cached{}
	}
	a.entries[key] = cached{snap: snap, at: a.now()}
	a.mu.Unlock()
	return snap, nil
}

// McpTables are the tables whose change invalidates admission (migration 000024).
var McpTables = map[string]bool{
	"mcp_servers": true, "mcp_hosted_specs": true, "mcp_primitives": true, "team_mcp_servers": true, "agent_mcp_servers": true,
	"pool_mcp_servers": true, "mcp_policies": true, "mcp_credentials": true, "agents": true, "agent_pools": true, "service_settings": true,
}

// Invalidate drops every snapshot when table is one that admission reads.
func (a *CachedAdmission) Invalidate(table string) {
	if McpTables[table] {
		a.InvalidateAll()
	}
}

// InvalidateAll drops every snapshot.
func (a *CachedAdmission) InvalidateAll() {
	a.mu.Lock()
	a.entries = map[string]cached{}
	a.mu.Unlock()
}

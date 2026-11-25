package mcpup

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpinventory"
)

// RefreshTarget is a server the team gateway may refresh.
type RefreshTarget struct {
	Target
	// Requested is set when someone asked for a refresh (refresh_requested_at).
	Requested bool
	// LastSeen is the newest last_seen of the server's primitives, shared by every gateway that refreshes it.
	LastSeen time.Time
}

// RefreshStore is the refresh's view of the database (postgres.McpGatewayStore).
type RefreshStore interface {
	McpRefreshTargets(ctx context.Context, teamID string) ([]RefreshTarget, error)
	McpRefreshInterval(ctx context.Context) (time.Duration, error)
	McpStored(ctx context.Context, serverID string) ([]mcpinventory.Stored, error)
	ApplyMcpPlan(ctx context.Context, t Target, plan mcpinventory.Plan, protocolVersion string) error
}

// Refresher keeps the inventories of the team's servers current (gateway contract §13.6): on request, and at most
// every mcp.refresh_interval_minutes otherwise. Every replica runs it; the store's lease lets one apply a refresh
// at a time, and LastSeen is shared, so a server refreshed by one replica or team is not listed again by the next.
type Refresher struct {
	Store    RefreshStore
	Upstream *Upstream
	TeamID   string
	// Tick is how often due servers are looked for; default 30 s.
	Tick time.Duration
	// Retry is the least time between two attempts at one server; default 1 minute.
	Retry time.Duration
	Log   *slog.Logger
	Now   func() time.Time

	mu       sync.Mutex
	attempts map[string]time.Time
}

// Run refreshes until ctx ends.
func (r *Refresher) Run(ctx context.Context) error {
	tick := r.Tick
	if tick <= 0 {
		tick = 30 * time.Second
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		r.Once(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (r *Refresher) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Refresher) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Once refreshes every due server and returns how many it stored.
func (r *Refresher) Once(ctx context.Context) int {
	interval, err := r.Store.McpRefreshInterval(ctx)
	if err != nil || interval <= 0 {
		interval = 15 * time.Minute
	}
	targets, err := r.Store.McpRefreshTargets(ctx, r.TeamID)
	if err != nil {
		r.log().Warn("mcp refresh: list servers", "error", err.Error())
		return 0
	}
	retry := r.Retry
	if retry <= 0 {
		retry = time.Minute
	}
	slugs := make([]string, 0, len(targets))
	for _, t := range targets {
		slugs = append(slugs, t.Slug)
	}
	now := r.now()
	done := 0
	r.mu.Lock()
	if r.attempts == nil {
		r.attempts = map[string]time.Time{}
	}
	r.mu.Unlock()
	for _, t := range targets {
		r.mu.Lock()
		last := r.attempts[t.ID]
		r.mu.Unlock()
		due := t.Requested || now.Sub(t.LastSeen) >= interval
		if !due || !last.IsZero() && now.Sub(last) < retry {
			continue
		}
		r.mu.Lock()
		r.attempts[t.ID] = now
		r.mu.Unlock()
		others := make([]string, 0, len(slugs))
		for _, s := range slugs {
			if s != t.Slug {
				others = append(others, s)
			}
		}
		if err := r.refresh(ctx, t, others); err != nil {
			r.log().Warn("mcp refresh failed", "server", t.Slug, "error", err.Error())
			continue
		}
		done++
	}
	return done
}

func (r *Refresher) refresh(ctx context.Context, t RefreshTarget, others []string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	l, err := r.Upstream.Lister(ctx, t.Target)
	if err != nil {
		return err
	}
	listed, truncated, err := mcpinventory.Fetch(ctx, l)
	if err != nil {
		return err
	}
	stored, err := r.Store.McpStored(ctx, t.ID)
	if err != nil {
		return err
	}
	plan, err := mcpinventory.Build(mcpinventory.Server{ID: t.ID, Slug: t.Slug, DriftPolicy: t.DriftPolicy, OtherServers: others}, stored, listed, truncated)
	if err != nil {
		return err
	}
	if err := r.Store.ApplyMcpPlan(ctx, t.Target, plan, version(t.Target)); err != nil && !errors.Is(err, ErrBusy) {
		return err
	}
	if len(plan.Findings) > 0 {
		r.log().Info("mcp refresh: definition findings held for review", "server", t.Slug, "findings", len(plan.Findings))
	}
	return nil
}

// ErrBusy is what a store returns when another replica holds the server's refresh; the refresh is skipped.
var ErrBusy = errors.New("mcpup: refresh held by another replica")

package mcpup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/mcpwire"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpinventory"
)

type fakeRefreshStore struct {
	targets  []RefreshTarget
	listErr  error
	applied  map[string]mcpinventory.Plan
	applyErr error
}

func (f *fakeRefreshStore) McpRefreshTargets(context.Context, string) ([]RefreshTarget, error) {
	return f.targets, f.listErr
}

func (f *fakeRefreshStore) McpRefreshInterval(context.Context) (time.Duration, error) {
	return 15 * time.Minute, nil
}

func (f *fakeRefreshStore) McpStored(context.Context, string) ([]mcpinventory.Stored, error) {
	return nil, nil
}

func (f *fakeRefreshStore) ApplyMcpPlan(_ context.Context, t Target, p mcpinventory.Plan, _ string) error {
	if f.applied == nil {
		f.applied = map[string]mcpinventory.Plan{}
	}
	f.applied[t.ID] = p
	return f.applyErr
}

func TestRefresher(t *testing.T) {
	r := newRig(t)
	r.target(t, "inv", domain.McpAuthNone, mcpwire.Version20260728)
	now := time.Unix(1_000_000, 0)
	st := &fakeRefreshStore{targets: []RefreshTarget{
		{Target: r.ts["mcp_inv"], LastSeen: now.Add(-time.Hour)},                                      // due by interval
		{Target: Target{ID: "mcp_fresh", Slug: "fresh"}, LastSeen: now},                               // not due
		{Target: Target{ID: "mcp_bad", Slug: "bad", AuthKind: domain.McpAuthStatic}, Requested: true}, // fails: no credential
	}}
	rf := &Refresher{Store: st, Upstream: r.u, TeamID: "team_1", Now: func() time.Time { return now }}
	if n := rf.Once(t.Context()); n != 1 {
		t.Fatalf("refreshed %d, want 1", n)
	}
	if p := st.applied["mcp_inv"]; len(p.Items) != 2 || p.Items[0].Action != mcpinventory.ActionInsertHeld {
		t.Errorf("plan = %+v", p)
	}
	// Within the retry window nothing is tried again.
	if n := rf.Once(t.Context()); n != 0 {
		t.Errorf("retried at once: %d", n)
	}
	st.applyErr = ErrBusy
	now = now.Add(2 * time.Minute)
	if n := rf.Once(t.Context()); n != 1 {
		t.Errorf("a busy lease is not a failure: %d", n)
	}
	st.listErr = errors.New("db down")
	if n := rf.Once(t.Context()); n != 0 {
		t.Error("refreshed without a list")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := (&Refresher{Store: st, Upstream: r.u, Tick: time.Millisecond}).Run(ctx); err != nil {
		t.Error(err)
	}
}

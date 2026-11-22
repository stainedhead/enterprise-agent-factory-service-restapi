package gateway_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway/gatewaytest"
)

type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(0, c.t.Load()) }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d)) }

func newCached(t *testing.T) (*gateway.CachedStore, *gatewaytest.Store, *clock) {
	t.Helper()
	s := newRig(t).store
	c := &clock{}
	c.t.Store(time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC).UnixNano())
	return gateway.NewCachedStore(s, time.Minute, c.now), s, c
}

func TestCachedStoreServesFromCacheUntilTheTTLEnds(t *testing.T) {
	cs, s, clk := newCached(t)
	ctx := t.Context()
	for range 3 {
		if _, err := cs.Team(ctx, teamA); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.TeamModel(ctx, teamA, modelID); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.Prices(ctx, modelID); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.Budget(ctx, teamA); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.Tags(ctx, teamA); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.TagSetID(ctx, teamA); err != nil {
			t.Fatal(err)
		}
	}
	for name, n := range s.Reads {
		if n != 1 {
			t.Errorf("%s read %d times, want 1", name, n)
		}
	}
	clk.advance(61 * time.Second)
	if _, err := cs.Team(ctx, teamA); err != nil || s.Reads["team"] != 2 {
		t.Fatalf("after the TTL: reads %d err %v", s.Reads["team"], err)
	}
}

func TestCachedStoreForgetsOnNotification(t *testing.T) {
	cs, s, _ := newCached(t)
	ctx := t.Context()
	_, _ = cs.Team(ctx, teamA)
	_, _ = cs.Team(ctx, teamB)
	_, _ = cs.TeamModel(ctx, teamA, modelID)
	_, _ = cs.TeamModel(ctx, teamB, modelID)
	_, _ = cs.Budget(ctx, teamA)
	_, _ = cs.Prices(ctx, modelID)
	_, _ = cs.Tags(ctx, teamA)
	_, _ = cs.TagSetID(ctx, teamA)

	cs.Invalidate("teams", teamA)
	_, _ = cs.Team(ctx, teamA)
	_, _ = cs.Team(ctx, teamB)
	if s.Reads["team"] != 3 {
		t.Fatalf("team reads = %d: only team A should be reloaded", s.Reads["team"])
	}
	cs.Invalidate("team_models", teamA)
	_, _ = cs.TeamModel(ctx, teamA, modelID)
	_, _ = cs.TeamModel(ctx, teamB, modelID)
	if s.Reads["teamModel"] != 3 {
		t.Fatalf("teamModel reads = %d", s.Reads["teamModel"])
	}
	cs.Invalidate("models", "")
	_, _ = cs.TeamModel(ctx, teamB, modelID)
	if s.Reads["teamModel"] != 4 {
		t.Fatalf("a model change must clear every team: reads %d", s.Reads["teamModel"])
	}
	cs.Invalidate("budgets", teamA)
	cs.Invalidate("model_prices", "")
	cs.Invalidate("team_tags", teamA)
	cs.Invalidate("unrelated_table", "")
	_, _ = cs.Budget(ctx, teamA)
	_, _ = cs.Prices(ctx, modelID)
	_, _ = cs.Tags(ctx, teamA)
	_, _ = cs.TagSetID(ctx, teamA)
	for _, k := range []string{"budget", "prices", "tags", "tagSet"} {
		if s.Reads[k] != 2 {
			t.Errorf("%s reads = %d, want 2", k, s.Reads[k])
		}
	}
	cs.InvalidateAll()
	_, _ = cs.Team(ctx, teamA)
	if s.Reads["team"] != 4 {
		t.Fatalf("InvalidateAll: reads %d", s.Reads["team"])
	}
}

func TestCachedStoreCachesNotFoundBrieflyAndErrorsNever(t *testing.T) {
	cs, s, clk := newCached(t)
	ctx := t.Context()
	for range 3 {
		if _, err := cs.Budget(ctx, "team_missing"); !errors.Is(err, gateway.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if s.Reads["budget"] != 1 {
		t.Fatalf("not found reads = %d", s.Reads["budget"])
	}
	clk.advance(11 * time.Second)
	_, _ = cs.Budget(ctx, "team_missing")
	if s.Reads["budget"] != 2 {
		t.Fatalf("a remembered absence must expire quickly: reads %d", s.Reads["budget"])
	}
	s.Err = gatewaytest.ErrDB
	for range 2 {
		if _, err := cs.Team(ctx, teamA); err == nil {
			t.Fatal("error swallowed")
		}
	}
	if s.Reads["team"] != 2 {
		t.Fatalf("an error must not be cached: reads %d", s.Reads["team"])
	}
}

func TestCachedStoreLoadsOncePerKeyConcurrently(t *testing.T) {
	cs, s, _ := newCached(t)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { _, _ = cs.Team(t.Context(), teamA) })
	}
	wg.Wait()
	if s.Reads["team"] > 5 {
		t.Fatalf("team read %d times for 50 concurrent callers", s.Reads["team"])
	}
}

func TestOperationHelpersAndRoleARN(t *testing.T) {
	for op, streams := range map[gateway.Operation]bool{gateway.OpConverse: false, gateway.OpConverseStream: true, gateway.OpInvoke: false, gateway.OpInvokeStream: true, gateway.OpAnthropicMessages: false} {
		if op.Streams() != streams || !op.Known() {
			t.Errorf("%s: streams %v known %v", op, op.Streams(), op.Known())
		}
	}
	if gateway.Operation("apply-guardrail").Known() {
		t.Fatal("unknown operation accepted")
	}
	if _, err := gateway.TeamRoleARN("aws", "111122223333", "afs-t-x", "not-an-id"); err == nil {
		t.Fatal("a malformed team id produced a role ARN")
	}
}

func TestThresholdRecordFailureIsRetriedByALaterCall(t *testing.T) {
	r := newRig(t)
	r.thr.Err = gatewaytest.ErrDB
	big := gateway.Result{Usage: gateway.Usage{Output: 400000}, UsageReported: true, Started: true} // 6 USD of 10
	if err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(big, nil)); err != nil {
		t.Fatal(err)
	}
	r.svc.Wait()
	if r.thr.Count(domain.EventBudgetThresholdCrossed) != 0 {
		t.Fatal("recorded despite failure")
	}
	r.thr.Err = nil
	if err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(1, 1), nil)); err != nil {
		t.Fatal(err)
	}
	r.svc.Wait()
	if r.thr.Count(domain.EventBudgetThresholdCrossed) != 1 {
		t.Fatalf("threshold events = %d, want the 50%% crossing recorded by the next call", r.thr.Count(domain.EventBudgetThresholdCrossed))
	}
}

func TestExhaustedRecordFailureIsRetried(t *testing.T) {
	r := newRig(t)
	r.spend.Spent = domain.MustMoney("10")
	r.thr.Err = gatewaytest.ErrDB
	_ = r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(1, 1), nil))
	r.svc.Wait()
	r.thr.Err = nil
	_ = r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(ok(1, 1), nil))
	r.svc.Wait()
	if r.thr.Count(domain.EventBudgetExhausted) != 1 {
		t.Fatalf("exhausted events = %d", r.thr.Count(domain.EventBudgetExhausted))
	}
}

func TestUnknownCallErrorsBecomeUpstreamErrors(t *testing.T) {
	r := newRig(t)
	err := r.svc.Do(t.Context(), req("10.0.0.1"), r.invoke(gateway.Result{}, errors.New("socket reset")))
	if domain.KindOf(err) != domain.KindUpstream {
		t.Fatalf("err = %v", err)
	}
	if r.spend.Held() != 0 || r.spend.Settlements[0].Actual.Micros() != 0 {
		t.Fatal("an upstream failure before the first byte must release the whole reservation")
	}
}

// M4-B2 / GW-5: a changed tag reaches the next call at once on its notification (a service tag change names
// no team and reaches every team), and never later than the 60 s TTL.
func TestTagChangesRefreshOnNotificationAndWithinSixtySeconds(t *testing.T) {
	cs, s, clk := newCached(t)
	ctx := t.Context()
	read := func() []gateway.Tag {
		tags, err := cs.Tags(ctx, teamA)
		if err != nil {
			t.Fatal(err)
		}
		return tags
	}
	s.TagRows[teamA] = []gateway.Tag{{Key: "a", Value: "1", Source: "team"}}
	read()
	s.TagRows[teamA] = []gateway.Tag{{Key: "a", Value: "2", Source: "team"}}
	if got := read(); got[0].Value != "1" {
		t.Fatalf("served %v before any notification", got)
	}
	cs.Invalidate("service_tags", "")
	if got := read(); got[0].Value != "2" {
		t.Fatalf("a service tag notification did not refresh: %v", got)
	}
	s.TagRows[teamA] = []gateway.Tag{{Key: "a", Value: "3", Source: "team"}}
	clk.advance(59 * time.Second)
	if got := read(); got[0].Value != "2" {
		t.Fatalf("refreshed before the TTL: %v", got)
	}
	clk.advance(2 * time.Second)
	if got := read(); got[0].Value != "3" {
		t.Fatalf("a change missed by the notification took more than 60 s: %v", got)
	}
}

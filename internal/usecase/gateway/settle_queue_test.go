package gateway_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/gateway/gatewaytest"
)

// flakySpend fails the first n Settle calls, then delegates; with n < 0 it always fails; with block set
// every Settle waits for its context (a database that hangs).
type flakySpend struct {
	*gatewaytest.Spend
	fail  atomic.Int64
	calls atomic.Int64
	block bool
}

func (f *flakySpend) Settle(ctx context.Context, hold, bud string, p time.Time, actual domain.Money) (gateway.Counter, error) {
	f.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return gateway.Counter{}, err // a real driver refuses a dead context
	}
	if f.block {
		<-ctx.Done()
		return gateway.Counter{}, ctx.Err()
	}
	if n := f.fail.Load(); n < 0 || f.fail.Add(-1) >= 0 {
		return gateway.Counter{}, gatewaytest.ErrDB
	}
	return f.Spend.Settle(ctx, hold, bud, p, actual)
}

type settleEvent struct {
	team, budget string
	amount       domain.Money
	reason       gateway.SettleReason
}

type fakeSettleRecorder struct {
	mu     sync.Mutex
	events []settleEvent
}

func (f *fakeSettleRecorder) SettleFailed(team, bud string, amount domain.Money, reason gateway.SettleReason) {
	f.mu.Lock()
	f.events = append(f.events, settleEvent{team, bud, amount, reason})
	f.mu.Unlock()
}

func (f *fakeSettleRecorder) all() []settleEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]settleEvent(nil), f.events...)
}

// movClock is a movable clock.
type movClock struct{ t atomic.Int64 }

func (c *movClock) Now() time.Time          { return time.Unix(0, c.t.Load()).UTC() }
func (c *movClock) advance(d time.Duration) { c.t.Add(int64(d)) }

type queueRig struct {
	*rig
	flaky *flakySpend
	rec   *fakeSettleRecorder
	clk   *movClock
}

func newQueueRig(t *testing.T, failures int64, mutate func(*gateway.Config)) *queueRig {
	t.Helper()
	r := newRig(t)
	q := &queueRig{rig: r, flaky: &flakySpend{Spend: r.spend}, rec: &fakeSettleRecorder{}, clk: &movClock{}}
	q.flaky.fail.Store(failures)
	q.clk.t.Store(now.UnixNano())
	cfg := gateway.Config{Partition: "aws", AccountID: "111122223333", AsyncSettle: true, Now: q.clk.Now,
		SettleTimeout: time.Second, SettleRetryBase: time.Millisecond, SettleRetryMax: 5 * time.Millisecond}
	if mutate != nil {
		mutate(&cfg)
	}
	r.svc = gateway.NewService(gateway.Deps{Pods: r.dir, Tokens: r.tokens, Store: r.store, Spend: q.flaky, Thresholds: r.thr, Usage: r.usage,
		SettleRecorder: q.rec}, cfg)
	t.Cleanup(func() { r.svc.Close(context.Background()) })
	return q
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// actual of ok(100, 50) at 3 in and 15 out per Mtok: 0.0003 + 0.00075 = 0.00105.
const actual100x50 = "0.00105"

func TestFailedSettlementIsRetriedUntilItLandsAndNeverAfter(t *testing.T) {
	// Three inline attempts fail, two queue attempts fail, the next succeeds.
	q := newQueueRig(t, 5, nil)
	if err := q.svc.Do(t.Context(), req("10.0.0.1"), q.invoke(ok(100, 50), nil)); err != nil {
		t.Fatal(err)
	}
	q.svc.Wait()
	eventually(t, "the queued settlement to land", func() bool { return len(q.spend.Settled()) == 1 })
	if got := q.spend.Settled()[0].Actual; got != domain.MustMoney(actual100x50) {
		t.Fatalf("settled %s, want %s", got, actual100x50)
	}
	if got := q.spend.Snapshot().Spent; got != domain.MustMoney(actual100x50) || q.spend.Held() != 0 {
		t.Fatalf("spent %s held %d: the actual cost must end up in the period", got, q.spend.Held())
	}
	ev := q.rec.all()
	if len(ev) != 1 || ev[0].reason != gateway.SettleQueued || ev[0].team != teamA || ev[0].budget != budget || ev[0].amount != domain.MustMoney(actual100x50) {
		t.Fatalf("recorder events %+v", ev)
	}
	calls := q.flaky.calls.Load()
	time.Sleep(60 * time.Millisecond)
	if q.flaky.calls.Load() != calls || calls != 6 {
		t.Fatalf("Settle called %d times, then %d: a settled entry must never be retried", calls, q.flaky.calls.Load())
	}
}

func TestSettlementFallbackDoesNotAddLatencyToTheResponse(t *testing.T) {
	q := newQueueRig(t, -1, nil)
	start := time.Now()
	if err := q.svc.Do(t.Context(), req("10.0.0.1"), q.invoke(ok(100, 50), nil)); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("Do took %v with every settlement failing; the books close off the response path", d)
	}
}

func TestFullQueueIsCountedAndLogged(t *testing.T) {
	q := newQueueRig(t, -1, func(c *gateway.Config) {
		c.SettleQueueSize = 1
		c.SettleRetryBase = time.Hour
		c.SettleRetryMax = time.Hour
	})
	for range 2 {
		if err := q.svc.Do(t.Context(), req("10.0.0.1"), q.invoke(ok(100, 50), nil)); err != nil {
			t.Fatal(err)
		}
	}
	q.svc.Wait()
	// The two calls close their books concurrently, so the order is not fixed: one is queued, one dropped.
	ev := q.rec.all()
	if len(ev) != 2 {
		t.Fatalf("events %+v, want one queued and one queue_full", ev)
	}
	var drop settleEvent
	switch {
	case ev[0].reason == gateway.SettleQueued && ev[1].reason == gateway.SettleQueueFull:
		drop = ev[1]
	case ev[1].reason == gateway.SettleQueued && ev[0].reason == gateway.SettleQueueFull:
		drop = ev[0]
	default:
		t.Fatalf("events %+v, want one queued and one queue_full", ev)
	}
	if drop.team != teamA || drop.budget != budget || drop.amount != domain.MustMoney(actual100x50) {
		t.Fatalf("the drop must name team, budget and amount: %+v", drop)
	}
}

func TestSettlementPastTheRetryWindowIsDroppedAndCounted(t *testing.T) {
	q := newQueueRig(t, -1, func(c *gateway.Config) { c.SettleRetryWindow = 10 * time.Minute })
	if err := q.svc.Do(t.Context(), req("10.0.0.1"), q.invoke(ok(100, 50), nil)); err != nil {
		t.Fatal(err)
	}
	q.svc.Wait()
	eventually(t, "the first retry", func() bool { return q.flaky.calls.Load() > 3 })
	q.clk.advance(11 * time.Minute)
	eventually(t, "the window_expired drop", func() bool {
		ev := q.rec.all()
		return len(ev) == 2 && ev[1].reason == gateway.SettleWindowExpired
	})
	if e := q.rec.all()[1]; e.team != teamA || e.budget != budget || e.amount != domain.MustMoney(actual100x50) {
		t.Fatalf("the drop must name team, budget and amount: %+v", e)
	}
	calls := q.flaky.calls.Load()
	time.Sleep(40 * time.Millisecond)
	if q.flaky.calls.Load() != calls {
		t.Fatal("an expired entry is still being retried")
	}
}

func TestCloseGivesPendingSettlementsOneMoreAttempt(t *testing.T) {
	// Inline attempts fail (3); the backoff is an hour, so only Close can settle it.
	q := newQueueRig(t, 3, func(c *gateway.Config) { c.SettleRetryBase = time.Hour; c.SettleRetryMax = time.Hour })
	if err := q.svc.Do(t.Context(), req("10.0.0.1"), q.invoke(ok(100, 50), nil)); err != nil {
		t.Fatal(err)
	}
	q.svc.Wait()
	if len(q.spend.Settled()) != 0 {
		t.Fatal("settled before Close")
	}
	q.svc.Close(t.Context())
	if len(q.spend.Settled()) != 1 || q.spend.Held() != 0 {
		t.Fatalf("settled %d held %d after Close", len(q.spend.Settled()), q.spend.Held())
	}
}

func TestCloseIsBoundedBySettleTimeout(t *testing.T) {
	q := newQueueRig(t, -1, func(c *gateway.Config) {
		c.SettleTimeout = 150 * time.Millisecond
		c.SettleRetryBase = time.Hour
		c.SettleRetryMax = time.Hour
	})
	if err := q.svc.Do(t.Context(), req("10.0.0.1"), q.invoke(ok(100, 50), nil)); err != nil {
		t.Fatal(err)
	}
	q.svc.Wait()
	q.flaky.block = true // the database now hangs
	start := time.Now()
	q.svc.Close(t.Context())
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %v with a hung database; it is bounded by SettleTimeout", d)
	}
	if q.spend.Held() != 1 {
		t.Fatal("the hold falls back to expiry when the final attempt fails")
	}
}

func TestSettlementAfterCloseIsCountedNotLost(t *testing.T) {
	q := newQueueRig(t, -1, nil)
	q.svc.Close(t.Context())
	if err := q.svc.Do(t.Context(), req("10.0.0.1"), q.invoke(ok(1, 1), nil)); err != nil {
		t.Fatal(err)
	}
	q.svc.Wait()
	ev := q.rec.all()
	if len(ev) != 1 || ev[0].reason != gateway.SettleQueueFull {
		t.Fatalf("events %+v", ev)
	}
}

func TestSettleErrIsNotMaskedByTheQueue(t *testing.T) {
	// A call that fails upstream still settles to zero through the queue path; the call's own error stays.
	q := newQueueRig(t, 3, nil)
	err := q.svc.Do(t.Context(), req("10.0.0.1"), q.invoke(gateway.Result{}, &domain.Error{Kind: domain.KindUpstream, Message: "boom"}))
	if kind(err) != domain.KindUpstream {
		t.Fatalf("err %v", err)
	}
	q.svc.Wait()
	eventually(t, "the release", func() bool { return q.spend.Held() == 0 })
}

func TestQueuedRetryIsNotBoundByTheInlineDeadline(t *testing.T) {
	// The inline loop gives up at SettleTimeout (30 ms); the retry runs later (60 ms) and must still have
	// a live context of its own.
	q := newQueueRig(t, 1, func(c *gateway.Config) {
		c.SettleTimeout = 30 * time.Millisecond
		c.SettleRetryBase = 60 * time.Millisecond
		c.SettleRetryMax = 60 * time.Millisecond
	})
	if err := q.svc.Do(t.Context(), req("10.0.0.1"), q.invoke(ok(100, 50), nil)); err != nil {
		t.Fatal(err)
	}
	q.svc.Wait()
	eventually(t, "the retry to land", func() bool { return len(q.spend.Settled()) == 1 })
}

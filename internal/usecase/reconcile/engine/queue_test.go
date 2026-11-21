package engine

import (
	"context"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestQueue() (*queue, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	return newQueue(c.Now, DefaultBackoff), c
}

func get(t *testing.T, q *queue) (Ref, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	return q.Get(ctx)
}

func TestQueueCoalescesAndOrders(t *testing.T) {
	q, clock := newTestQueue()
	a, b := NewRef(KindTeam, "team_A", "team_A"), NewRef(KindTeam, "team_B", "team_B")
	q.Add(a)
	q.Add(b)
	q.Add(a) // coalesced
	if q.Len() != 2 {
		t.Fatalf("Len = %d, want 2", q.Len())
	}
	first, ok := get(t, q)
	second, _ := get(t, q)
	if !ok || first != a || second != b {
		t.Errorf("order = %v, %v", first, second)
	}
	if _, ok := get(t, q); ok {
		t.Error("Get returned an item from an empty queue")
	}
	q.Done(first)
	q.Done(second)
	// a future item is not returned until due
	q.AddAfter(a, time.Minute)
	if _, ok := get(t, q); ok {
		t.Error("item returned before it was due")
	}
	if _, due := q.OldestDue(); due {
		t.Error("OldestDue reports a future item")
	}
	clock.Advance(time.Minute)
	if got, ok := get(t, q); !ok || got != a {
		t.Errorf("due item = %v, %v", got, ok)
	}
	// an earlier due time wins when added again
	q.Done(a)
	q.AddAfter(a, time.Hour)
	q.AddAfter(a, time.Second)
	clock.Advance(time.Second)
	if got, ok := get(t, q); !ok || got != a {
		t.Errorf("earlier due time lost: %v, %v", got, ok)
	}
}

func TestQueueTeamAdoptedLater(t *testing.T) {
	q, _ := newTestQueue()
	q.Add(Ref{Key: Key{KindAgent, "agt_1"}})
	q.Add(NewRef(KindAgent, "agt_1", "team_A"))
	got, _ := get(t, q)
	if got.TeamID != "team_A" {
		t.Errorf("team not adopted: %+v", got)
	}
}

func TestQueueSerializesATeam(t *testing.T) {
	q, _ := newTestQueue()
	t1, t2 := NewRef(KindTeam, "team_A", "team_A"), NewRef(KindAgent, "agt_1", "team_A")
	other := NewRef(KindAgent, "agt_2", "team_B")
	q.Add(t1)
	q.Add(t2)
	q.Add(other)
	g1, _ := get(t, q)
	if g1 != t1 {
		t.Fatalf("first = %v", g1)
	}
	g2, _ := get(t, q)
	if g2 != other {
		t.Errorf("second = %v, want the other team's item while team_A is busy", g2)
	}
	if _, ok := get(t, q); ok {
		t.Error("a second team_A item ran concurrently")
	}
	q.Done(t1)
	if g3, ok := get(t, q); !ok || g3 != t2 {
		t.Errorf("after Done: %v, %v", g3, ok)
	}
}

func TestQueueAddWhileRunningRunsAgain(t *testing.T) {
	q, clock := newTestQueue()
	a := NewRef(KindTeam, "team_A", "team_A")
	q.Add(a)
	got, _ := get(t, q)
	q.Add(a)
	q.AddAfter(a, time.Hour) // later than the pending rerun: ignored
	q.AddAfter(a, time.Second)
	if _, ok := get(t, q); ok {
		t.Fatal("running item handed out twice")
	}
	q.Done(got)
	clock.Advance(time.Second)
	if again, ok := get(t, q); !ok || again != a {
		t.Errorf("item added while running was lost: %v, %v", again, ok)
	}
}

func TestQueueRateLimited(t *testing.T) {
	q, clock := newTestQueue()
	a := NewRef(KindTeam, "team_A", "team_A")
	var delays []time.Duration
	for range 11 {
		delays = append(delays, q.AddRateLimited(a, false))
		clock.Advance(10 * time.Minute)
		g, _ := get(t, q)
		q.Done(g)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300}
	for i, w := range want {
		if delays[i] != w*time.Second {
			t.Errorf("delay %d = %v, want %v", i, delays[i], w*time.Second)
		}
	}
	if q.Failures(a.Key) != 11 {
		t.Errorf("failures = %d", q.Failures(a.Key))
	}
	q.Forget(a.Key)
	if d := q.AddRateLimited(a, false); d != time.Second {
		t.Errorf("after Forget: %v", d)
	}
	if d := q.AddRateLimited(NewRef(KindTeam, "team_T", "team_T"), true); d != DefaultBackoff.Max {
		t.Errorf("terminal delay = %v", d)
	}
}

func TestQueueShutDownReleasesWaiters(t *testing.T) {
	q, _ := newTestQueue()
	done := make(chan bool, 2)
	for range 2 {
		go func() { _, ok := q.Get(context.Background()); done <- ok }()
	}
	time.Sleep(10 * time.Millisecond)
	q.ShutDown()
	for range 2 {
		select {
		case ok := <-done:
			if ok {
				t.Error("Get returned an item after shutdown")
			}
		case <-time.After(time.Second):
			t.Fatal("waiter not released")
		}
	}
	q.Add(NewRef(KindTeam, "team_A", "team_A")) // ignored
	if q.Len() != 0 {
		t.Error("item accepted after shutdown")
	}
}

func TestQueueWakesAllWaiters(t *testing.T) {
	q, _ := newTestQueue()
	res := make(chan Ref, 2)
	for range 2 {
		go func() { r, _ := q.Get(context.Background()); res <- r }()
	}
	time.Sleep(10 * time.Millisecond)
	q.Add(NewRef(KindTeam, "team_A", "team_A"))
	q.Add(NewRef(KindTeam, "team_B", "team_B"))
	for range 2 {
		select {
		case <-res:
		case <-time.After(time.Second):
			t.Fatal("a waiter was not woken for the second item")
		}
	}
	q.ShutDown()
}

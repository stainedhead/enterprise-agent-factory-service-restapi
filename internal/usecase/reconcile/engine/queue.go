package engine

import (
	"context"
	"sync"
	"time"
)

// queue is the work queue: items keyed by (kind, id), each with a due time. Adding a key that is
// already waiting keeps the earlier due time (coalescing); adding one that is running marks it to run
// again afterwards. Items of one team never run concurrently, which orders a team's work.
type queue struct {
	mu       sync.Mutex
	now      func() time.Time
	backoff  Backoff
	waiting  map[Key]*item
	running  map[Key]*item // being reconciled
	rerun    map[Key]*item // added while running
	teamBusy map[string]bool
	failures map[Key]int
	seq      uint64
	notify   chan struct{} // closed and replaced on every change, so every waiting Get wakes
	closed   bool
}

type item struct {
	ref Ref
	due time.Time
	seq uint64
}

func newQueue(now func() time.Time, b Backoff) *queue {
	return &queue{
		now: now, backoff: b,
		waiting: map[Key]*item{}, running: map[Key]*item{}, rerun: map[Key]*item{},
		teamBusy: map[string]bool{}, failures: map[Key]int{}, notify: make(chan struct{}),
	}
}

// signal wakes every waiting Get. The caller holds q.mu.
func (q *queue) signal() {
	close(q.notify)
	q.notify = make(chan struct{})
}

// Add queues ref to run now.
func (q *queue) Add(ref Ref) { q.AddAfter(ref, 0) }

// AddAfter queues ref to run after d.
func (q *queue) AddAfter(ref Ref, d time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	due := q.now().Add(d)
	q.seq++
	if _, ok := q.running[ref.Key]; ok {
		if r, ok := q.rerun[ref.Key]; !ok || due.Before(r.due) {
			q.rerun[ref.Key] = &item{ref: ref, due: due, seq: q.seq}
		}
		return
	}
	if it, ok := q.waiting[ref.Key]; ok {
		if due.Before(it.due) {
			it.due = due
		}
		if it.ref.TeamID == "" {
			it.ref.TeamID = ref.TeamID
		}
		q.signal()
		return
	}
	q.waiting[ref.Key] = &item{ref: ref, due: due, seq: q.seq}
	q.signal()
}

// AddRateLimited counts a failure of ref and queues it after the backoff for it. A terminal failure
// waits the longest interval.
func (q *queue) AddRateLimited(ref Ref, terminal bool) time.Duration {
	q.mu.Lock()
	q.failures[ref.Key]++
	d := q.backoff.Delay(q.failures[ref.Key])
	if terminal {
		d = q.backoff.Max
	}
	q.mu.Unlock()
	q.AddAfter(ref, d)
	return d
}

// Forget resets the failure count of key.
func (q *queue) Forget(key Key) {
	q.mu.Lock()
	delete(q.failures, key)
	q.mu.Unlock()
}

// Failures returns the consecutive failures of key.
func (q *queue) Failures(key Key) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.failures[key]
}

// Len returns the number of waiting items.
func (q *queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiting)
}

// OldestDue returns the due time of the longest-overdue waiting item, if any is due.
func (q *queue) OldestDue() (time.Time, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	var oldest time.Time
	found := false
	for _, it := range q.waiting {
		if !it.due.After(now) && (!found || it.due.Before(oldest)) {
			oldest, found = it.due, true
		}
	}
	return oldest, found
}

// Get blocks until an item is due and its team is idle, and returns it marked as running. ok is
// false when ctx ended or the queue was shut down.
func (q *queue) Get(ctx context.Context) (Ref, bool) {
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return Ref{}, false
		}
		now := q.now()
		var best *item
		var next time.Time
		for _, it := range q.waiting {
			if it.ref.TeamID != "" && q.teamBusy[it.ref.TeamID] {
				continue
			}
			if it.due.After(now) {
				if next.IsZero() || it.due.Before(next) {
					next = it.due
				}
				continue
			}
			if best == nil || it.due.Before(best.due) || (it.due.Equal(best.due) && it.seq < best.seq) {
				best = it
			}
		}
		if best != nil {
			delete(q.waiting, best.ref.Key)
			q.running[best.ref.Key] = best
			if best.ref.TeamID != "" {
				q.teamBusy[best.ref.TeamID] = true
			}
			q.mu.Unlock()
			return best.ref, true
		}
		ch := q.notify
		q.mu.Unlock()

		var timer <-chan time.Time
		var t *time.Timer
		if !next.IsZero() {
			t = time.NewTimer(max(time.Until(next), time.Millisecond))
			timer = t.C
		}
		select {
		case <-ctx.Done():
			if t != nil {
				t.Stop()
			}
			return Ref{}, false
		case <-ch:
		case <-timer:
		}
		if t != nil {
			t.Stop()
		}
	}
}

// Done marks ref finished; an addition made while it ran is queued now.
func (q *queue) Done(ref Ref) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.running, ref.Key)
	if ref.TeamID != "" {
		delete(q.teamBusy, ref.TeamID)
	}
	if r, ok := q.rerun[ref.Key]; ok {
		delete(q.rerun, ref.Key)
		if !q.closed {
			q.waiting[ref.Key] = r
		}
	}
	q.signal()
}

// ShutDown makes Get return false.
func (q *queue) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.signal()
}

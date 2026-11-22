package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// SettleReason says why a settlement is counted as a fallback (FR-R05).
type SettleReason string

// Reasons a settlement is reported to the SettleRecorder.
const (
	// SettleQueued: the inline attempts failed and the settlement went to the retry queue.
	SettleQueued SettleReason = "queued"
	// SettleQueueFull: the queue had no room (or the service is closed); the actual cost is not added.
	SettleQueueFull SettleReason = "queue_full"
	// SettleWindowExpired: the retry window ran out; the actual cost is not added.
	SettleWindowExpired SettleReason = "window_expired"
)

// SettleRecorder is told of every settlement that falls back to the retry queue, and of every one the queue
// then drops. The composition root binds it to afs_gateway_settle_failures_total (RD-3, RD-4). One
// settlement can be reported twice (queued, later window_expired); alert on the rate.
type SettleRecorder interface {
	SettleFailed(teamID, budgetID string, amount domain.Money, reason SettleReason)
}

// Defaults of the settlement retry queue (FR-R05, Q2).
const (
	DefaultSettleQueueSize   = 10_000
	DefaultSettleRetryWindow = 10 * time.Minute
	DefaultSettleRetryBase   = time.Second
	DefaultSettleRetryMax    = time.Minute
)

// pendingSettle is one settlement waiting for the database.
type pendingSettle struct {
	// ctx carries the request's values (trace ids) but never its cancellation.
	ctx                      context.Context
	teamID, holdID, budgetID string
	period                   time.Time
	actual                   domain.Money
	first                    time.Time // cfg.Now at the first failure; the retry window starts here
	attempts                 int
	next                     time.Time // wall clock; backoff is not affected by a test's fake Now
}

// settleQueue retries settlements that failed inline: bounded in size, exponential backoff, a fixed
// window. A settlement is retried only after an error, never after a success, so the actual cost is added
// once (afs.settle_reservation still adds the cost for a hold that is already released, database contract).
// It is in memory: a crash falls back to hold expiry, which the gateway contract allows (Q2).
type settleQueue struct {
	s       *Service
	mu      sync.Mutex
	items   []*pendingSettle
	size    int // queued plus the one being attempted
	running bool
	closed  bool
	stop    chan struct{}
	wg      sync.WaitGroup
}

func newSettleQueue(s *Service) *settleQueue { return &settleQueue{s: s, stop: make(chan struct{})} }

func (q *settleQueue) backoff(attempts int) time.Duration {
	d := q.s.cfg.SettleRetryBase
	for i := 1; i < attempts && d < q.s.cfg.SettleRetryMax; i++ {
		d *= 2
	}
	return min(d, q.s.cfg.SettleRetryMax)
}

// add queues p and reports why it did not fit.
func (q *settleQueue) add(p *pendingSettle) (SettleReason, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.size >= q.s.cfg.SettleQueueSize {
		return SettleQueueFull, false
	}
	p.first = q.s.cfg.Now()
	p.attempts = 3 // the inline attempts
	p.next = time.Now().Add(q.backoff(1))
	q.items = append(q.items, p)
	q.size++
	if !q.running {
		q.running = true
		q.wg.Go(q.run)
	}
	return SettleQueued, true
}

// pop returns the next due entry, or how long to wait; ok is false when the loop should end.
func (q *settleQueue) pop() (p *pendingSettle, wait time.Duration, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || len(q.items) == 0 {
		q.running = false
		return nil, 0, false
	}
	now := time.Now()
	soonest := 0
	for i, it := range q.items {
		if !it.next.After(now) {
			q.items = append(q.items[:i], q.items[i+1:]...)
			return it, 0, true
		}
		if it.next.Before(q.items[soonest].next) {
			soonest = i
		}
	}
	return nil, q.items[soonest].next.Sub(now), true
}

func (q *settleQueue) run() {
	for {
		p, wait, ok := q.pop()
		if !ok {
			return
		}
		if p == nil {
			select {
			case <-time.After(wait):
			case <-q.stop:
			}
			continue
		}
		q.attempt(p)
	}
}

// attempt tries one settlement; on failure it goes back in the queue or is dropped after the window.
func (q *settleQueue) attempt(p *pendingSettle) {
	s := q.s
	ctx, cancel := context.WithTimeout(p.ctx, s.cfg.SettleTimeout)
	_, err := s.spend.Settle(ctx, p.holdID, p.budgetID, p.period, p.actual)
	cancel()
	q.mu.Lock()
	defer q.mu.Unlock()
	if err == nil {
		q.size--
		return
	}
	p.attempts++
	if s.cfg.Now().Sub(p.first) >= s.cfg.SettleRetryWindow {
		q.size--
		s.dropSettle(p, SettleWindowExpired, err)
		return
	}
	p.next = time.Now().Add(q.backoff(p.attempts - 2))
	q.items = append(q.items, p)
}

// close stops the loop and gives every pending settlement one more attempt, all bounded by SettleTimeout.
func (q *settleQueue) close(ctx context.Context) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	close(q.stop)
	q.mu.Unlock()
	q.wg.Wait()

	q.mu.Lock()
	rest := q.items
	q.items = nil
	q.mu.Unlock()
	if len(rest) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), q.s.cfg.SettleTimeout)
	defer cancel()
	for _, p := range rest {
		if _, err := q.s.spend.Settle(ctx, p.holdID, p.budgetID, p.period, p.actual); err != nil {
			q.s.log.Error("settlement not completed at shutdown; the hold will expire",
				"teamId", p.teamID, "budgetId", p.budgetID, "amountUsd", p.actual.String(), "err", err)
		}
	}
}

// dropSettle counts and logs a settlement the queue gave up on (team, budget and amount, no more).
func (s *Service) dropSettle(p *pendingSettle, reason SettleReason, err error) {
	s.log.Error("settlement dropped; the actual cost is not in the period", "reason", string(reason),
		"teamId", p.teamID, "budgetId", p.budgetID, "amountUsd", p.actual.String(), "err", err)
	if s.Deps.SettleRecorder != nil {
		s.Deps.SettleRecorder.SettleFailed(p.teamID, p.budgetID, p.actual, reason)
	}
}

// fallBack queues a settlement whose inline attempts failed and counts it.
func (s *Service) fallBack(p *pendingSettle, lastErr error) {
	reason, ok := s.queue.add(p)
	if !ok {
		s.dropSettle(p, reason, lastErr)
		return
	}
	s.log.Error("settle reservation failed; queued for retry", "teamId", p.teamID, "budgetId", p.budgetID,
		"amountUsd", p.actual.String(), "err", lastErr)
	if s.Deps.SettleRecorder != nil {
		s.Deps.SettleRecorder.SettleFailed(p.teamID, p.budgetID, p.actual, SettleQueued)
	}
}

// Close drains the books of finished calls and gives the settlements still queued one last attempt,
// bounded by SettleTimeout whatever ctx does (shutdown cancels its context first). It is for shutdown,
// after the listener stopped. A crash instead of Close falls back to hold expiry (gateway contract
// section 4).
func (s *Service) Close(ctx context.Context) {
	s.Wait()
	s.queue.close(ctx)
}

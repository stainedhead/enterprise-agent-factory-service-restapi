package housekeeping

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Source is the EventBridge source of every AFS event (API contract §7).
const Source = "afs"

// PutEventsMax is the EventBridge limit of entries per PutEvents call.
const PutEventsMax = 10

// Backoff bounds: after the n-th failed attempt of an event the next try waits
// min(BackoffMax, BackoffBase * 2^(n-1)).
const (
	BackoffBase = time.Second
	BackoffMax  = 5 * time.Minute
	// BatchSize is how many pending rows one read takes.
	BatchSize = 100
)

// OutboxEvent is one unpublished outbox row.
type OutboxEvent struct {
	ID            int64
	Type          string
	AggregateKind string
	AggregateID   string
	TeamID        string
	Payload       json.RawMessage
	CreatedAt     time.Time
	// Attempts counts failed publishes so far (outbox_events.attempts).
	Attempts int
}

// OutboxStore is the database side (adapter: postgres/housekeeping.go).
type OutboxStore interface {
	// Pending returns up to limit unpublished events in id order.
	Pending(ctx context.Context, limit int) ([]OutboxEvent, error)
	// MarkPublished sets published_at on the rows.
	MarkPublished(ctx context.Context, ids []int64) error
	// RecordFailure adds one to the attempts of the row.
	RecordFailure(ctx context.Context, id int64) error
	// Lag returns how many events are unpublished and the age of the oldest.
	Lag(ctx context.Context) (pending int, oldest time.Duration, err error)
}

// Entry is one event to put on the bus.
type Entry struct {
	// Key is the outbox id, so a result can be matched to its row.
	Key        int64
	DetailType string
	Detail     json.RawMessage
	Time       time.Time
	// Resources lists the aggregate, as "<kind>/<id>".
	Resources []string
}

// EventSink puts events on the bus (adapter: aws/eventbridge).
type EventSink interface {
	// PutEvents sends entries (at most PutEventsMax) and returns one error per entry, in order, nil
	// for an accepted entry. A non-nil error return means the whole call failed.
	PutEvents(ctx context.Context, entries []Entry) ([]error, error)
}

// Recorder receives the publisher's metrics (observability adapter, M1-L).
type Recorder interface {
	OutboxPublished(n int)
	OutboxFailed(n int)
	// OutboxLag reports the number of unpublished events and the age of the oldest.
	OutboxLag(pending int, oldest time.Duration)
}

// Publisher publishes outbox events at least once, in id order per aggregate.
type Publisher struct {
	store OutboxStore
	sink  EventSink
	rec   Recorder
	clock func() time.Time

	mu    sync.Mutex
	retry map[int64]time.Time // event id -> earliest next attempt
}

// NewPublisher returns a Publisher; rec may be nil.
func NewPublisher(store OutboxStore, sink EventSink, rec Recorder, clock func() time.Time) *Publisher {
	if clock == nil {
		clock = time.Now
	}
	return &Publisher{store: store, sink: sink, rec: rec, clock: clock, retry: map[int64]time.Time{}}
}

// Backoff returns the wait after the n-th failed attempt (n >= 1).
func Backoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := BackoffBase
	for i := 1; i < n && d < BackoffMax; i++ {
		d *= 2
	}
	return min(d, BackoffMax)
}

// Drain publishes pending events until none are due, then reports lag. It is the scheduled job.
func (p *Publisher) Drain(ctx context.Context) error {
	var errs []error
	for {
		n, err := p.RunOnce(ctx)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if n == 0 || ctx.Err() != nil {
			break
		}
	}
	if p.rec != nil {
		if pending, oldest, err := p.store.Lag(ctx); err == nil {
			p.rec.OutboxLag(pending, oldest)
		} else {
			errs = append(errs, fmt.Errorf("housekeeping: outbox lag: %w", err))
		}
	}
	return errors.Join(errs...)
}

// RunOnce reads one batch and publishes what is due. It returns how many events it published; zero
// means there was nothing due (or everything is backing off).
func (p *Publisher) RunOnce(ctx context.Context) (int, error) {
	events, err := p.store.Pending(ctx, BatchSize)
	if err != nil {
		return 0, fmt.Errorf("housekeeping: read outbox: %w", err)
	}
	now := p.clock()
	queues, order := p.queues(events, now)
	published, failed := 0, 0
	var firstErr error
	// Rounds: each aggregate contributes its next event per round, so an aggregate's events go out
	// in separate sequential calls and in id order; an aggregate whose event failed sits out the
	// rest of the pass.
	for len(order) > 0 {
		var round []OutboxEvent
		for _, agg := range order {
			round = append(round, queues[agg][0])
		}
		order = order[:0]
		for start := 0; start < len(round); start += PutEventsMax {
			batch := round[start:min(start+PutEventsMax, len(round))]
			ok, bad, err := p.send(ctx, batch)
			published += len(ok)
			failed += len(bad)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			failedAgg := map[string]bool{}
			for _, e := range bad {
				failedAgg[aggKey(e)] = true
			}
			for _, e := range batch {
				agg := aggKey(e)
				if failedAgg[agg] {
					delete(queues, agg)
					continue
				}
				queues[agg] = queues[agg][1:]
				if len(queues[agg]) > 0 {
					order = append(order, agg)
				}
			}
		}
	}
	if p.rec != nil {
		if published > 0 {
			p.rec.OutboxPublished(published)
		}
		if failed > 0 {
			p.rec.OutboxFailed(failed)
		}
	}
	return published, firstErr
}

// queues groups due events per aggregate keeping id order. An aggregate whose first pending event is
// still backing off is skipped whole (head-of-line: later events must not overtake it).
func (p *Publisher) queues(events []OutboxEvent, now time.Time) (map[string][]OutboxEvent, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	q := map[string][]OutboxEvent{}
	var order []string
	blocked := map[string]bool{}
	for _, e := range events {
		agg := aggKey(e)
		if blocked[agg] {
			continue
		}
		if len(q[agg]) == 0 {
			if at, ok := p.retry[e.ID]; ok && now.Before(at) {
				blocked[agg] = true
				continue
			}
			order = append(order, agg)
		}
		q[agg] = append(q[agg], e)
	}
	return q, order
}

func aggKey(e OutboxEvent) string { return e.AggregateKind + "/" + e.AggregateID }

// send puts one batch and settles the rows: published ones are marked, failed ones get an attempt
// and a retry time.
func (p *Publisher) send(ctx context.Context, batch []OutboxEvent) (ok, bad []OutboxEvent, err error) {
	entries := make([]Entry, len(batch))
	for i, e := range batch {
		entries[i] = Entry{Key: e.ID, DetailType: e.Type, Detail: Envelope(e), Time: e.CreatedAt, Resources: []string{aggKey(e)}}
	}
	results, callErr := p.sink.PutEvents(ctx, entries)
	if callErr == nil && len(results) != len(entries) {
		callErr = fmt.Errorf("housekeeping: PutEvents returned %d results for %d entries", len(results), len(entries))
	}
	var ids []int64
	for i, e := range batch {
		if callErr != nil || results[i] != nil {
			bad = append(bad, e)
			continue
		}
		ok = append(ok, e)
		ids = append(ids, e.ID)
	}
	var errs []error
	if callErr != nil {
		errs = append(errs, callErr)
	}
	if len(ids) > 0 {
		// If this write fails the events go out again next pass: at least once.
		if err := p.store.MarkPublished(ctx, ids); err != nil {
			errs = append(errs, fmt.Errorf("housekeeping: mark published: %w", err))
		}
		p.mu.Lock()
		for _, id := range ids {
			delete(p.retry, id)
		}
		p.mu.Unlock()
	}
	for i, e := range batch {
		if callErr == nil && results[i] == nil {
			continue
		}
		if err := p.store.RecordFailure(ctx, e.ID); err != nil {
			errs = append(errs, fmt.Errorf("housekeeping: record failure: %w", err))
		}
		p.mu.Lock()
		p.retry[e.ID] = p.clock().Add(Backoff(e.Attempts + 1))
		p.mu.Unlock()
		if callErr == nil {
			errs = append(errs, fmt.Errorf("housekeeping: event %d (%s): %w", e.ID, e.Type, results[i]))
		}
	}
	return ok, bad, errors.Join(errs...)
}

// Envelope returns the event detail in the API contract section 7 shape {id, occurredAt, teamId, poolId?, actor,
// data}. Two payload shapes sit in the outbox: the envelope that the API and -db 000019 (afs.event_detail) write,
// which goes out unchanged, and the flat payload that afs.instantiate_work_order_team wrote before 000019
// (for example {teamId, kind, workOrderId}), which is wrapped: the flat keys become data, teamId (and poolId when
// present) are lifted into the envelope, occurredAt is the row's creation time, and the id derives from the outbox
// id so a redelivery carries the same id (consumers dedupe on it). Rows already in the outbox keep their old
// payload after 000019, so the publisher accepts both for as long as such rows can exist.
func Envelope(e OutboxEvent) json.RawMessage {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(e.Payload, &top); err != nil {
		return e.Payload // not an object: nothing to wrap, publish as is
	}
	if isEnvelope(top) {
		return e.Payload
	}
	env := map[string]any{
		"id":         fmt.Sprintf("evt_%026d", e.ID),
		"occurredAt": e.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"actor":      "system",
		"data":       top,
	}
	team := e.TeamID
	if team == "" {
		team = stringField(top, "teamId")
	}
	if team != "" {
		env["teamId"] = team
	}
	if pool := stringField(top, "poolId"); pool != "" {
		env["poolId"] = pool
	}
	out, err := json.Marshal(env)
	if err != nil {
		return e.Payload
	}
	return out
}

// isEnvelope reports whether the payload already has the section 7 shape: an id, an occurredAt and an object data.
func isEnvelope(top map[string]json.RawMessage) bool {
	d, ok := top["data"]
	if !ok || stringField(top, "id") == "" || stringField(top, "occurredAt") == "" {
		return false
	}
	var data map[string]json.RawMessage
	return json.Unmarshal(d, &data) == nil
}

func stringField(top map[string]json.RawMessage, key string) string {
	var s string
	if raw, ok := top[key]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

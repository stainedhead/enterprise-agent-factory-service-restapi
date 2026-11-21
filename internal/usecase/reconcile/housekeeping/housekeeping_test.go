package housekeeping

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu        sync.Mutex
	events    []OutboxEvent
	published map[int64]bool
	attempts  map[int64]int
	err       error
	markErr   error
}

func newStore(evs ...OutboxEvent) *fakeStore {
	return &fakeStore{events: evs, published: map[int64]bool{}, attempts: map[int64]int{}}
}

func (s *fakeStore) Pending(_ context.Context, limit int) ([]OutboxEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	var out []OutboxEvent
	for _, e := range s.events {
		if !s.published[e.ID] && len(out) < limit {
			e.Attempts = s.attempts[e.ID]
			out = append(out, e)
		}
	}
	return out, nil
}
func (s *fakeStore) MarkPublished(_ context.Context, ids []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markErr != nil {
		return s.markErr
	}
	for _, id := range ids {
		s.published[id] = true
	}
	return nil
}
func (s *fakeStore) RecordFailure(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[id]++
	return nil
}
func (s *fakeStore) Lag(context.Context) (int, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if !s.published[e.ID] {
			n++
		}
	}
	return n, 42 * time.Second, nil
}

type fakeSink struct {
	mu      sync.Mutex
	calls   [][]int64 // keys per call
	sent    []int64   // in the order accepted
	failIDs map[int64]error
	callErr error
	short   bool
}

func (f *fakeSink) PutEvents(_ context.Context, es []Entry) ([]error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(es) > PutEventsMax {
		panic("batch over the EventBridge limit")
	}
	var keys []int64
	res := make([]error, len(es))
	for i, e := range es {
		keys = append(keys, e.Key)
		if err := f.failIDs[e.Key]; err != nil {
			res[i] = err
			continue
		}
		f.sent = append(f.sent, e.Key)
	}
	f.calls = append(f.calls, keys)
	if f.callErr != nil {
		return nil, f.callErr
	}
	if f.short {
		return res[:0], nil
	}
	return res, nil
}

type rec struct{ pub, fail, pending int }

func (r *rec) OutboxPublished(n int)            { r.pub += n }
func (r *rec) OutboxFailed(n int)               { r.fail += n }
func (r *rec) OutboxLag(p int, _ time.Duration) { r.pending = p }

func ev(id int64, agg string) OutboxEvent {
	return OutboxEvent{ID: id, Type: "team.created", AggregateKind: "team", AggregateID: agg, Payload: json.RawMessage(`{"id":"evt"}`), CreatedAt: time.Unix(1700000000+id, 0)}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestPublishesInOrderPerAggregate(t *testing.T) {
	st := newStore(ev(1, "a"), ev(2, "b"), ev(3, "a"), ev(4, "a"), ev(5, "b"))
	sk := &fakeSink{}
	r := &rec{}
	p := NewPublisher(st, sk, r, nil)
	if err := p.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	pos := map[int64]int{}
	for i, k := range sk.sent {
		pos[k] = i
	}
	if len(sk.sent) != 5 || pos[1] >= pos[3] || pos[3] >= pos[4] || pos[2] >= pos[5] {
		t.Fatalf("sent %v", sk.sent)
	}
	// events of one aggregate never share a call
	for _, c := range sk.calls {
		seen := map[string]bool{}
		for _, k := range c {
			a := "a"
			if k == 2 || k == 5 {
				a = "b"
			}
			if seen[a] {
				t.Fatalf("aggregate %s twice in call %v", a, c)
			}
			seen[a] = true
		}
	}
	if len(st.published) != 5 || r.pub != 5 || r.pending != 0 {
		t.Errorf("published=%d rec=%+v", len(st.published), r)
	}
}

func TestBatchesOfTen(t *testing.T) {
	var evs []OutboxEvent
	for i := 1; i <= 25; i++ {
		evs = append(evs, ev(int64(i), fmt.Sprintf("agg%02d", i)))
	}
	st, sk := newStore(evs...), &fakeSink{}
	if err := NewPublisher(st, sk, nil, nil).Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(sk.sent) != 25 || len(sk.calls) != 3 {
		t.Fatalf("sent=%d calls=%d", len(sk.sent), len(sk.calls))
	}
}

func TestFailureHoldsBackOnlyItsAggregate(t *testing.T) {
	st := newStore(ev(1, "a"), ev(2, "a"), ev(3, "b"))
	sk := &fakeSink{failIDs: map[int64]error{1: errors.New("ThrottlingException")}}
	c := &clock{time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	r := &rec{}
	p := NewPublisher(st, sk, r, c.now)
	if err := p.Drain(t.Context()); err == nil {
		t.Fatal("want the failure reported")
	}
	if slices.Contains(sk.sent, 2) {
		t.Fatal("event 2 overtook the failed event 1 of its aggregate")
	}
	if !st.published[3] || st.published[1] || st.published[2] {
		t.Fatalf("published %v", st.published)
	}
	if st.attempts[1] != 1 || r.fail != 1 {
		t.Errorf("attempts=%d rec=%+v", st.attempts[1], r)
	}
	// still backing off: nothing is sent
	before := len(sk.calls)
	if err := p.Drain(t.Context()); err != nil || len(sk.calls) != before {
		t.Fatalf("sent during backoff: err=%v calls=%d->%d", err, before, len(sk.calls))
	}
	// after the backoff and with the bus healthy again, 1 then 2 go out
	c.t = c.t.Add(BackoffBase + time.Millisecond)
	sk.failIDs = nil
	if err := p.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !st.published[1] || !st.published[2] {
		t.Fatalf("published %v", st.published)
	}
	i1, i2 := slices.Index(sk.sent, 1), slices.Index(sk.sent, 2)
	if i1 < 0 || i2 < i1 {
		t.Errorf("order %v", sk.sent)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := Backoff(i + 1); got != w {
			t.Errorf("Backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	for _, n := range []int{10, 100, 1000} {
		if got := Backoff(n); got != BackoffMax {
			t.Errorf("Backoff(%d) = %v, want the %v cap", n, got, BackoffMax)
		}
	}
	if Backoff(0) != BackoffBase {
		t.Error("Backoff(0) must not be below the base")
	}
}

func TestWholeCallFailure(t *testing.T) {
	st := newStore(ev(1, "a"), ev(2, "b"))
	sk := &fakeSink{callErr: errors.New("bus unreachable")}
	if err := NewPublisher(st, sk, nil, nil).Drain(t.Context()); err == nil {
		t.Fatal("want error")
	}
	if len(st.published) != 0 || st.attempts[1] != 1 || st.attempts[2] != 1 {
		t.Fatalf("published=%v attempts=%v", st.published, st.attempts)
	}
}

func TestShortResultIsAFailureNotASuccess(t *testing.T) {
	st := newStore(ev(1, "a"))
	if err := NewPublisher(st, &fakeSink{short: true}, nil, nil).Drain(t.Context()); err == nil || st.published[1] {
		t.Fatalf("err=%v published=%v", err, st.published)
	}
}

func TestDuplicateAfterLostMarkIsHarmless(t *testing.T) {
	// The bus accepted the event but marking it failed: it is sent again next pass (at least once),
	// carrying the same payload and key so a consumer can dedupe on detail.id.
	st := newStore(ev(1, "a"))
	sk := &fakeSink{}
	p := NewPublisher(st, sk, nil, nil)
	st.markErr = errors.New("db down")
	if err := p.Drain(t.Context()); err == nil {
		t.Fatal("want error")
	}
	st.markErr = nil
	if err := p.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sk.sent, []int64{1, 1}) || !st.published[1] {
		t.Fatalf("sent %v published %v", sk.sent, st.published)
	}
}

func TestReadFailure(t *testing.T) {
	st := newStore()
	st.err = errors.New("down")
	if err := NewPublisher(st, &fakeSink{}, nil, nil).Drain(t.Context()); err == nil {
		t.Fatal("want error")
	}
}

func TestEntryShape(t *testing.T) {
	st := newStore(ev(7, "team_x"))
	var got []Entry
	sink := sinkFunc(func(es []Entry) ([]error, error) { got = es; return make([]error, len(es)), nil })
	if err := NewPublisher(st, sink, nil, nil).Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DetailType != "team.created" || got[0].Resources[0] != "team/team_x" || string(got[0].Detail) != `{"id":"evt"}` {
		t.Fatalf("entry %+v", got)
	}
}

type sinkFunc func([]Entry) ([]error, error)

func (f sinkFunc) PutEvents(_ context.Context, es []Entry) ([]error, error) { return f(es) }

// ---- partitions ----

type fakeParts struct {
	ensured map[string][]string
	dropped int
	ensErr  map[PartitionTable]error
	dropErr error
}

func (f *fakeParts) EnsurePartition(_ context.Context, t PartitionTable, m time.Time) (string, error) {
	if err := f.ensErr[t]; err != nil {
		return "", err
	}
	if f.ensured == nil {
		f.ensured = map[string][]string{}
	}
	f.ensured[string(t)] = append(f.ensured[string(t)], m.Format("200601"))
	return string(t) + "_" + m.Format("200601"), nil
}
func (f *fakeParts) DropOlderThan(_ context.Context, months int) ([]string, error) {
	if months != 13 {
		panic("retention must be 13 months")
	}
	f.dropped++
	return []string{"usage_events_202510"}, f.dropErr
}

func TestEnsureCreatesNextMonthEvenOnTheLastDay(t *testing.T) {
	tests := []struct {
		name string
		now  time.Time
		want []string
	}{
		{"mid month", time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC), []string{"202611", "202612", "202701"}},
		{"last day of month", time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC), []string{"202611", "202612", "202701"}},
		{"31 Jan into short February", time.Date(2027, 1, 31, 23, 59, 0, 0, time.UTC), []string{"202701", "202702", "202703"}},
		{"year end", time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC), []string{"202612", "202701", "202702"}},
		{"non UTC clock", time.Date(2026, 11, 30, 23, 0, 0, 0, time.FixedZone("x", -5*3600)), []string{"202612", "202701", "202702"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeParts{}
			m := &PartitionManager{Ops: f, Clock: func() time.Time { return tt.now }}
			if err := m.Ensure(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, tab := range Tables() {
				if !slices.Equal(f.ensured[string(tab)], tt.want) {
					t.Errorf("%s: %v, want %v", tab, f.ensured[string(tab)], tt.want)
				}
			}
		})
	}
}

func TestEnsureTriesEveryTableAfterAFailure(t *testing.T) {
	f := &fakeParts{ensErr: map[PartitionTable]error{TableUsage: errors.New("boom")}}
	m := &PartitionManager{Ops: f, Clock: time.Now}
	if err := m.Ensure(t.Context()); err == nil {
		t.Fatal("want error")
	}
	if len(f.ensured[string(TableActivity)]) != 3 || len(f.ensured[string(TableTraffic)]) != 3 {
		t.Errorf("other tables skipped: %v", f.ensured)
	}
}

func TestDropRunsOncePerMonth(t *testing.T) {
	f := &fakeParts{}
	now := time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC)
	m := &PartitionManager{Ops: f, Clock: func() time.Time { return now }}
	for i := 0; i < 3; i++ {
		if err := m.DropMonthly(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if f.dropped != 1 {
		t.Fatalf("dropped %d times in one month", f.dropped)
	}
	now = time.Date(2026, 12, 2, 0, 5, 0, 0, time.UTC) // missed the 1st: made up on the 2nd
	_ = m.DropMonthly(t.Context())
	if f.dropped != 2 {
		t.Fatalf("dropped %d times after the month turned", f.dropped)
	}
}

func TestDropFailureIsRetriedSameMonth(t *testing.T) {
	f := &fakeParts{dropErr: errors.New("lock timeout")}
	m := &PartitionManager{Ops: f, Clock: time.Now}
	if err := m.DropMonthly(t.Context()); err == nil {
		t.Fatal("want error")
	}
	f.dropErr = nil
	if err := m.DropMonthly(t.Context()); err != nil || f.dropped != 2 {
		t.Fatalf("err=%v dropped=%d", err, f.dropped)
	}
}

// ---- purge and registration ----

type fakePurger struct {
	n   int64
	err error
}

func (f *fakePurger) PurgeIdempotencyKeys(context.Context) (int64, error) { return f.n, f.err }

type fakeSched struct{ jobs map[string]time.Duration }

func (s *fakeSched) Every(name string, d time.Duration, _ func(context.Context) error) {
	if s.jobs == nil {
		s.jobs = map[string]time.Duration{}
	}
	s.jobs[name] = d
}

func TestPurgeAndRegistration(t *testing.T) {
	if err := (&Purge{Purger: &fakePurger{n: 3}}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := (&Purge{Purger: &fakePurger{err: errors.New("x")}}).Run(t.Context()); err == nil {
		t.Fatal("want error")
	}
	s := &fakeSched{}
	Register(s, Jobs(Deps{Outbox: newStore(), Sink: &fakeSink{}, Partitions: &fakeParts{}, Purger: &fakePurger{}}))
	want := map[string]time.Duration{
		"outbox-publisher": time.Second, "partitions-ensure": 24 * time.Hour,
		"partitions-drop": 24 * time.Hour, "idempotency-purge": 5 * time.Minute,
	}
	for k, v := range want {
		if s.jobs[k] != v {
			t.Errorf("job %s interval = %v, want %v", k, s.jobs[k], v)
		}
	}
	if len(s.jobs) != len(want) {
		t.Errorf("jobs = %v", s.jobs)
	}
}

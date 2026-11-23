package collab

import (
	"sync"
	"time"
)

// BucketSeconds is the width of an access bucket.
const BucketSeconds = 300

// BucketStart returns the start of the five-minute bucket containing t (UTC).
func BucketStart(t time.Time) time.Time {
	u := t.UTC().Unix()
	return time.Unix(u-u%BucketSeconds, 0).UTC()
}

// AccessKey identifies one counter of collaboration_access.
type AccessKey struct {
	Bucket  time.Time
	TeamID  string
	Surface Surface
	// Exposure is the entry level of the hostname the request came to: public or private.
	Exposure          string
	Kind              PrincipalKind
	PrincipalID       string
	Outcome           string
	ServiceAdminEntry bool
}

// Access is a counter.
type Access struct {
	Requests int
	Bytes    int64
}

// MaxBufferedKeys caps the unflushed counters (EDGE-7); beyond it new keys are dropped and counted.
const MaxBufferedKeys = 100000

// MaxFlushRows is the largest batch of one afs.record_collab_access call.
const MaxFlushRows = 1000

// AccessBuffer accumulates five-minute counts until they are flushed.
type AccessBuffer struct {
	mu      sync.Mutex
	counts  map[AccessKey]Access
	dropped int64
}

// NewAccessBuffer returns an empty buffer.
func NewAccessBuffer() *AccessBuffer { return &AccessBuffer{counts: map[AccessKey]Access{}} }

// Add counts one request of the given size at time t.
func (b *AccessBuffer) Add(t time.Time, k AccessKey, bytes int64) {
	k.Bucket = BucketStart(t)
	b.mu.Lock()
	defer b.mu.Unlock()
	cur, ok := b.counts[k]
	if !ok && len(b.counts) >= MaxBufferedKeys {
		b.dropped++
		return
	}
	cur.Requests++
	cur.Bytes += bytes
	b.counts[k] = cur
}

// Dropped returns how many counters were dropped because the buffer was full.
func (b *AccessBuffer) Dropped() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

// Len is the number of buffered keys.
func (b *AccessBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.counts)
}

// Row is one flushed counter.
type Row struct {
	Key AccessKey
	Access
}

// Take removes and returns up to MaxFlushRows counters. A failed flush hands them back with Restore.
func (b *AccessBuffer) Take() []Row {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Row
	for k, v := range b.counts {
		out = append(out, Row{Key: k, Access: v})
		delete(b.counts, k)
		if len(out) == MaxFlushRows {
			break
		}
	}
	return out
}

// Restore merges rows back after a failed flush (the batch that failed is retried).
func (b *AccessBuffer) Restore(rows []Row) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range rows {
		cur, ok := b.counts[r.Key]
		if !ok && len(b.counts) >= MaxBufferedKeys {
			b.dropped += int64(r.Requests)
			continue
		}
		cur.Requests += r.Requests
		cur.Bytes += r.Bytes
		b.counts[r.Key] = cur
	}
}

// Limiter is a token bucket per principal: Rate requests per second with a burst (20 and 40, EDGE-5).
type Limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*tokens
	now     func() time.Time
}

type tokens struct {
	n    float64
	last time.Time
}

// NewLimiter returns a limiter; now is the clock (time.Now when nil).
func NewLimiter(rate, burst float64, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{rate: rate, burst: burst, buckets: map[string]*tokens{}, now: now}
}

// Allow takes a token for key; when none is left it returns the seconds to wait (at least 1).
func (l *Limiter) Allow(key string) (ok bool, retryAfter int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	b, found := l.buckets[key]
	if !found {
		b = &tokens{n: l.burst, last: t}
		l.buckets[key] = b
	}
	b.n += t.Sub(b.last).Seconds() * l.rate
	if b.n > l.burst {
		b.n = l.burst
	}
	b.last = t
	if b.n >= 1 {
		b.n--
		return true, 0
	}
	wait := int((1-b.n)/l.rate) + 1
	return false, wait
}

// Sweep drops buckets idle for longer than idle, so memory follows the active principals.
func (l *Limiter) Sweep(idle time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := l.now().Add(-idle)
	for k, b := range l.buckets {
		if b.last.Before(cut) {
			delete(l.buckets, k)
		}
	}
}

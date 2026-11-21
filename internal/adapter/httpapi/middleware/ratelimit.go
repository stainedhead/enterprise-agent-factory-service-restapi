package middleware

import (
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Rate limits (API contract §1): 20 requests per second per principal with a burst of 40. The limit
// is per replica, so each replica takes its share: rate and burst are divided by AFS_API_REPLICAS.
const (
	DefaultRatePerSecond = 20
	DefaultBurst         = 40

	maxBuckets  = 100_000
	idleEvicted = time.Minute
	sweepEvery  = 30 * time.Second
	overflowKey = "*overflow"
)

// LimiterConfig configures a Limiter. The zero value is the contract default on one replica.
type LimiterConfig struct {
	RatePerSecond float64
	Burst         float64
	// Replicas divides rate and burst; values below 1 mean 1.
	Replicas int
	// Now returns the current time; default time.Now.
	Now func() time.Time
}

// Limiter is a token bucket per key with idle eviction. Keys are "principal id" or "source address"
// strings chosen by the caller; a flood of distinct keys cannot grow it past a fixed bound.
type Limiter struct {
	rate, burst float64
	now         func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLimiter returns a Limiter.
func NewLimiter(c LimiterConfig) *Limiter {
	if c.RatePerSecond <= 0 {
		c.RatePerSecond = DefaultRatePerSecond
	}
	if c.Burst <= 0 {
		c.Burst = DefaultBurst
	}
	if c.Replicas < 1 {
		c.Replicas = 1
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	n := float64(c.Replicas)
	return &Limiter{rate: c.RatePerSecond / n, burst: math.Max(1, math.Floor(c.Burst/n)), now: c.Now, buckets: map[string]*bucket{}}
}

// Allow takes one token from key's bucket. When there is none it returns false and how long until
// one is available.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxBuckets {
			key = overflowKey
			b = l.buckets[key]
		}
		if b == nil {
			b = &bucket{tokens: l.burst, last: now}
			l.buckets[key] = b
		}
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed.Seconds()*l.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

// sweep drops buckets idle long enough to have refilled completely.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.last) >= idleEvicted {
			delete(l.buckets, k)
		}
	}
}

// Size returns the number of live buckets (tests and metrics).
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Throttled returns the 429 error for a caller that must wait; Retry-After is whole seconds, at
// least one.
func Throttled(retryAfter time.Duration) *domain.Error {
	secs := int(math.Ceil(retryAfter.Seconds()))
	if secs < 1 {
		secs = 1
	}
	return &domain.Error{Kind: domain.KindThrottled, Message: "too many requests", RetryAfterSeconds: secs}
}

// PublicLimit limits requests to public routes (no credential to key on) per source address with the
// same rate as authenticated callers (RA-US-15.1). Everything else passes: authenticated routes are
// limited per principal, and failed authentication per source address, by Authn.
func PublicLimit(l *Limiter, routes RouteLookup, errs ErrorWriter) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if info, ok := routes(r); ok && info.Public {
				if ok, wait := l.Allow(sourceKey(r)); !ok {
					errs(w, r, Throttled(wait))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func sourceKey(r *http.Request) string {
	ip := MetaFrom(r.Context()).IP
	if ip == "" {
		ip = ClientIP(r, 0)
	}
	return "ip:" + ip
}

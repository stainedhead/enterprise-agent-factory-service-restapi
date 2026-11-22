package edge

import (
	"container/list"
	"net"
	"strings"
	"time"
)

// Cache bounds (finding 7): every cache has a size limit, so random credentials cannot grow memory, and the
// per-source limit on lookups of credentials the database does not know keeps them from reaching it.
const (
	// DefaultCacheSize is the most entries each of the people, session and key caches holds.
	DefaultCacheSize = 10000
	// DefaultUnknownPerMinute is how many lookups of unknown credentials one source address may cause per
	// minute before further lookups are refused with 429 without touching the database.
	DefaultUnknownPerMinute = 20
	unknownWindow           = time.Minute
)

// lru is a size-bounded least-recently-used map. It is not safe for concurrent use; Service.mu guards it.
type lru[K comparable, V any] struct {
	max int
	ll  *list.List
	m   map[K]*list.Element
}

type lruEntry[K comparable, V any] struct {
	k K
	v V
}

func newLRU[K comparable, V any](max int) *lru[K, V] {
	if max < 1 {
		max = 1
	}
	return &lru[K, V]{max: max, ll: list.New(), m: map[K]*list.Element{}}
}

func (c *lru[K, V]) get(k K) (V, bool) {
	if e, ok := c.m[k]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*lruEntry[K, V]).v, true
	}
	var zero V
	return zero, false
}

func (c *lru[K, V]) put(k K, v V) {
	if e, ok := c.m[k]; ok {
		e.Value.(*lruEntry[K, V]).v = v
		c.ll.MoveToFront(e)
		return
	}
	c.m[k] = c.ll.PushFront(&lruEntry[K, V]{k, v})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.m, last.Value.(*lruEntry[K, V]).k)
	}
}

func (c *lru[K, V]) delete(k K) {
	if e, ok := c.m[k]; ok {
		c.ll.Remove(e)
		delete(c.m, k)
	}
}

func (c *lru[K, V]) len() int { return c.ll.Len() }

type missWindow struct {
	start time.Time
	n     int
}

// unknownBlocked reports whether the source has used up its lookups of unknown credentials this minute.
// The caller holds s.mu.
func (s *Service) unknownBlocked(ip string) bool {
	w, ok := s.unknown.get(ip)
	return ok && s.now().Sub(w.start) < unknownWindow && w.n >= s.unknownLimit()
}

// noteUnknown counts one lookup that found nothing. The caller holds s.mu.
func (s *Service) noteUnknown(ip string) {
	w, ok := s.unknown.get(ip)
	if !ok || s.now().Sub(w.start) >= unknownWindow {
		w = missWindow{start: s.now()}
	}
	w.n++
	s.unknown.put(ip, w)
}

func (s *Service) unknownLimit() int {
	if s.UnknownPerMinute > 0 {
		return s.UnknownPerMinute
	}
	return DefaultUnknownPerMinute
}

// sourceIP is the host part of the connection's remote address.
func sourceIP(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// NormalizeHost lower-cases a Host header, strips the port and a trailing dot. It is the one place a Host is
// normalized (finding 13).
func NormalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.TrimRight(h, ".")
}

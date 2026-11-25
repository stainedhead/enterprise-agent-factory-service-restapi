package gatewayhttp

import (
	"net/http"
	"strings"
	"sync"
)

// Family is one group of gateway paths (Bedrock and Anthropic, model routes, and later MCP). It mounts
// its handlers on the gateway's Mux; the Mux adds nothing but dispatch, so every family keeps its own
// wire format, errors and metering.
type Family interface {
	Mount(m *Mux)
}

// FamilyFunc adapts a function to Family.
type FamilyFunc func(m *Mux)

// Mount implements Family.
func (f FamilyFunc) Mount(m *Mux) { f(m) }

// Mux is the composable router of a gateway's listener. A family registers an exact path or a prefix
// (ending in "/"); the longest match wins, and anything unmatched goes to the fallback, which answers the
// gateway contract's 404. It deliberately does not use http.ServeMux patterns: the Bedrock path carries
// an escaped model id that ServeMux would clean or redirect.
type Mux struct {
	mu       sync.RWMutex
	exact    map[string]http.Handler
	prefixes []prefixRoute
	fallback http.Handler
}

type prefixRoute struct {
	prefix string
	h      http.Handler
}

// NewMux returns a Mux whose unmatched requests go to fallback.
func NewMux(fallback http.Handler) *Mux {
	return &Mux{exact: map[string]http.Handler{}, fallback: fallback}
}

// Handle registers h for path: an exact path, or every path under it when it ends in "/". A second
// registration of the same path panics, because two families claiming one path is a wiring bug.
func (m *Mux) Handle(path string, h http.Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.HasSuffix(path, "/") {
		for _, p := range m.prefixes {
			if p.prefix == path {
				panic("gatewayhttp: prefix registered twice: " + path)
			}
		}
		m.prefixes = append(m.prefixes, prefixRoute{prefix: path, h: h})
		return
	}
	if _, dup := m.exact[path]; dup {
		panic("gatewayhttp: path registered twice: " + path)
	}
	m.exact[path] = h
}

// Handler returns the handler for an escaped path.
func (m *Mux) Handler(escaped string) http.Handler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if h, ok := m.exact[escaped]; ok {
		return h
	}
	var best prefixRoute
	for _, p := range m.prefixes {
		if strings.HasPrefix(escaped, p.prefix) && len(p.prefix) > len(best.prefix) {
			best = p
		}
	}
	if best.h != nil {
		return best.h
	}
	return m.fallback
}

// ServeHTTP dispatches on the escaped path.
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.Handler(r.URL.EscapedPath()).ServeHTTP(w, r)
}

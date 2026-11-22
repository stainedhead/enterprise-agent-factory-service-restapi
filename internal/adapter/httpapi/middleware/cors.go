package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// CORS headers of API contract §1. There is no credentials mode: tokens travel in Authorization.
const (
	corsAllowedHeaders = "Authorization, Content-Type, If-Match, Idempotency-Key, X-Request-Id"
	corsExposedHeaders = "ETag, X-Request-Id, Idempotent-Replayed, Retry-After, X-Afs-Contract-Version, X-Afs-Next-Token"
	corsAllowedMethods = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsPreflightAge   = "600"

	// originsTTL is how long the api.cors_origins setting is reused, so an origin added by a service
	// admin is honored within half a minute without a database read per browser request.
	originsTTL = 30 * time.Second
)

// OriginSource returns the allowed browser origins (service setting api.cors_origins).
type OriginSource func(ctx context.Context) ([]string, error)

// CORS answers preflight requests and adds the CORS response headers for requests from an origin
// listed in api.cors_origins (default: none). A request from any other origin gets no CORS headers, so
// a browser refuses to expose the response; a request without Origin is untouched (RA-US-15.2,
// CT-API-4).
func CORS(origins OriginSource, now func() time.Time, log *slog.Logger) Middleware {
	if now == nil {
		now = time.Now
	}
	c := &corsCache{src: origins, now: now, log: log}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Add("Vary", "Origin")
			if !c.allowed(r.Context(), origin) {
				next.ServeHTTP(w, r)
				return
			}
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Expose-Headers", corsExposedHeaders)
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				h.Set("Access-Control-Allow-Methods", corsAllowedMethods)
				h.Set("Access-Control-Allow-Headers", corsAllowedHeaders)
				h.Set("Access-Control-Max-Age", corsPreflightAge)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type corsCache struct {
	src OriginSource
	now func() time.Time
	log *slog.Logger

	mu      sync.Mutex
	origins []string
	at      time.Time
	loaded  bool
}

func (c *corsCache) allowed(ctx context.Context, origin string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded || c.now().Sub(c.at) >= originsTTL {
		list, err := c.src(ctx)
		switch {
		case err == nil:
			c.origins, c.loaded = list, true
		case c.loaded:
			// Keep the last known list while the database is down.
		default:
			c.origins, c.loaded = nil, true
			if c.log != nil {
				c.log.WarnContext(ctx, "api.cors_origins unreadable; no browser origin is allowed", "err", err)
			}
		}
		c.at = c.now()
	}
	return origin != "null" && !strings.ContainsAny(origin, " ,") && slices.Contains(c.origins, origin)
}

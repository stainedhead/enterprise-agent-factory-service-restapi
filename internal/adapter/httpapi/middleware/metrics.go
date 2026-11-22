// Package middleware holds the afs-api HTTP middleware that lives outside the router: RED metrics
// here, idempotency, If-Match, rate limiting, CORS and denied-audit in their own files.
package middleware

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
)

type routeKey struct{}

// routeHolder is filled by the router once it has matched the operation, and read by Metrics after
// the handler returns.
type routeHolder struct{ name string }

// SetRoute records the operation (the OpenAPI operationId, e.g. "ListTeams") of the current
// request. The router calls it once the route is matched, so metrics carry the exact operation; a
// request that never matches is labelled "unmatched".
func SetRoute(ctx context.Context, operationID string) {
	if h, ok := ctx.Value(routeKey{}).(*routeHolder); ok {
		h.name = operationID
	}
}

// Metrics records RED metrics per route: requests by route, method and status code, duration, and
// the in-flight count. When the router never calls SetRoute it falls back to the matched ServeMux
// pattern, and failing that to the path with identifiers collapsed, so the label cardinality stays
// bounded either way. The route label is never a raw path.
func Metrics(m *observability.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		holder := &routeHolder{}
		r = r.WithContext(context.WithValue(r.Context(), routeKey{}, holder))
		sw := &statusWriter{ResponseWriter: w}
		m.APIInFlight.Inc()
		defer func() {
			m.APIInFlight.Dec()
			route := holder.name
			code := sw.code
			if code == 0 {
				code = http.StatusOK
			}
			if route == "" {
				route = fallbackRoute(r)
				if r.Pattern == "" && code == http.StatusNotFound {
					// An unknown path must not mint a label per probe.
					route = "unmatched"
				}
			}
			m.APIRequests.WithLabelValues(route, r.Method, strconv.Itoa(code)).Inc()
			m.APIRequestDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
		}()
		next.ServeHTTP(sw, r)
	})
}

// statusWriter captures the status code and keeps http.Flusher working for streaming responses.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) {
	if s.code == 0 {
		s.code = c
	}
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

var idSegment = regexp.MustCompile(`^(?:[a-z]{2,5}_[0-9A-Za-z]{10,}|[0-9]+|[0-9a-fA-F-]{16,})$`)

// fallbackRoute labels a request whose operation is unknown: the ServeMux pattern when the request
// carries one, otherwise the path with identifier-looking segments replaced by "{id}".
func fallbackRoute(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		switch r.URL.Path {
		case "/healthz", "/readyz", "/metrics":
			return r.URL.Path
		}
		return "unmatched"
	}
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	for i, s := range segs {
		if idSegment.MatchString(s) {
			segs[i] = "{id}"
		}
	}
	// A bounded depth keeps a hostile path from minting labels.
	if len(segs) > 6 {
		segs = segs[:6]
	}
	return "/" + strings.Join(segs, "/")
}

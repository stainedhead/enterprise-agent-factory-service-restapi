// Package middleware holds the request pipeline of afs-api that sits around the handlers (RA-4,
// RA-15): bearer authentication with rate limits, CORS, idempotency, If-Match and ETag rules, and the
// denied-request audit. It does not import package httpapi (which assembles the chain), so it takes the
// few things it needs from there as small function values: the route being served, the error writer
// and the authenticated principal.
package middleware

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
)

// Middleware wraps a handler.
type Middleware = func(http.Handler) http.Handler

// ErrorWriter writes err as the contract §6 error envelope (httpapi.WriteError).
type ErrorWriter func(w http.ResponseWriter, r *http.Request, err error)

// RouteInfo is what the middleware needs to know about the route a request was matched to.
type RouteInfo struct {
	OperationID string
	Method      string
	// Public routes need no credential.
	Public bool
	// Idempotent routes accept Idempotency-Key (creating POSTs and :verb actions).
	Idempotent bool
	// Mutation routes change state.
	Mutation bool
}

// RouteLookup returns the route of the request being served (httpapi.RouteFrom).
type RouteLookup func(r *http.Request) (RouteInfo, bool)

// PrincipalLookup returns the authenticated principal of a request (httpapi.PrincipalFrom).
type PrincipalLookup func(ctx context.Context) (authz.Principal, bool)

type ctxKey int

const (
	keyMeta ctxKey = iota
	keyClaim
)

// RequestMeta is where a request came from, for audit records and per-source limits.
type RequestMeta struct {
	// IP is the client address: the socket peer, or the forwarded address when trusted proxies are
	// configured. Empty when unknown.
	IP        string
	UserAgent string
}

// MetaFrom returns the request metadata set by WithRequestMeta.
func MetaFrom(ctx context.Context) RequestMeta {
	m, _ := ctx.Value(keyMeta).(RequestMeta)
	return m
}

// WithRequestMeta records the client address and user agent in the request context. trustedHops is
// the number of reverse proxies in front of afs-api that append to X-Forwarded-For; 0 trusts only the
// socket peer, so a client cannot choose its own address.
func WithRequestMeta(trustedHops int) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m := RequestMeta{IP: ClientIP(r, trustedHops), UserAgent: truncate(r.UserAgent(), 256)}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyMeta, m)))
		})
	}
}

// ClientIP returns the client address of r. With trustedHops proxies in front, it is the entry that
// many places from the right of X-Forwarded-For; entries further left are client-controlled and
// ignored. A malformed or missing entry falls back to the socket peer.
func ClientIP(r *http.Request, trustedHops int) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if trustedHops > 0 {
		var hops []string
		for _, h := range r.Header.Values("X-Forwarded-For") {
			for _, p := range strings.Split(h, ",") {
				hops = append(hops, strings.TrimSpace(p))
			}
		}
		if len(hops) >= trustedHops {
			if ip := net.ParseIP(hops[len(hops)-trustedHops]); ip != nil {
				return ip.String()
			}
		}
	}
	if ip := net.ParseIP(peer); ip != nil {
		return ip.String()
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

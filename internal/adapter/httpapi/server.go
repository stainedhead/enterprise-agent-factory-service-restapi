// Package httpapi contains the HTTP handlers, routing, request validation and the authorization
// middleware for afs-api. Every /v1 route declares the authz.Permission it requires.
package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/api"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// ReadinessCheck reports whether a dependency (for example the database) is ready.
type ReadinessCheck func(*http.Request) error

// Config assembles the afs-api handler.
type Config struct {
	// Auth authenticates callers; nil refuses every non-public route with 401.
	Auth Authenticator
	// Teams supplies team flags for conditional grants; nil means all flags off.
	Teams TeamLookup
	// Hooks observe denials and cross-team 404s.
	Hooks Hooks
	// TeamState feeds the suspended-team guard (M1-B7); nil disables it.
	TeamState TeamStateLookup
	// OnRoute, when set, is called with the operationId once a request matched a route, before any
	// middleware of the route runs (RED metrics label by it).
	OnRoute func(ctx context.Context, operationID string)
	// Ready maps a dependency name to its readiness check (GetReadiness).
	Ready map[string]ReadinessCheck
	// Registrars bind handlers: the explicit list of handler areas the caller wires (FR-R15). There is no
	// package-level default list.
	Registrars []Registrar
	// AllowUnbound lets New succeed when some contract operations have no handler; they answer 501. Only
	// development without a database and tests that exercise part of the API set it. Without it New returns
	// an error naming every unbound operation, so a forgotten Bind cannot ship.
	AllowUnbound bool
	// Outer wrap the whole router, inside the request id and panic recovery: request metadata and CORS
	// preflight, which no route answers. The first element is the outermost.
	Outer []Middleware
	// PreAuth wrap each operation before authentication, after strict query checking and the body
	// limit: limits on routes that carry no credential. The route is available to them (RouteFrom).
	PreAuth []Middleware
	// PostAuth wrap each operation's handler after authorization has allowed the call: the suspended
	// team guard, denied-request audit, If-Match, ETag and Idempotency-Key handling. The first element
	// is the outermost.
	PostAuth []Middleware
	// IdempotencyHMACKey keys the HMAC-SHA-256 that fingerprints secret values in idempotency claims
	// (at least 32 bytes, the same on every replica). The secrets handlers read it; it is never logged.
	IdempotencyHMACKey []byte
	// Now and Entropy feed request ids; defaults are time.Now and crypto/rand.
	Now     func() time.Time
	Entropy io.Reader
}

// Middleware wraps a handler.
type Middleware = func(http.Handler) http.Handler

// chain wraps h in ms, the first being outermost.
func chain(h http.Handler, ms []Middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

// NewHandler returns the root handler with a single readiness check named database. It is kept
// for callers that need no other configuration. It binds nothing else, so it sets AllowUnbound: every
// other operation answers 501.
func NewHandler(ready ReadinessCheck) http.Handler {
	cfg := Config{AllowUnbound: true}
	if ready != nil {
		cfg.Ready = map[string]ReadinessCheck{"database": ready}
	}
	h, err := New(cfg)
	if err != nil {
		panic(err) // the route table is generated; an error here is a build defect
	}
	return h
}

// New returns the afs-api root handler: every contract operation, GET /v1/openapi.yaml, and the
// middleware chain (request id and contract version, panic recovery, body limit, strict query,
// authorization).
func New(cfg Config) (http.Handler, error) {
	ops, err := buildOperations()
	if err != nil {
		return nil, err
	}
	reg := &Registry{ops: ops, bound: map[string]bool{}}
	if err := reg.Handle("GetHealth", http.HandlerFunc(health)); err != nil {
		return nil, err
	}
	if err := reg.Handle("GetReadiness", readiness(cfg.Ready)); err != nil {
		return nil, err
	}
	for _, register := range cfg.Registrars {
		if err := register(reg); err != nil {
			return nil, err
		}
	}
	if !cfg.AllowUnbound {
		if missing := reg.Unbound(); len(missing) > 0 {
			return nil, fmt.Errorf("httpapi: %d unbound operations (set Config.AllowUnbound only for development or partial tests): %s",
				len(missing), strings.Join(missing, ", "))
		}
	}
	mux := http.NewServeMux()
	mount(mux, ops, func(op *operation) http.Handler {
		inner := chain(authorize(op, cfg.Auth, cfg.Teams, cfg.Hooks, suspendedGuard(op, cfg.TeamState, chain(op.handler, cfg.PostAuth))), cfg.PreAuth)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(context.WithValue(r.Context(), keyRoute, op))
			if cfg.OnRoute != nil {
				cfg.OnRoute(r.Context(), op.OperationID)
			}
			if err := checkQuery(op, r); err != nil {
				WriteError(w, r, err)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, op.bodyLimit)
			inner.ServeHTTP(w, r)
		})
	})
	mux.HandleFunc("GET /v1/openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(api.OpenAPI)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, domain.NotFound("route"))
	})
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	entropy := cfg.Entropy
	if entropy == nil {
		entropy = defaultEntropy()
	}
	return withRequestID(now, entropy, withRecovery(chain(mux, cfg.Outer))), nil
}

type healthStatus struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthStatus{Status: "ok"})
}

func readiness(checks map[string]ReadinessCheck) http.Handler {
	names := make([]string, 0, len(checks))
	for n := range checks {
		names = append(names, n)
	}
	sort.Strings(names)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := healthStatus{Status: "ready", Checks: map[string]string{}}
		code := http.StatusOK
		for _, n := range names {
			if err := checks[n](r); err != nil {
				// The cause (driver text can name the login role and host) goes to the log only.
				slog.ErrorContext(r.Context(), "readiness check failed", "check", n, "request_id", RequestIDFrom(r.Context()), "err", err)
				out.Checks[n] = "unavailable"
				out.Status, code = "not ready", http.StatusServiceUnavailable
				continue
			}
			out.Checks[n] = "ok"
		}
		writeJSON(w, code, out)
	})
}

// Package httpapi contains the HTTP handlers, routing, request validation and the authorization
// middleware for afs-api. Every /v1 route declares the authz.Permission it requires.
package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	// Ready maps a dependency name to its readiness check (GetReadiness).
	Ready map[string]ReadinessCheck
	// Registrars bind handlers; the default is Areas.
	Registrars []Registrar
	// Now and Entropy feed request ids; defaults are time.Now and crypto/rand.
	Now     func() time.Time
	Entropy io.Reader
}

// NewHandler returns the root handler with a single readiness check named database. It is kept
// for callers that need no other configuration.
func NewHandler(ready ReadinessCheck) http.Handler {
	cfg := Config{}
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
	registrars := cfg.Registrars
	if registrars == nil {
		for _, a := range Areas {
			registrars = append(registrars, a.Register)
		}
	}
	for _, register := range registrars {
		if err := register(reg); err != nil {
			return nil, err
		}
	}
	mux := http.NewServeMux()
	mount(mux, ops, func(op *operation) http.Handler {
		inner := authorize(op, cfg.Auth, cfg.Teams, cfg.Hooks, op.handler)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(context.WithValue(r.Context(), keyRoute, op))
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
	return withRequestID(now, entropy, withRecovery(mux)), nil
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
				out.Checks[n] = fmt.Sprint(err)
				out.Status, code = "not ready", http.StatusServiceUnavailable
				continue
			}
			out.Checks[n] = "ok"
		}
		writeJSON(w, code, out)
	})
}

// Package httpapi contains the HTTP handlers, routing, request validation and the authorization
// middleware for afs-api. Every /v1 route declares the authz.Permission it requires.
package httpapi

import (
	"encoding/json"
	"net/http"
)

// ReadinessCheck reports whether a dependency (for example the database) is ready.
type ReadinessCheck func(*http.Request) error

// NewHandler returns the root handler for afs-api.
func NewHandler(ready ReadinessCheck) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready != nil {
			if err := ready(r); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

package middleware

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Denied is a refused request to audit.
type Denied struct {
	Principal   authz.Principal
	OperationID string
	TeamID      string
	Required    authz.Permission
}

// DeniedAuditor writes the audit record of every 403 (RA-US-1.7) and counts the 404s caused by a
// missing binding (RA-US-6.3, afs_api_cross_team_not_found_total).
type DeniedAuditor struct {
	Audit port.AuditWriter
	// Session builds the database session of a principal (auth.SessionOf).
	Session func(p authz.Principal, requestID string) port.Session
	// RequestID returns the request id of ctx.
	RequestID func(ctx context.Context) string
	Log       *slog.Logger

	crossTeam atomic.Int64
}

func (d *DeniedAuditor) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// Record writes one denied audit record in its own transaction: the request's own transaction, if it
// had one, rolled back, and a denial must be recorded either way. A failure to write is logged and
// never changes the 403.
func (d *DeniedAuditor) Record(ctx context.Context, x Denied) {
	meta := MetaFrom(ctx)
	rec := port.AuditRecord{
		Action:    x.OperationID,
		TeamID:    x.TeamID,
		SourceIP:  meta.IP,
		UserAgent: meta.UserAgent,
		After:     map[string]any{"requiredPermission": string(x.Required)},
	}
	if x.TeamID != "" {
		rec.ResourceKind, rec.ResourceID = "teams", x.TeamID
	}
	if err := d.Audit.WriteDenied(ctx, d.Session(x.Principal, d.RequestID(ctx)), rec); err != nil {
		d.log().ErrorContext(ctx, "denied audit not written", "op", x.OperationID, "err", err)
	}
}

// CrossTeamNotFound counts a 404 caused by a missing binding. Many of them from one caller is the
// signature of someone probing for teams.
func (d *DeniedAuditor) CrossTeamNotFound(ctx context.Context, op string) {
	d.crossTeam.Add(1)
	d.log().InfoContext(ctx, "cross-team not found", "metric", "afs_api_cross_team_not_found_total", "op", op)
}

// CrossTeamCount is the value of afs_api_cross_team_not_found_total.
func (d *DeniedAuditor) CrossTeamCount() int64 { return d.crossTeam.Load() }

// Stage audits the 403s that handlers and use cases produce (a team-admin changing a guardrail field,
// for example), which the route-level check cannot see. The 403 is held until its audit record is
// written, so a client that sees the response can rely on the record existing.
func (d *DeniedAuditor) Stage(routes RouteLookup, principal PrincipalLookup) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hw := &denialWriter{ResponseWriter: w}
			next.ServeHTTP(hw, r)
			if !hw.held {
				return
			}
			if p, ok := principal(r.Context()); ok {
				x := Denied{Principal: p, Required: hw.required()}
				if info, ok := routes(r); ok {
					x.OperationID = info.OperationID
				}
				x.TeamID = r.PathValue("teamId")
				d.Record(r.Context(), x)
			}
			hw.release()
		})
	}
}

// denialWriter passes every response through except a 403, which it holds.
type denialWriter struct {
	http.ResponseWriter
	held bool
	body []byte
}

func (d *denialWriter) WriteHeader(code int) {
	if code == http.StatusForbidden {
		d.held = true
		return
	}
	d.ResponseWriter.WriteHeader(code)
}

func (d *denialWriter) Write(p []byte) (int, error) {
	if d.held {
		if len(d.body)+len(p) <= 64<<10 {
			d.body = append(d.body, p...)
		}
		return len(p), nil
	}
	return d.ResponseWriter.Write(p)
}

func (d *denialWriter) Flush() {
	if !d.held {
		if f, ok := d.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (d *denialWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }

func (d *denialWriter) release() {
	d.ResponseWriter.WriteHeader(http.StatusForbidden)
	_, _ = d.ResponseWriter.Write(d.body)
}

// required reads details.requiredPermission from the held error envelope.
func (d *denialWriter) required() authz.Permission {
	var env struct {
		Details struct {
			Required string `json:"requiredPermission"`
		} `json:"details"`
	}
	if json.Unmarshal(d.body, &env) != nil {
		return ""
	}
	return authz.Permission(env.Details.Required)
}

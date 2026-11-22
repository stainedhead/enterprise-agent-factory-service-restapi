package edgehttp

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/edge"
)

// messages are the human texts of the status table (API contract 4.4c); they carry no team detail.
var messages = map[string]string{ //nolint:gosec // texts of error reasons, not credentials
	"ambiguous_credentials": "Send either the session cookie or X-Afs-Authorization, not both.",
	"malformed_credential":  "The X-Afs-Authorization header must be \"Bearer <token>\".",
	"body_too_large":        "The request body is larger than the limit set for collaboration.",
	"unsupported_encoding":  "A request body with a content encoding cannot be scanned. Send it without Content-Encoding.",
	"token_invalid":         "The credential is missing or not valid.",
	"token_expired":         "The token has expired. Sign in again.",
	"key_expired":           "The API key has expired.",
	"key_revoked":           "The API key has been revoked.",
	"session_expired":       "Your session has ended. Sign in again.",
	"no_team_role":          "You have no role on this team.",
	"permission_required":   "Your role does not allow this request.",
	"surface_disabled":      "This way into the team's collaboration is switched off.",
	"paused":                "The team's collaboration agent is paused.",
	"team_suspended":        "The team is suspended.",
	"starting":              "The team's collaboration agent is starting. Try again shortly.",
	"upstream_unavailable":  "The collaboration service is not reachable. Try again shortly.",
}

var defaultMessages = map[string]string{ //nolint:gosec // texts of error codes, not credentials
	"ValidationError": "The request is not valid.",
	"Unauthenticated": "Authentication is required.",
	"AccessDenied":    "Access is denied.",
	"NotFound":        "Not found.",
	"Throttled":       "Too many requests. Slow down.",
	"Unavailable":     "The service is not available.",
	"SecretDetected":  "A credential was found in the request. Store it as an AFS secret instead of sending it to Paperclip.",
}

type envelope struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"requestId"`
	Details   map[string]any `json:"details,omitempty"`
}

func messageOf(r edge.Result) string {
	if r.Message != "" {
		return r.Message
	}
	if m, ok := messages[r.Reason]; ok {
		return m
	}
	return defaultMessages[r.Code]
}

func detailsOf(r edge.Result) map[string]any {
	d := map[string]any{}
	if r.Reason != "" {
		d["reason"] = r.Reason
	}
	if r.Required != "" {
		d["requiredPermission"] = r.Required
	}
	if len(r.Patterns) > 0 {
		d["patterns"] = r.Patterns
	}
	if len(d) == 0 {
		return nil
	}
	return d
}

var page = template.Must(template.New("error").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Status}} {{.Code}}</title>
<style>body{font:16px/1.5 system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;background:#f6f7f8;color:#16191f}
main{max-width:34rem;padding:2rem;background:#fff;border:1px solid #d5dbdb;border-radius:4px}
h1{font-size:1.25rem;margin:0 0 .5rem}code{background:#f1f3f3;padding:0 .25rem}p{margin:.5rem 0}</style></head>
<body><main role="main"><h1>{{.Status}} {{.Code}}</h1><p>{{.Message}}</p>
{{if .Reason}}<p>Reason: <code>{{.Reason}}</code></p>{{end}}
{{if .Retry}}<p>Try again in about {{.Retry}} seconds.</p>{{end}}
<p>Request id: <code>{{.RequestID}}</code></p></main></body></html>`))

// securityHeaders are added to every response the edge writes or passes through.
func securityHeaders(h http.Header) {
	h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	csp := h.Get("Content-Security-Policy")
	switch {
	case csp == "":
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	case !containsFold(csp, "frame-ancestors"):
		h.Set("Content-Security-Policy", csp+"; frame-ancestors 'none'")
	}
}

func containsFold(s, sub string) bool { return strings.Contains(strings.ToLower(s), sub) }

// writeError answers a refused request: the AFS envelope for API traffic, an HTML page for navigation.
func writeError(w http.ResponseWriter, r *http.Request, res edge.Result, requestID string, navigate bool) {
	h := w.Header()
	securityHeaders(h)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Request-Id", requestID)
	if res.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(res.RetryAfter))
	}
	if navigate {
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(res.Status)
		if r.Method == http.MethodHead {
			return
		}
		_ = page.Execute(w, map[string]any{"Status": res.Status, "Code": res.Code, "Message": messageOf(res), "Reason": res.Reason,
			"Retry": res.RetryAfter, "RequestID": requestID})
		return
	}
	h.Set("Content-Type", "application/json")
	w.WriteHeader(res.Status)
	if r.Method == http.MethodHead {
		return
	}
	_ = json.NewEncoder(w).Encode(envelope{Code: res.Code, Message: messageOf(res), RequestID: requestID, Details: detailsOf(res)})
}

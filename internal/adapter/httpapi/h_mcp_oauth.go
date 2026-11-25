package httpapi

import (
	"net"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/middleware"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
)

// mcpOAuthStartBody is McpOAuthStart.
type mcpOAuthStartBody struct {
	AuthorizationURL string `json:"authorizationUrl"`
	State            string `json:"state"`
	ExpiresAt        string `json:"expiresAt"`
}

// startOAuth serves StartMcpOAuthConnect. Each call makes a new single-use state, so an Idempotency-Key is accepted
// and not replayed: a retry gets a fresh authorization URL, and the earlier state expires unused.
func (a *McpAPI) startOAuth(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.Mcp.StartOAuth(r.Context(), c, r.PathValue("teamId"), r.PathValue("mcpServerId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, http.StatusOK, mcpOAuthStartBody{AuthorizationURL: out.AuthorizationURL, State: out.State, ExpiresAt: w2Time(out.ExpiresAt)})
}

// completeOAuth serves CompleteMcpOAuthConnect, the only unauthenticated /v1 route (CD-48). It always redirects to the
// console with the outcome; it never shows the authorization server's error text.
func (a *McpAPI) completeOAuth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	meta := middleware.MetaFrom(r.Context())
	ip := meta.IP
	if ip == "" {
		if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			ip = h
		}
	}
	ua := r.UserAgent()
	if len(ua) > 500 {
		ua = ua[:500]
	}
	res := a.Mcp.CompleteOAuth(r.Context(), mcpcatalog.CallbackInput{Code: q.Get("code"), State: q.Get("state"), Issuer: q.Get("iss"),
		Error: q.Get("error"), RequestID: RequestIDFrom(r.Context()), SourceIP: ip, UserAgent: ua})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, res.Redirect, http.StatusSeeOther)
}

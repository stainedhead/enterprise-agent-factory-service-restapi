package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/middleware"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
)

// callerThrough runs w2Caller behind the request-metadata middleware with the given trusted hops.
func callerThrough(t *testing.T, hops int, remote, xff string) acting.Caller {
	t.Helper()
	var got acting.Caller
	h := middleware.WithRequestMeta(hops)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		var err error
		got, err = w2Caller(r.WithContext(withPrincipal(r.Context(), authz.Principal{ID: "usr_1", Type: authz.PrincipalUser})))
		if err != nil {
			t.Errorf("w2Caller: %v", err)
		}
	}))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/teams", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	h.ServeHTTP(httptest.NewRecorder(), r)
	return got
}

// TestW2CallerSourceIPHonorsTrustedHops is FR-R02 AC1 to AC3: the audited address is the one the
// request metadata middleware computes, the same one the denied-request auditor records.
func TestW2CallerSourceIPHonorsTrustedHops(t *testing.T) {
	const xff = "203.0.113.7, 10.0.0.5"
	for _, tc := range []struct {
		name string
		hops int
		want string
	}{
		{"0 hops ignores the header", 0, "192.0.2.1"},
		{"1 hop", 1, "10.0.0.5"},
		{"2 hops", 2, "203.0.113.7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := callerThrough(t, tc.hops, "192.0.2.1:4433", xff)
			if c.SourceIP != tc.want {
				t.Fatalf("SourceIP = %q, want %q", c.SourceIP, tc.want)
			}
			// Same value the denied-request auditor reads.
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
			r.RemoteAddr = "192.0.2.1:4433"
			r.Header.Set("X-Forwarded-For", xff)
			if got := middleware.ClientIP(r, tc.hops); got != c.SourceIP {
				t.Fatalf("allowed record %q differs from denied record %q", c.SourceIP, got)
			}
		})
	}
}

// TestW2CallerWithoutMetadataFallsBackToThePeer: with no middleware in front, a client header is
// still never honored.
func TestW2CallerWithoutMetadataFallsBackToThePeer(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
	r.RemoteAddr = "192.0.2.1:4433"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	c, err := w2Caller(r.WithContext(withPrincipal(r.Context(), authz.Principal{ID: "usr_1"})))
	if err != nil || c.SourceIP != "192.0.2.1" {
		t.Fatalf("caller = %+v, %v", c, err)
	}
}

func withPrincipal(ctx context.Context, p authz.Principal) context.Context {
	return context.WithValue(ctx, keyPrincipal, &p)
}

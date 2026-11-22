package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/middleware"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
)

// CLI-CD-1: a workload principal may omit If-Match on UpdateWorkOrderRepo and nowhere else; every other caller
// needs it, and a header the workload does send is still validated.
func TestRequireIfMatchForWorkload(t *testing.T) {
	required := map[string]bool{"UpdateWorkOrderRepo": true, "UpdateTeam": true}
	var op string
	routes := func(*http.Request) (middleware.RouteInfo, bool) { return middleware.RouteInfo{OperationID: op}, true }
	var who authz.Principal
	principals := func(context.Context) (authz.Principal, bool) { return who, who.ID != "" }
	errs := func(w http.ResponseWriter, _ *http.Request, err error) {
		w.WriteHeader(http.StatusPreconditionRequired)
	}
	h := middleware.RequireIfMatchFor(required, routes, principals, errs)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	wl := authz.Principal{ID: "agt_1", Type: authz.PrincipalWorkload, Workload: &authz.Workload{TeamID: "team_a"}}
	admin := authz.Principal{ID: "usr_1"}

	for _, c := range []struct {
		name   string
		op     string
		p      authz.Principal
		header string
		want   int
	}{
		{"workload without the header", "UpdateWorkOrderRepo", wl, "", http.StatusNoContent},
		{"workload with a version", "UpdateWorkOrderRepo", wl, `"2"`, http.StatusNoContent},
		{"workload with a malformed header", "UpdateWorkOrderRepo", wl, "junk", http.StatusPreconditionRequired},
		{"admin without the header", "UpdateWorkOrderRepo", admin, "", http.StatusPreconditionRequired},
		{"workload on another operation", "UpdateTeam", wl, "", http.StatusPreconditionRequired},
	} {
		op, who = c.op, c.p
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPatch, "/", nil)
		if c.header != "" {
			r.Header.Set("If-Match", c.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, rec.Code, c.want)
		}
	}
	if !middleware.IfMatchOptionalForWorkload("UpdateWorkOrderRepo") || middleware.IfMatchOptionalForWorkload("UpdateTeam") {
		t.Error("IfMatchOptionalForWorkload")
	}
}

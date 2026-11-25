package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPendingMcpOpsAreSection414(t *testing.T) {
	sec := map[string]bool{}
	for _, rt := range Routes {
		sec[rt.OperationID] = true
	}
	if len(PendingMcpOps) != 2 {
		t.Errorf("pending = %d", len(PendingMcpOps))
	}
	for _, op := range PendingMcpOps {
		if !sec[op] {
			t.Errorf("%s is not a contract operation", op)
		}
	}
	h, _ := newServer(t, BindMcpPending())
	rec := do(t, h, http.MethodGet, "/v1/mcp-oauth/callback", "svc", "")
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "not_implemented") {
		t.Errorf("pending answer = %d %s", rec.Code, rec.Body.String())
	}
}

func TestMcpMemberIsRefusedUntilBuilt(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/teams/team_1/agents", nil)
	rec := httptest.NewRecorder()
	if refuseMcpUntilBuilt(rec, r, nil) {
		t.Fatal("a body without mcp was refused")
	}
	access := "all"
	if !refuseMcpUntilBuilt(rec, r, &mcpRequest{Access: &access}) {
		t.Fatal("mcp was not refused")
	}
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "not_implemented") {
		t.Errorf("answer = %d %s", rec.Code, rec.Body.String())
	}
}

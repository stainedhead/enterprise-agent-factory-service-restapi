package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPendingMcpOpsAreEmpty(t *testing.T) {
	if len(PendingMcpOps) != 0 {
		t.Errorf("pending = %v", PendingMcpOps)
	}
	if err := BindMcpPending()(&Registry{}); err != nil {
		t.Errorf("binding no operation: %v", err)
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

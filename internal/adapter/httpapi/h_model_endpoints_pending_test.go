package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestPendingModelEndpointOpsAreSection413(t *testing.T) {
	sec := map[string]bool{}
	for _, rt := range Routes {
		sec[rt.OperationID] = true
	}
	if len(PendingModelEndpointOps) != 27 {
		t.Errorf("pending = %d", len(PendingModelEndpointOps))
	}
	for _, op := range PendingModelEndpointOps {
		if !sec[op] {
			t.Errorf("%s is not a contract operation", op)
		}
	}
	h, _ := newServer(t, BindModelEndpointsPending())
	rec := do(t, h, http.MethodGet, "/v1/admin/inference-providers", "svc", "")
	if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "not_implemented") {
		t.Errorf("pending answer = %d %s", rec.Code, rec.Body.String())
	}
}

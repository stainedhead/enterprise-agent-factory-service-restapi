package httpapi

import (
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

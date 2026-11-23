package teams

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
)

func TestErrDeniedAndNonNil(t *testing.T) {
	if got := (errDenied{perm: authz.TeamsWrite}).Error(); got == "" {
		t.Error("errDenied.Error is empty")
	}
	if got := nonNil(nil); got == nil || len(got) != 0 {
		t.Errorf("nonNil(nil) = %#v", got)
	}
	if got := nonNil([]string{"a"}); len(got) != 1 {
		t.Errorf("nonNil = %v", got)
	}
}

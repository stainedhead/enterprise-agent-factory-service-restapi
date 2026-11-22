//go:build e2e

package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// The harness's own behavior, runnable anywhere: it skips without a cluster, names are unique, and
// WaitFor returns as soon as the condition holds.
func TestHarnessSkipsWithoutKubeconfig(t *testing.T) {
	t.Setenv(EnvKubeconfig, "")
	res := t.Run("inner", func(t *testing.T) {
		NewHarness(t)
		t.Fatal("NewHarness did not skip")
	})
	if !res {
		t.Fatal("the inner test failed instead of skipping")
	}
}

func TestHarnessUniqueNameAndWaitFor(t *testing.T) {
	kc := t.TempDir() + "/kubeconfig"
	if err := writeFile(kc); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvKubeconfig, kc)
	h := NewHarness(t)
	a, b := h.UniqueName("agent"), h.UniqueName("agent")
	if a == b || !strings.HasPrefix(a, "agent-") || len(a) != len("agent-")+8 {
		t.Fatalf("names %q %q", a, b)
	}
	n := 0
	h.WaitFor(time.Second, time.Millisecond, "three tries", func(_ context.Context) (bool, error) { n++; return n == 3, nil })
	if n != 3 {
		t.Fatalf("polled %d times", n)
	}
}

func writeFile(p string) error {
	return os.WriteFile(p, []byte("apiVersion: v1\nkind: Config\n"), 0o600)
}

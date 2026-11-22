//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Environment variables of the harness. Every e2e test starts with NewHarness, which skips the test
// when AFS_E2E_KUBECONFIG is unset, so `make e2e` is safe anywhere.
const (
	EnvKubeconfig = "AFS_E2E_KUBECONFIG" // required: kubeconfig of the dev cluster
	EnvAPIURL     = "AFS_E2E_API_URL"    // optional: base URL of afs-api on the dev cluster
	EnvAPIKey     = "AFS_E2E_API_KEY"    // optional: service-admin key for the suite (never logged)
	EnvRegion     = "AFS_E2E_REGION"     // optional: AWS region for AWS-side assertions
)

// Harness is the shared setup of the e2e suites: kubectl against the dev cluster, unique names, and
// cleanup. It shells out to kubectl rather than importing client-go, like the M0 probes.
type Harness struct {
	T          *testing.T
	Kubeconfig string
	APIURL     string
	Region     string
	apiKey     string
}

// NewHarness returns the harness, or skips t when no cluster is configured.
func NewHarness(t *testing.T) *Harness {
	t.Helper()
	kc := os.Getenv(EnvKubeconfig)
	if kc == "" {
		t.Skipf("%s not set: e2e suites need the dev cluster", EnvKubeconfig)
	}
	if _, err := os.Stat(kc); err != nil {
		t.Fatalf("%s: %v", EnvKubeconfig, err)
	}
	return &Harness{T: t, Kubeconfig: kc, APIURL: os.Getenv(EnvAPIURL), Region: os.Getenv(EnvRegion), apiKey: os.Getenv(EnvAPIKey)}
}

// APIKey returns the suite's key and skips the test when none is configured. Tests pass it to an
// Authorization header and never print it.
func (h *Harness) APIKey() string {
	h.T.Helper()
	if h.apiKey == "" || h.APIURL == "" {
		h.T.Skipf("%s and %s not set", EnvAPIURL, EnvAPIKey)
	}
	return h.apiKey
}

// Kubectl runs kubectl against the harness cluster and returns its combined output; a failure
// fails the test.
func (h *Harness) Kubectl(ctx context.Context, args ...string) string {
	h.T.Helper()
	out, err := h.KubectlErr(ctx, args...)
	if err != nil {
		h.T.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// KubectlErr is Kubectl returning the error instead of failing.
func (h *Harness) KubectlErr(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", h.Kubeconfig}, args...)...) //nolint:gosec // operator-run harness
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// UniqueName returns prefix plus eight random hex characters, a valid DNS label for prefix up to 20
// characters, so parallel runs and reruns never collide.
func (h *Harness) UniqueName(prefix string) string {
	h.T.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		h.T.Fatal(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

// Fixture is a throwaway team namespace the reconciler is not involved in: tests that exercise only
// Kubernetes behavior (admission, network policy) use it.
type Fixture struct {
	Namespace string
}

// NewNamespace creates a namespace named afs-t-<unique> labelled as AFS-managed and deletes it when
// the test ends.
func (h *Harness) NewNamespace(ctx context.Context) Fixture {
	h.T.Helper()
	ns := "afs-t-" + h.UniqueName("e2e")
	h.Kubectl(ctx, "create", "namespace", ns)
	h.Kubectl(ctx, "label", "namespace", ns, "app.kubernetes.io/managed-by=afs-e2e", "--overwrite")
	h.T.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = h.KubectlErr(cctx, "delete", "namespace", ns, "--wait=false", "--ignore-not-found")
	})
	return Fixture{Namespace: ns}
}

// WaitFor polls cond every interval until it returns true or timeout passes, then fails the test
// with msg. It is how e2e tests wait for the reconciler (RC-US-1.1: within 60 s).
func (h *Harness) WaitFor(timeout, interval time.Duration, msg string, cond func(ctx context.Context) (bool, error)) {
	h.T.Helper()
	ctx, cancel := context.WithTimeout(h.T.Context(), timeout)
	defer cancel()
	var last error
	for {
		ok, err := cond(ctx)
		if ok {
			return
		}
		last = err
		select {
		case <-ctx.Done():
			if last != nil {
				h.T.Fatalf("timed out after %v waiting for %s: last error: %v", timeout, msg, last)
			}
			h.T.Fatalf("timed out after %v waiting for %s", timeout, msg)
		case <-time.After(interval):
		}
	}
}

// Describe is a one-line summary for logs.
func (h *Harness) Describe() string {
	return fmt.Sprintf("kubeconfig=%s api=%s region=%s", h.Kubeconfig, h.APIURL, h.Region)
}

//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestViewerKubectl (M5-E2, RC-13, RC-US-11.1) runs against the dev cluster. A person with the dev account
// sets up a team with a federated viewer role (AFS_VIEWER_KUBECTL_ROLES on the reconciler) and runs:
//
//	AFS_E2E_TEAM_ID=team_... AFS_E2E_TEAM_NAMESPACE=afs-t-<slug> \
//	AFS_E2E_VIEWER_KUBECONFIG=/path/to/kubeconfig-from-GetTeamKubeconfig make e2e
//
// with the viewer's AWS credentials in the environment of the viewer kubeconfig's exec plugin (the role the
// identity provider federates to) and the suite's API key and URL set (AFS_E2E_API_URL, AFS_E2E_API_KEY, a
// service admin's key). The test turns viewerKubectl on, checks the viewer can read the namespace and is
// refused secrets, exec and writes, turns the flag off and checks the access is gone within 60 s.
func TestViewerKubectl(t *testing.T) {
	h := NewHarness(t)
	key := h.APIKey()
	env := need(t, "AFS_E2E_TEAM_ID", "AFS_E2E_TEAM_NAMESPACE", "AFS_E2E_VIEWER_KUBECONFIG")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	ns, kc := env["AFS_E2E_TEAM_NAMESPACE"], env["AFS_E2E_VIEWER_KUBECONFIG"]

	setFlag := func(on bool) {
		t.Helper()
		url := strings.TrimRight(h.APIURL, "/") + "/v1/teams/" + env["AFS_E2E_TEAM_ID"]
		get, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		get.Header.Set("Authorization", "Bearer "+key)
		res, err := http.DefaultClient.Do(get)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		body, _ := json.Marshal(map[string]any{"viewerKubectl": on})
		patch, _ := http.NewRequestWithContext(ctx, http.MethodPatch, url, strings.NewReader(string(body)))
		patch.Header.Set("Authorization", "Bearer "+key)
		patch.Header.Set("Content-Type", "application/json")
		patch.Header.Set("If-Match", res.Header.Get("ETag"))
		res, err = http.DefaultClient.Do(patch)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("PATCH viewerKubectl=%v: %d", on, res.StatusCode)
		}
	}
	viewer := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "kubectl", append([]string{"--kubeconfig", kc, "-n", ns}, args...)...).CombinedOutput() //nolint:gosec // operator-run harness
		return string(out), err
	}

	setFlag(true)
	t.Cleanup(func() { setFlag(false) })
	h.WaitFor(60*time.Second, 2*time.Second, "the viewer to read pods", func(context.Context) (bool, error) {
		_, err := viewer("get", "pods")
		return err == nil, err
	})
	for _, args := range [][]string{{"get", "pods"}, {"get", "events"}, {"get", "deployments"}} {
		if out, err := viewer(args...); err != nil {
			t.Errorf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	for _, args := range [][]string{{"get", "secrets"}, {"auth", "can-i", "create", "pods/exec"}, {"auth", "can-i", "create", "pods/portforward"},
		{"delete", "deployments", "--all"}, {"create", "configmap", "x", "--from-literal=a=b"}} {
		out, err := viewer(args...)
		refused := err != nil || strings.TrimSpace(out) == "no"
		if !refused {
			t.Errorf("kubectl %s was allowed:\n%s", strings.Join(args, " "), out)
		}
	}

	setFlag(false)
	h.WaitFor(60*time.Second, 2*time.Second, "the viewer to lose access", func(context.Context) (bool, error) {
		_, err := viewer("get", "pods")
		return err != nil, nil
	})
}

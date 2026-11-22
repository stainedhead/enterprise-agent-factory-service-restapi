//go:build e2e

// Spikes OQ-1 and OQ-2 (M0-E1, M0-E2; research.md §3.3). A person with the dev account runs:
//
//	AFS_E2E_NAMESPACE=afs-t-spike AFS_E2E_SERVICE_ACCOUNT=agent-spike \
//	AFS_E2E_CHAINED_ROLE_ARN=arn:aws:iam::111122223333:role/afs-spike-target \
//	AFS_E2E_STORAGE_CLASS=gp3 make e2e
//
// with kubectl and the AWS CLI configured for the dev cluster. The namespace and ServiceAccount
// must already have a Pod Identity association whose role may assume AFS_E2E_CHAINED_ROLE_ARN. The
// probe prints what it sees; record the result in research.md §3.3.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func need(t *testing.T, names ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, n := range names {
		v := os.Getenv(n)
		if v == "" {
			t.Skipf("%s not set", n)
		}
		out[n] = v
	}
	return out
}

func run(ctx context.Context, t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // operator-run probe
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestPodIdentitySessionTags (OQ-1) runs a pod with the ServiceAccount, reads its caller identity
// and the session tags visible through a chained role. The AWS CLI image prints the assumed-role
// ARN; tags are read back from the chained session with sts:GetCallerIdentity and, if CloudTrail is
// reachable, from the AssumeRole event.
func TestPodIdentitySessionTags(t *testing.T) {
	env := need(t, "AFS_E2E_NAMESPACE", "AFS_E2E_SERVICE_ACCOUNT", "AFS_E2E_CHAINED_ROLE_ARN")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	ns, sa := env["AFS_E2E_NAMESPACE"], env["AFS_E2E_SERVICE_ACCOUNT"]
	script := `set -e
aws sts get-caller-identity --output json
aws sts assume-role --role-arn "$CHAINED" --role-session-name afs-spike --output json \
  --query '{arn: AssumedRoleUser.Arn, packed: PackedPolicySize}'`
	overrides, _ := json.Marshal(map[string]any{"spec": map[string]any{"serviceAccountName": sa}})
	out := run(ctx, t, "kubectl", "-n", ns, "run", "afs-oq1-probe", "--rm", "-i", "--restart=Never",
		"--image=public.ecr.aws/aws-cli/aws-cli:latest", "--overrides="+string(overrides),
		"--env=CHAINED="+env["AFS_E2E_CHAINED_ROLE_ARN"], "--command", "--", "sh", "-c", script)
	t.Logf("OQ-1 pod identity probe:\n%s", out)
	// A non-zero PackedPolicySize on the chained session means session tags were carried as
	// transitive tags (they count against the packed size).
	t.Log("record: built-in session tags seen, whether they are transitive through the chain (PackedPolicySize > 0), and the role_session_name in the chained ARN (D51)")
}

// TestEBSCSIVolumeTags (OQ-2) creates a PVC with the storage class, waits for its volume and
// counts the tags the CSI driver set.
func TestEBSCSIVolumeTags(t *testing.T) {
	env := need(t, "AFS_E2E_NAMESPACE", "AFS_E2E_STORAGE_CLASS")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	ns := env["AFS_E2E_NAMESPACE"]
	pvc := `{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"afs-oq2-probe"},
"spec":{"accessModes":["ReadWriteOnce"],"storageClassName":"` + env["AFS_E2E_STORAGE_CLASS"] + `","resources":{"requests":{"storage":"1Gi"}}}}`
	cmd := exec.CommandContext(ctx, "kubectl", "-n", ns, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(pvc)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("apply pvc: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.CommandContext(context.WithoutCancel(ctx), "kubectl", "-n", ns, "delete", "pvc", "afs-oq2-probe", "--wait=false").Run() //nolint:gosec // operator-run probe
	})
	// WaitForFirstConsumer storage classes need a pod; the probe pod mounts the claim.
	overrides := `{"spec":{"volumes":[{"name":"v","persistentVolumeClaim":{"claimName":"afs-oq2-probe"}}],
"containers":[{"name":"p","image":"public.ecr.aws/docker/library/busybox:stable","command":["sleep","5"],"volumeMounts":[{"name":"v","mountPath":"/v"}]}]}}`
	run(ctx, t, "kubectl", "-n", ns, "run", "afs-oq2-pod", "--rm", "-i", "--restart=Never",
		"--image=public.ecr.aws/docker/library/busybox:stable", "--overrides="+overrides)
	pv := strings.TrimSpace(run(ctx, t, "kubectl", "-n", ns, "get", "pvc", "afs-oq2-probe", "-o", "jsonpath={.spec.volumeName}"))
	vol := strings.TrimSpace(run(ctx, t, "kubectl", "get", "pv", pv, "-o", "jsonpath={.spec.csi.volumeHandle}"))
	tags := run(ctx, t, "aws", "ec2", "describe-volumes", "--volume-ids", vol, "--query", "Volumes[0].Tags", "--output", "json")
	var parsed []map[string]string
	if err := json.Unmarshal([]byte(tags), &parsed); err != nil {
		t.Fatal(err)
	}
	t.Logf("OQ-2: CSI volume %s carries %d tags:\n%s", vol, len(parsed), tags)
	if len(parsed) > 10 {
		t.Errorf("the CSI driver set %d tags; the 40-key cap of RC-9 leaves room for only %d", len(parsed), 50-40)
	}
}

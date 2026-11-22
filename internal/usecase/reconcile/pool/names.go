package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// Names of the objects of a pool in its team namespace. The ServiceAccount is "pool-<name>", the name the
// database's resolve_workload maps back to the pool (database contract §5).
const workloadPrefix = "pool-"

// WorkloadName is the name of the pool's Deployment and ServiceAccount.
func WorkloadName(poolName string) string { return workloadPrefix + poolName }

// ServiceAccountName is the pool's ServiceAccount.
func ServiceAccountName(poolName string) string { return WorkloadName(poolName) }

// ConfigMapName holds the pool's configuration values (envFrom and /etc/afs/config).
func ConfigMapName(poolName string) string { return WorkloadName(poolName) + "-config" }

// AWSConfigMapName holds /etc/afs/aws/config and /etc/afs/tags.json.
func AWSConfigMapName(poolName string) string { return WorkloadName(poolName) + "-aws" }

// EnvSecretName is the Secret the env ExternalSecret writes.
func EnvSecretName(poolName string) string { return WorkloadName(poolName) + "-env" }

// FilesSecretName is the Secret the file ExternalSecret writes.
func FilesSecretName(poolName string) string { return WorkloadName(poolName) + "-files" }

// SharedClaimName is the claim of the pool's shared EFS volume.
func SharedClaimName(poolName string) string { return WorkloadName(poolName) + "-shared" }

// EgressPolicyName is the pool's egress policy.
func EgressPolicyName(poolID string) string {
	return "afs-egress-" + strings.ToLower(strings.ReplaceAll(poolID, "_", "-"))
}

// RoleName is the pool's own IAM role: "afs-<pool ulid>" (D25).
func RoleName(poolID string) (string, error) { return team.RoleNameForID(domain.ID(poolID)) }

// QueueName is the pool's inbox queue: "afs-<team ulid>-<short hash>", the hash being the first eight hex
// digits of the SHA-256 of the pool id. It is deterministic, so a sender computes the receiver's queue
// without asking AWS, and at most 40 characters of the 80 an SQS name may have.
func QueueName(teamID, poolID string) (string, error) {
	_, ulid, ok := strings.Cut(teamID, "_")
	if !ok || ulid == "" {
		return "", fmt.Errorf("pool: %q is not a prefixed id", teamID)
	}
	if poolID == "" {
		return "", fmt.Errorf("pool: queue name needs the pool id")
	}
	sum := sha256.Sum256([]byte(poolID))
	return "afs-" + ulid + "-" + hex.EncodeToString(sum[:])[:8], nil
}

// HandoffEnvName is the variable that carries a receiving pool's queue URL to a pool that hands work to it:
// AFS_HANDOFF_<TO_POOL_NAME_UPPER_SNAKE>_QUEUE_URL (API contract §4.4a).
func HandoffEnvName(toPool string) string {
	return "AFS_HANDOFF_" + strings.ToUpper(strings.ReplaceAll(toPool, "-", "_")) + "_QUEUE_URL"
}

// InboxEnvName is the variable that carries the pool's own inbox queue URL.
const InboxEnvName = "AFS_INBOX_QUEUE_URL"

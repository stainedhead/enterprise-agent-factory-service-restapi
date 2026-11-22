package agent

import (
	"fmt"
	"strconv"
	"strings"
)

func itoa(n int) string { return strconv.Itoa(n) }

// Names of the objects of an agent in its team namespace. The ServiceAccount is "agent-<name>", the name
// the database's resolve_workload maps back to the agent (database contract §5).
const (
	workloadPrefix = "agent-"
	// VolumeClaimName is the volumeClaimTemplate name; the claim of replica 0 is
	// "<VolumeClaimName>-agent-<name>-0".
	VolumeClaimName = "data"
)

// WorkloadName is the name of the Agentic Teammates resource, its StatefulSet and its ServiceAccount.
func WorkloadName(agentName string) string { return workloadPrefix + agentName }

// ServiceAccountName is the agent's ServiceAccount.
func ServiceAccountName(agentName string) string { return WorkloadName(agentName) }

// PVCName is the claim of the agent's only replica.
func PVCName(agentName string) string { return VolumeClaimName + "-" + WorkloadName(agentName) + "-0" }

// ConfigMapName holds the agent's configuration values (envFrom and /etc/afs/config).
func ConfigMapName(agentName string) string { return WorkloadName(agentName) + "-config" }

// AWSConfigMapName holds /etc/afs/aws/config and /etc/afs/tags.json.
func AWSConfigMapName(agentName string) string { return WorkloadName(agentName) + "-aws" }

// EnvSecretName is the Secret the env ExternalSecret writes.
func EnvSecretName(agentName string) string { return WorkloadName(agentName) + "-env" }

// FilesSecretName is the Secret the file ExternalSecret writes.
func FilesSecretName(agentName string) string { return WorkloadName(agentName) + "-files" }

// RoleName is the IAM role name of a dedicated agent identity: "afs-<agent ulid>" (D25).
func RoleName(agentID string) (string, error) {
	_, ulid, ok := strings.Cut(agentID, "_")
	if !ok || ulid == "" {
		return "", fmt.Errorf("agent: %q is not a prefixed id", agentID)
	}
	return "afs-" + ulid, nil
}

// SnapshotClassName is the VolumeSnapshotClass of an agent, which carries its tags.
func SnapshotClassName(agentID string) string {
	_, ulid, _ := strings.Cut(agentID, "_")
	return "afs-" + strings.ToLower(ulid)
}

// Tag keys of an agent's AWS resources.
const (
	TagAgentID = "afs-agent-id"
)

package render

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// Snapshot objects (RC-16).
const (
	snapshotAPI = "snapshot.storage.k8s.io/v1"
	// EBSCSIDriver is the CSI driver of agent volumes.
	EBSCSIDriver = "ebs.csi.aws.com"
	// SnapshotGroup is the API group of snapshot objects.
	SnapshotGroup = "snapshot.storage.k8s.io"
)

// SnapshotObjectName is the VolumeSnapshot name of a snapshot row ("vsnap_01J…" → "afs-vsnap-01j…").
func SnapshotObjectName(snapshotID string) string {
	return "afs-" + strings.ReplaceAll(strings.ToLower(snapshotID), "_", "-")
}

// RestoreSnapshotName is the VolumeSnapshot a missing volume is restored from.
func RestoreSnapshotName(agentID string) string {
	return "afs-restore-" + strings.TrimPrefix(agent.SnapshotClassName(agentID), "afs-")
}

// SnapshotClass renders the agent's VolumeSnapshotClass. The EBS CSI driver tags every snapshot it creates
// with the class's tagSpecification parameters, which is how the agent's effective tags reach its EBS
// snapshots (TAG-4). The deletion policy is Retain: a snapshot survives the namespace (the final snapshot
// of snapshot_then_delete, and a restore after a cluster rebuild); pruning flips the content's policy to
// Delete first (volumesnapshot.PrunePatch).
func SnapshotClass(d agent.Desired) *unstructured.Unstructured {
	u := New(snapshotAPI, "VolumeSnapshotClass", "", agent.SnapshotClassName(d.ID))
	u.Object["driver"] = EBSCSIDriver
	u.Object["deletionPolicy"] = "Retain"
	params := map[string]any{}
	keys := make([]string, 0, len(d.Tags))
	for k := range d.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		params[fmt.Sprintf("tagSpecification_%d", i+1)] = k + "=" + d.Tags[k]
	}
	if len(params) > 0 {
		u.Object["parameters"] = params
	}
	return Managed(u, OwnerOfAgent(d))
}

// VolumeSnapshot renders the request for a snapshot of the agent's claim.
func VolumeSnapshot(d agent.Desired, name string) *unstructured.Unstructured {
	u := New(snapshotAPI, "VolumeSnapshot", d.Team.Namespace, name)
	u.Object["spec"] = map[string]any{
		"volumeSnapshotClassName": agent.SnapshotClassName(d.ID),
		"source":                  map[string]any{"persistentVolumeClaimName": agent.PVCName(d.Name)},
	}
	return Managed(u, OwnerOfAgent(d))
}

// ImportedContent renders the VolumeSnapshotContent that adopts an existing EBS snapshot into a rebuilt
// cluster, bound to the VolumeSnapshot name in the agent's namespace.
func ImportedContent(d agent.Desired, name, ebsSnapshotID string) *unstructured.Unstructured {
	u := New(snapshotAPI, "VolumeSnapshotContent", "", name)
	u.Object["spec"] = map[string]any{
		"deletionPolicy":          "Retain",
		"driver":                  EBSCSIDriver,
		"volumeSnapshotClassName": agent.SnapshotClassName(d.ID),
		"source":                  map[string]any{"snapshotHandle": ebsSnapshotID},
		"volumeSnapshotRef":       map[string]any{"name": name, "namespace": d.Team.Namespace},
	}
	return Managed(u, OwnerOfAgent(d))
}

// ImportedSnapshot renders the VolumeSnapshot bound to ImportedContent.
func ImportedSnapshot(d agent.Desired, name string) *unstructured.Unstructured {
	u := New(snapshotAPI, "VolumeSnapshot", d.Team.Namespace, name)
	u.Object["spec"] = map[string]any{
		"volumeSnapshotClassName": agent.SnapshotClassName(d.ID),
		"source":                  map[string]any{"volumeSnapshotContentName": name},
	}
	return Managed(u, OwnerOfAgent(d))
}

package render

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// StorageQuantity is the claim size for a volume of gib GiB.
func StorageQuantity(gib int) string { return fmt.Sprintf("%dGi", gib) }

// RestoredPVC renders the claim of an agent whose volume is missing (a rebuilt cluster), populated from the
// VolumeSnapshot snapshotName, under the name the StatefulSet expects for replica 0, which the StatefulSet
// then adopts (RC-16, RC-US-3.2). The claim carries the managed labels the apply requires.
func RestoredPVC(d agent.Desired, snapshotName string) *unstructured.Unstructured {
	u := New("v1", "PersistentVolumeClaim", d.Team.Namespace, agent.PVCName(d.Name))
	spec := map[string]any{
		"accessModes": []any{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]any{"storage": StorageQuantity(d.Volume.SizeGiB)}},
		"dataSource":  map[string]any{"apiGroup": "snapshot.storage.k8s.io", "kind": "VolumeSnapshot", "name": snapshotName},
	}
	if d.Volume.StorageClass != "" {
		spec["storageClassName"] = d.Volume.StorageClass
	}
	u.Object["spec"] = spec
	u.SetLabels(map[string]string{"app.kubernetes.io/name": agent.WorkloadName(d.Name), LabelAgentID: d.ID})
	return Managed(u, OwnerOfAgent(d))
}

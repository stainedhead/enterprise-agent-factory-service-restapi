package k8s

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/snapshot"
)

// SnapshotCluster implements snapshot.Cluster on the EBS CSI snapshot API (RC-16).
type SnapshotCluster struct{ client *Client }

// NewSnapshotCluster returns a SnapshotCluster.
func NewSnapshotCluster(c *Client) *SnapshotCluster { return &SnapshotCluster{client: c} }

var _ snapshot.Cluster = (*SnapshotCluster)(nil)

// Prepare implements snapshot.Cluster: the agent's VolumeSnapshotClass carries its tags.
func (s *SnapshotCluster) Prepare(ctx context.Context, d agent.Desired) error {
	if _, err := s.client.Apply(ctx, render.SnapshotClass(d)); err != nil {
		return fmt.Errorf("VolumeSnapshotClass: %w", err)
	}
	return nil
}

// Request implements snapshot.Cluster.
func (s *SnapshotCluster) Request(ctx context.Context, d agent.Desired, name string) error {
	if _, err := s.client.Apply(ctx, render.VolumeSnapshot(d, name)); err != nil {
		return fmt.Errorf("VolumeSnapshot %s: %w", name, err)
	}
	return nil
}

// Status implements snapshot.Cluster.
func (s *SnapshotCluster) Status(ctx context.Context, d agent.Desired, name string) (snapshot.Status, error) {
	vs, err := s.client.Get(ctx, gvkSnapshot, d.Team.Namespace, name)
	if err != nil || vs == nil {
		return snapshot.Status{}, err
	}
	st := snapshot.Status{Exists: true}
	st.Ready, _, _ = unstructured.NestedBool(vs.Object, "status", "readyToUse")
	st.SizeGiB = quantityGiB(vs.Object, "status", "restoreSize")
	st.Error, _, _ = unstructured.NestedString(vs.Object, "status", "error", "message")
	if content, _, _ := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName"); content != "" {
		vc, err := s.client.Get(ctx, gvkSnapContent, "", content)
		if err != nil {
			return st, err
		}
		if vc != nil {
			st.EBSID, _, _ = unstructured.NestedString(vc.Object, "status", "snapshotHandle")
		}
	}
	return st, nil
}

// Delete implements snapshot.Cluster: the content's deletion policy becomes Delete, then the VolumeSnapshot
// goes, and the snapshot controller removes the content and the EBS snapshot.
func (s *SnapshotCluster) Delete(ctx context.Context, d agent.Desired, name string) error {
	vs, err := s.client.Get(ctx, gvkSnapshot, d.Team.Namespace, name)
	if err != nil || vs == nil {
		return err
	}
	if content, _, _ := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName"); content != "" {
		ri, err := s.client.iface(gvkSnapContent, "")
		if err != nil {
			return err
		}
		patch := []byte(`{"spec":{"deletionPolicy":"Delete"}}`)
		if _, err := ri.Patch(ctx, content, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: FieldManager}); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("flip VolumeSnapshotContent %s: %w", content, wrap(err))
			}
		}
	}
	if err := s.client.Delete(ctx, gvkSnapshot, d.Team.Namespace, name); err != nil {
		return fmt.Errorf("delete VolumeSnapshot %s: %w", name, err)
	}
	return nil
}

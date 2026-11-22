package k8s

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/lifecycle"
)

var (
	gvkPod = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
	gvkPV  = schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolume"}
)

// AgentLifecycle implements the Kubernetes ports of the agent delete steps (lifecycle.AgentWorkloads and
// lifecycle.AgentVolumes) on top of AgentCluster.
type AgentLifecycle struct{ *AgentCluster }

// NewAgentLifecycle returns the lifecycle ports of cluster.
func NewAgentLifecycle(c *AgentCluster) *AgentLifecycle { return &AgentLifecycle{c} }

var (
	_ lifecycle.AgentWorkloads = (*AgentLifecycle)(nil)
	_ lifecycle.AgentVolumes   = (*AgentLifecycle)(nil)
)

// Stop implements lifecycle.AgentWorkloads: the Teammate and what it referenced are removed; the claim is
// not touched.
func (l *AgentLifecycle) Stop(ctx context.Context, d agent.Desired) error {
	return l.DeleteAgent(ctx, d)
}

// Stopped implements lifecycle.AgentWorkloads: the Teammate and every pod labeled with the agent are gone.
func (l *AgentLifecycle) Stopped(ctx context.Context, d agent.Desired) (bool, error) {
	gone, err := l.TeammateGone(ctx, d)
	if err != nil || !gone {
		return false, err
	}
	pods, err := l.client.List(ctx, gvkPod, d.Team.Namespace, render.LabelAgentID+"="+d.ID)
	if err != nil {
		return false, err
	}
	return len(pods) == 0, nil
}

// DeleteVolume implements lifecycle.AgentVolumes.
func (l *AgentLifecycle) DeleteVolume(ctx context.Context, d agent.Desired) error {
	if err := l.client.Delete(ctx, gvkPVC, d.Team.Namespace, agent.PVCName(d.Name)); err != nil {
		return fmt.Errorf("delete claim %s: %w", agent.PVCName(d.Name), err)
	}
	return nil
}

// RetainVolume implements lifecycle.AgentVolumes: the PersistentVolume behind the claim gets reclaim policy
// Retain, so it outlives the claim and the namespace. A missing or unbound claim returns the zero value.
func (l *AgentLifecycle) RetainVolume(ctx context.Context, d agent.Desired) (lifecycle.RetainedVolume, error) {
	pvc, err := l.client.Get(ctx, gvkPVC, d.Team.Namespace, agent.PVCName(d.Name))
	if err != nil || pvc == nil {
		return lifecycle.RetainedVolume{}, err
	}
	pv, _, _ := unstructured.NestedString(pvc.Object, "spec", "volumeName")
	if pv == "" {
		return lifecycle.RetainedVolume{}, nil
	}
	ri, err := l.client.iface(gvkPV, "")
	if err != nil {
		return lifecycle.RetainedVolume{}, err
	}
	patch := []byte(`{"spec":{"persistentVolumeReclaimPolicy":"Retain"}}`)
	if _, err := ri.Patch(ctx, pv, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: FieldManager}); err != nil {
		if apierrors.IsNotFound(err) {
			return lifecycle.RetainedVolume{}, nil
		}
		return lifecycle.RetainedVolume{}, fmt.Errorf("retain volume %s: %w", pv, wrap(err))
	}
	return lifecycle.RetainedVolume{Claim: agent.PVCName(d.Name), Volume: pv}, nil
}

// DeleteSnapshotClass implements lifecycle.AgentVolumes.
func (l *AgentLifecycle) DeleteSnapshotClass(ctx context.Context, d agent.Desired) error {
	return l.client.Delete(ctx, gvkSnapClass, "", agent.SnapshotClassName(d.ID))
}

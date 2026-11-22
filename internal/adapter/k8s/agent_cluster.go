package k8s

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/crd/agentictm"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

var (
	gvkExternalSecret = schema.GroupVersionKind{Group: "external-secrets.io", Version: "v1", Kind: "ExternalSecret"}
	gvkServiceAcct    = schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}
	gvkPVC            = schema.GroupVersionKind{Version: "v1", Kind: "PersistentVolumeClaim"}
	gvkNodePool       = schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodePool"}
	gvkNodeClass      = schema.GroupVersionKind{Group: "karpenter.k8s.aws", Version: "v1", Kind: "EC2NodeClass"}
	gvkSnapshot       = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshot"}
	gvkSnapContent    = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"}
	gvkSnapClass      = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotClass"}
)

// AgentOwnedKinds are the kinds the agent controller creates, for the drift watch (appended to OwnedKinds).
// NodePool and EC2NodeClass are not watched: they belong to the team, not to one agent, and an edit to them
// is repaired by the next agent reconcile.
func AgentOwnedKinds() []schema.GroupVersionKind {
	return []schema.GroupVersionKind{
		gvkServiceAcct,
		gvkExternalSecret,
		agentictm.GVK(),
	}
}

// AgentCluster implements agent.Cluster and agent.Volumes on the Kubernetes API.
type AgentCluster struct {
	client   *Client
	nodePool render.NodePoolConfig
}

// NewAgentCluster returns an AgentCluster. np is the platform's node class base for dedicated teams.
func NewAgentCluster(c *Client, np render.NodePoolConfig) *AgentCluster {
	return &AgentCluster{client: c, nodePool: np}
}

var (
	_ agent.Cluster = (*AgentCluster)(nil)
	_ agent.Volumes = (*AgentCluster)(nil)
)

// ApplyAgent implements agent.Cluster. The objects the pod references (ServiceAccount, ConfigMaps,
// ExternalSecrets, the team's nodes) go first and the Teammate last, so a pod never starts with a reference
// that does not exist yet. Objects are applied independently and the first error is returned; objects the
// agent no longer has (an ExternalSecret without bindings) are removed.
func (c *AgentCluster) ApplyAgent(ctx context.Context, env agent.Environment) (agent.ApplyResult, error) {
	d := env.Agent
	var res agent.ApplyResult
	var firstErr error
	apply := func(o *unstructured.Unstructured) {
		r, err := c.client.Apply(ctx, o)
		if r.Modified() {
			res.Changed = append(res.Changed, r.Name)
		}
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s %s: %w", o.GetKind(), o.GetName(), err)
		}
	}

	if err := c.reconcileNodePool(ctx, env, &res); err != nil && firstErr == nil {
		firstErr = err
	}
	apply(render.ServiceAccount(d))
	apply(render.AgentConfigMap(d))
	apply(render.AgentAWSConfigMap(d))
	rendered := map[string]bool{}
	for _, es := range render.ExternalSecrets(d) {
		rendered[es.GetName()] = true
		apply(es)
	}
	if err := c.pruneExternalSecrets(ctx, d, rendered); err != nil && firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		return res, firstErr
	}
	tm, err := render.Teammate(env)
	if err != nil {
		return res, err
	}
	apply(tm)
	return res, firstErr
}

// reconcileNodePool applies the team's NodePool and EC2NodeClass for the dedicated tier, and removes them
// for the shared tier.
func (c *AgentCluster) reconcileNodePool(ctx context.Context, env agent.Environment, res *agent.ApplyResult) error {
	d := env.Agent
	name := render.NodePoolName(d.Team.Slug)
	if !env.NodePool.Dedicated {
		for _, gvk := range []schema.GroupVersionKind{gvkNodePool, gvkNodeClass} {
			live, err := c.client.Get(ctx, gvk, "", name)
			if err != nil {
				return err
			}
			if live == nil || !render.IsManaged(live) {
				continue
			}
			if err := c.client.Delete(ctx, gvk, "", name); err != nil {
				return fmt.Errorf("delete %s %s: %w", gvk.Kind, name, err)
			}
			res.Changed = append(res.Changed, engine.ObjectName(gvk.Kind, "", name))
		}
		return nil
	}
	for _, o := range []*unstructured.Unstructured{render.NodeClass(d, env.NodePool.Tags, c.nodePool), render.NodePool(d)} {
		r, err := c.client.Apply(ctx, o)
		if r.Modified() {
			res.Changed = append(res.Changed, r.Name)
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	return nil
}

func (c *AgentCluster) pruneExternalSecrets(ctx context.Context, d agent.Desired, keep map[string]bool) error {
	live, err := c.client.List(ctx, gvkExternalSecret, d.Team.Namespace, render.LabelResourceID+"="+d.ID)
	if err != nil {
		return err
	}
	for _, o := range live {
		if keep[o.GetName()] || !render.IsManaged(&o) {
			continue
		}
		if err := c.client.Delete(ctx, gvkExternalSecret, d.Team.Namespace, o.GetName()); err != nil {
			return fmt.Errorf("delete ExternalSecret %s: %w", o.GetName(), err)
		}
	}
	return nil
}

// DeleteAgent implements agent.Cluster: the Teammate first (its pod stops), then the objects it referenced.
// Volume claims and snapshots are not touched; the on-delete rule decides them.
func (c *AgentCluster) DeleteAgent(ctx context.Context, d agent.Desired) error {
	ns := d.Team.Namespace
	var errs []error
	del := func(gvk schema.GroupVersionKind, name string) {
		if err := c.client.Delete(ctx, gvk, ns, name); err != nil {
			errs = append(errs, fmt.Errorf("delete %s %s: %w", gvk.Kind, name, err))
		}
	}
	del(agentictm.GVK(), agent.WorkloadName(d.Name))
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	del(gvkExternalSecret, agent.EnvSecretName(d.Name))
	del(gvkExternalSecret, agent.FilesSecretName(d.Name))
	del(gvkConfigMap, agent.ConfigMapName(d.Name))
	del(gvkConfigMap, agent.AWSConfigMapName(d.Name))
	del(gvkServiceAcct, agent.ServiceAccountName(d.Name))
	return errors.Join(errs...)
}

// TeammateGone reports whether the agent's workload and its pods are gone (the delete step waits for it
// before it touches the volume).
func (c *AgentCluster) TeammateGone(ctx context.Context, d agent.Desired) (bool, error) {
	live, err := c.client.Get(ctx, agentictm.GVK(), d.Team.Namespace, agent.WorkloadName(d.Name))
	if err != nil {
		return false, err
	}
	return live == nil, nil
}

// Volume implements agent.Volumes.
func (c *AgentCluster) Volume(ctx context.Context, d agent.Desired) (agent.VolumeState, error) {
	pvc, err := c.client.Get(ctx, gvkPVC, d.Team.Namespace, agent.PVCName(d.Name))
	if err != nil || pvc == nil {
		return agent.VolumeState{}, err
	}
	st := agent.VolumeState{Exists: true}
	phase, _, _ := unstructured.NestedString(pvc.Object, "status", "phase")
	st.Bound = phase == "Bound"
	st.RequestedGiB = quantityGiB(pvc.Object, "spec", "resources", "requests", "storage")
	st.CapacityGiB = quantityGiB(pvc.Object, "status", "capacity", "storage")
	conds, _, _ := unstructured.NestedSlice(pvc.Object, "status", "conditions")
	for _, raw := range conds {
		m, _ := raw.(map[string]any)
		if (m["type"] == "Resizing" || m["type"] == "FileSystemResizePending") && m["status"] == "True" {
			st.Resizing = true
		}
	}
	return st, nil
}

func quantityGiB(obj map[string]any, fields ...string) int {
	s, found, _ := unstructured.NestedString(obj, fields...)
	if !found {
		return 0
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return int(q.Value() / (1 << 30))
}

// Expand implements agent.Volumes: an online expansion is a patch of the claim's storage request (the CSI
// driver and the file system follow). The claim is never shrunk: a smaller or equal size changes nothing.
func (c *AgentCluster) Expand(ctx context.Context, d agent.Desired) (bool, error) {
	st, err := c.Volume(ctx, d)
	if err != nil {
		return false, err
	}
	if !st.Exists || st.RequestedGiB >= d.Volume.SizeGiB {
		return false, nil
	}
	ri, err := c.client.iface(gvkPVC, d.Team.Namespace)
	if err != nil {
		return false, err
	}
	patch := fmt.Sprintf(`{"spec":{"resources":{"requests":{"storage":%q}}}}`, render.StorageQuantity(d.Volume.SizeGiB))
	if _, err := ri.Patch(ctx, agent.PVCName(d.Name), types.MergePatchType, []byte(patch), metav1.PatchOptions{FieldManager: FieldManager}); err != nil {
		return false, wrap(err)
	}
	return true, nil
}

// Restore implements agent.Volumes: the EBS snapshot is adopted into the cluster (a VolumeSnapshotContent
// with the snapshot handle and a VolumeSnapshot bound to it) and the claim is re-created from it under the
// name the StatefulSet expects.
func (c *AgentCluster) Restore(ctx context.Context, d agent.Desired, ebsSnapshotID string) error {
	name := render.RestoreSnapshotName(d.ID)
	for _, o := range []*unstructured.Unstructured{
		render.SnapshotClass(d),
		render.ImportedContent(d, name, ebsSnapshotID),
		render.ImportedSnapshot(d, name),
		render.RestoredPVC(d, name),
	} {
		if _, err := c.client.Apply(ctx, o); err != nil {
			return fmt.Errorf("%s %s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	return nil
}

package k8s

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

var gvkDeployment = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

// PoolOwnedKinds are the kinds the pool controller creates and the drift watch should observe, appended to
// OwnedKinds and AgentOwnedKinds (the ServiceAccount, ConfigMap, ExternalSecret and CiliumNetworkPolicy are
// watched already). A change to a Deployment that is not the reconciler's own (a `kubectl scale`) reconciles
// the pool, whose apply puts the replica count back.
func PoolOwnedKinds() []schema.GroupVersionKind { return []schema.GroupVersionKind{gvkDeployment} }

// PoolCluster implements pool.Cluster on the Kubernetes API (RC-11).
type PoolCluster struct {
	client   *Client
	nodePool render.NodePoolConfig
}

// NewPoolCluster returns a PoolCluster. np is the platform's node class base for dedicated teams.
func NewPoolCluster(c *Client, np render.NodePoolConfig) *PoolCluster {
	return &PoolCluster{client: c, nodePool: np}
}

var _ pool.Cluster = (*PoolCluster)(nil)

// ApplyPool implements pool.Cluster. The objects the pods reference (ServiceAccount, ConfigMaps,
// ExternalSecrets, the shared claim, the team's nodes) and the egress policy go first and the Deployment last,
// so a worker never starts with a reference that does not exist yet. Objects are applied independently and
// the first error is returned; objects the pool no longer has are removed.
func (c *PoolCluster) ApplyPool(ctx context.Context, env pool.Environment) (pool.ApplyResult, error) {
	d := env.Pool
	var res pool.ApplyResult
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
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	note(c.reconcileNodePool(ctx, env, &res))
	apply(render.PoolServiceAccount(d))
	apply(render.PoolConfigMap(d))
	apply(render.PoolAWSConfigMap(d))
	rendered := map[string]bool{}
	for _, es := range render.PoolExternalSecrets(d) {
		rendered[es.GetName()] = true
		apply(es)
	}
	note(c.pruneExternalSecrets(ctx, d, rendered))
	apply(render.PoolEgress(d.Team.Namespace, d, env.Egress))
	if d.SharedVolume == pool.SharedEFS {
		apply(render.SharedClaim(env))
	} else {
		note(c.deleteIfManaged(ctx, gvkPVC, d.Team.Namespace, pool.SharedClaimName(d.Name), &res))
	}
	if firstErr != nil {
		return res, firstErr
	}
	dep, err := render.PoolDeployment(env)
	if err != nil {
		return res, err
	}
	apply(dep)
	return res, firstErr
}

// deleteIfManaged removes an object the reconciler created and reports it as changed.
func (c *PoolCluster) deleteIfManaged(ctx context.Context, gvk schema.GroupVersionKind, ns, name string, res *pool.ApplyResult) error {
	live, err := c.client.Get(ctx, gvk, ns, name)
	if err != nil || live == nil || !render.IsManaged(live) {
		return err
	}
	if err := c.client.Delete(ctx, gvk, ns, name); err != nil {
		return fmt.Errorf("delete %s %s: %w", gvk.Kind, name, err)
	}
	res.Changed = append(res.Changed, engine.ObjectName(gvk.Kind, ns, name))
	return nil
}

// reconcileNodePool applies the team's NodePool and EC2NodeClass for the dedicated tier and removes them for
// the shared tier, exactly as the agent controller does: the objects belong to the team, and a team with
// only pools needs them as much as a team with agents.
func (c *PoolCluster) reconcileNodePool(ctx context.Context, env pool.Environment, res *pool.ApplyResult) error {
	d := env.Pool
	team := agent.Desired{Team: d.Team}
	name := render.NodePoolName(d.Team.Slug)
	if !env.NodePool.Dedicated {
		for _, gvk := range []schema.GroupVersionKind{gvkNodePool, gvkNodeClass} {
			if err := c.deleteIfManaged(ctx, gvk, "", name, res); err != nil {
				return err
			}
		}
		return nil
	}
	for _, o := range []*unstructured.Unstructured{render.NodeClass(team, env.NodePool.Tags, c.nodePool), render.NodePool(team)} {
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

func (c *PoolCluster) pruneExternalSecrets(ctx context.Context, d pool.Desired, keep map[string]bool) error {
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

// Workers implements pool.Cluster.
func (c *PoolCluster) Workers(ctx context.Context, d pool.Desired) (pool.Workers, error) {
	live, err := c.client.Get(ctx, gvkDeployment, d.Team.Namespace, pool.WorkloadName(d.Name))
	if err != nil || live == nil {
		return pool.Workers{}, err
	}
	desired, _, _ := unstructured.NestedInt64(live.Object, "spec", "replicas")
	ready, _, _ := unstructured.NestedInt64(live.Object, "status", "readyReplicas")
	return pool.Workers{Exists: true, Desired: int32(desired), Ready: int32(ready)}, nil //nolint:gosec // bounded by the pool's 0..200 workers
}

// DeletePool implements pool.Cluster: the Deployment first (its workers stop), then what it referenced. The
// shared volume claim is not touched: the finalizer deletes it once the workers are gone.
func (c *PoolCluster) DeletePool(ctx context.Context, d pool.Desired) error {
	ns := d.Team.Namespace
	if err := c.client.Delete(ctx, gvkDeployment, ns, pool.WorkloadName(d.Name)); err != nil {
		return fmt.Errorf("delete Deployment %s: %w", pool.WorkloadName(d.Name), err)
	}
	var errs []error
	del := func(gvk schema.GroupVersionKind, name string) {
		if err := c.client.Delete(ctx, gvk, ns, name); err != nil {
			errs = append(errs, fmt.Errorf("delete %s %s: %w", gvk.Kind, name, err))
		}
	}
	del(gvkExternalSecret, pool.EnvSecretName(d.Name))
	del(gvkExternalSecret, pool.FilesSecretName(d.Name))
	del(gvkConfigMap, pool.ConfigMapName(d.Name))
	del(gvkConfigMap, pool.AWSConfigMapName(d.Name))
	del(gvkCiliumNetworkPolicy, pool.EgressPolicyName(d.ID))
	del(gvkServiceAcct, pool.ServiceAccountName(d.Name))
	return errors.Join(errs...)
}

package k8s

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/egress"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

var gvkCiliumNetworkPolicy = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNetworkPolicy"}

// EgressCluster implements egress.Cluster: it renders each agent's egress policy and applies it with
// server-side apply (M2-H1). Policies change in place; no pod is touched.
type EgressCluster struct{ client *Client }

// NewEgressCluster returns an EgressCluster.
func NewEgressCluster(c *Client) *EgressCluster { return &EgressCluster{client: c} }

var _ egress.Cluster = (*EgressCluster)(nil)

// ApplyAgentPolicy implements egress.Cluster.
func (c *EgressCluster) ApplyAgentPolicy(ctx context.Context, namespace string, p egress.AgentPolicy) (egress.ApplyResult, error) {
	obj := render.CiliumAgentEgress(namespace, p)
	r, err := c.client.Apply(ctx, obj)
	var res egress.ApplyResult
	if r.Modified() {
		res.Changed = append(res.Changed, r.Name)
	}
	if err != nil {
		return res, fmt.Errorf("%s %s: %w", obj.GetKind(), obj.GetName(), err)
	}
	return res, nil
}

// PruneAgentPolicies implements egress.Cluster: a managed egress policy of the team whose agent is not
// in keep is deleted. Policies the reconciler does not manage are never listed.
func (c *EgressCluster) PruneAgentPolicies(ctx context.Context, namespace, teamID string, keep []string) (egress.ApplyResult, error) {
	var res egress.ApplyResult
	live, err := c.client.List(ctx, gvkCiliumNetworkPolicy, namespace,
		render.LabelManagedBy+"="+render.ManagedByValue+","+render.LabelResourceKind+"="+render.EgressKind+","+render.LabelTeamID+"="+teamID)
	if err != nil {
		return res, fmt.Errorf("list egress policies: %w", err)
	}
	var first error
	for i := range live {
		o := &live[i]
		if slices.Contains(keep, o.GetLabels()[render.LabelAgentID]) {
			continue
		}
		if err := c.client.Delete(ctx, gvkCiliumNetworkPolicy, namespace, o.GetName()); err != nil {
			if first == nil {
				first = fmt.Errorf("delete %s: %w", o.GetName(), err)
			}
			continue
		}
		res.Changed = append(res.Changed, engine.ObjectName(o.GetKind(), namespace, o.GetName()))
	}
	return res, first
}

package k8s

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/logs"
)

// LogCluster implements logs.Cluster: it renders a team's Fluent Bit aggregator and applies it with
// server-side apply (M2-I3).
type LogCluster struct {
	client         *Client
	routeNamespace string
}

// NewLogCluster returns a LogCluster. routeNamespace holds the per-team route ConfigMaps the node
// collector reads (render.DefaultLogRouteNamespace when empty).
func NewLogCluster(c *Client, routeNamespace string) *LogCluster {
	if routeNamespace == "" {
		routeNamespace = render.DefaultLogRouteNamespace
	}
	return &LogCluster{client: c, routeNamespace: routeNamespace}
}

var _ logs.Cluster = (*LogCluster)(nil)

// ApplyAggregator implements logs.Cluster. Objects are applied independently; the first error is
// returned after the others were attempted. Optional objects the spec no longer needs are deleted.
func (c *LogCluster) ApplyAggregator(ctx context.Context, spec logs.Spec) (logs.ApplyResult, error) {
	agg, err := render.LogAggregatorFor(spec, c.routeNamespace)
	if err != nil {
		return logs.ApplyResult{}, engine.Terminal(err)
	}
	var res logs.ApplyResult
	var first error
	for _, o := range agg.Objects {
		r, err := c.client.Apply(ctx, o)
		if r.Modified() {
			res.Changed = append(res.Changed, r.Name)
		}
		if err != nil && first == nil {
			first = fmt.Errorf("%s %s: %w", o.GetKind(), o.GetName(), err)
		}
	}
	for _, o := range agg.Optional {
		if changed, err := c.deleteManaged(ctx, schema.FromAPIVersionAndKind(o.APIVersion, o.Kind), o.Namespace, o.Name); err != nil && first == nil {
			first = err
		} else if changed {
			res.Changed = append(res.Changed, engine.ObjectName(o.Kind, o.Namespace, o.Name))
		}
	}
	return res, first
}

// RemoveAggregator implements logs.Cluster: every object the controller owns for the team is deleted
// when it carries the managed-by label.
func (c *LogCluster) RemoveAggregator(ctx context.Context, namespace, teamID string) (logs.ApplyResult, error) {
	var res logs.ApplyResult
	var first error
	gv := func(g, v, k string) schema.GroupVersionKind {
		return schema.GroupVersionKind{Group: g, Version: v, Kind: k}
	}
	for _, t := range []struct {
		gvk       schema.GroupVersionKind
		namespace string
		name      string
	}{
		{gv("apps", "v1", "Deployment"), namespace, render.LogAggregatorName},
		{gv("", "v1", "Service"), namespace, render.LogAggregatorName},
		{gv("cilium.io", "v2", "CiliumNetworkPolicy"), namespace, render.LogAggregatorName},
		{gv("external-secrets.io", "v1", "ExternalSecret"), namespace, render.LogCredentialsName},
		{gv("external-secrets.io", "v1", "ExternalSecret"), namespace, render.LogRedactionValuesName},
		{gv("", "v1", "Secret"), namespace, render.LogRedactionKeyName},
		{gv("", "v1", "ConfigMap"), namespace, render.LogAggregatorName},
		{gv("", "v1", "ServiceAccount"), namespace, render.LogAggregatorName},
	} {
		changed, err := c.deleteManaged(ctx, t.gvk, t.namespace, t.name)
		if err != nil && first == nil {
			first = err
		}
		if changed {
			res.Changed = append(res.Changed, engine.ObjectName(t.gvk.Kind, t.namespace, t.name))
		}
	}
	// The route ConfigMap is found by its labels: its name carries the team slug, which the caller of a
	// removal may not have.
	routes, err := c.client.List(ctx, gv("", "v1", "ConfigMap"), c.routeNamespace,
		render.LabelLogRoute+"=true,"+render.LabelTeamID+"="+teamID+","+render.LabelManagedBy+"="+render.ManagedByValue)
	if err != nil && first == nil {
		first = err
	}
	for i := range routes {
		if err := c.client.Delete(ctx, gv("", "v1", "ConfigMap"), routes[i].GetNamespace(), routes[i].GetName()); err != nil && first == nil {
			first = err
		} else if err == nil {
			res.Changed = append(res.Changed, engine.ObjectName("ConfigMap", routes[i].GetNamespace(), routes[i].GetName()))
		}
	}
	return res, first
}

// deleteManaged deletes an object the reconciler owns and reports whether it existed. An object
// without the managed-by label is left alone.
func (c *LogCluster) deleteManaged(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (bool, error) {
	live, err := c.client.Get(ctx, gvk, namespace, name)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", gvk.Kind, name, err)
	}
	if live == nil || !render.IsManaged(live) {
		return false, nil
	}
	if err := c.client.Delete(ctx, gvk, namespace, name); err != nil {
		return false, fmt.Errorf("%s %s: %w", gvk.Kind, name, err)
	}
	return true, nil
}

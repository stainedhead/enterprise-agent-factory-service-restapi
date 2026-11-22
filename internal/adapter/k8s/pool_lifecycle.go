package k8s

import (
	"context"
	"errors"
	"fmt"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/lifecycle"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/pool"
)

// PoolLifecycle implements the Kubernetes ports of the pool delete steps (lifecycle.PoolWorkloads and
// lifecycle.PoolVolumes) on top of PoolCluster.
type PoolLifecycle struct{ *PoolCluster }

// NewPoolLifecycle returns the lifecycle ports of cluster.
func NewPoolLifecycle(c *PoolCluster) *PoolLifecycle { return &PoolLifecycle{c} }

var (
	_ lifecycle.PoolWorkloads = (*PoolLifecycle)(nil)
	_ lifecycle.PoolVolumes   = (*PoolLifecycle)(nil)
)

// Stop implements lifecycle.PoolWorkloads: the Deployment and what it referenced are removed.
func (l *PoolLifecycle) Stop(ctx context.Context, d pool.Desired) error { return l.DeletePool(ctx, d) }

// Stopped implements lifecycle.PoolWorkloads: the Deployment and every worker pod labeled with the pool are
// gone, so no worker still holds a volume.
func (l *PoolLifecycle) Stopped(ctx context.Context, d pool.Desired) (bool, error) {
	live, err := l.client.Get(ctx, gvkDeployment, d.Team.Namespace, pool.WorkloadName(d.Name))
	if err != nil || live != nil {
		return false, err
	}
	pods, err := l.client.List(ctx, gvkPod, d.Team.Namespace, render.LabelPoolID+"="+d.ID)
	if err != nil {
		return false, err
	}
	return len(pods) == 0, nil
}

// DeleteVolumes implements lifecycle.PoolVolumes: the shared claim, and any scratch claim a worker left
// behind (a scratch claim goes with its pod, so none should remain; one that does is deleted here). A
// missing claim is success.
func (l *PoolLifecycle) DeleteVolumes(ctx context.Context, d pool.Desired) error {
	ns := d.Team.Namespace
	var errs []error
	if err := l.client.Delete(ctx, gvkPVC, ns, pool.SharedClaimName(d.Name)); err != nil {
		errs = append(errs, fmt.Errorf("delete claim %s: %w", pool.SharedClaimName(d.Name), err))
	}
	claims, err := l.client.List(ctx, gvkPVC, ns, render.LabelPoolID+"="+d.ID)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, c := range claims {
		if err := l.client.Delete(ctx, gvkPVC, ns, c.GetName()); err != nil {
			errs = append(errs, fmt.Errorf("delete claim %s: %w", c.GetName(), err))
		}
	}
	return errors.Join(errs...)
}

// VolumesGone implements lifecycle.PoolVolumes: no claim of the pool is left.
func (l *PoolLifecycle) VolumesGone(ctx context.Context, d pool.Desired) (bool, error) {
	claims, err := l.client.List(ctx, gvkPVC, d.Team.Namespace, render.LabelPoolID+"="+d.ID)
	if err != nil {
		return false, err
	}
	shared, err := l.client.Get(ctx, gvkPVC, d.Team.Namespace, pool.SharedClaimName(d.Name))
	if err != nil {
		return false, err
	}
	return len(claims) == 0 && shared == nil, nil
}

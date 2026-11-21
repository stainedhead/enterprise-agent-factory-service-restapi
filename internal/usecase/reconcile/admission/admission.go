// Package admission reconciles the cluster's admission guardrails for team namespaces (RC-19): the
// policies that keep agent workloads from loosening their own network policy, exposing themselves,
// escaping the node or claiming another team's dedicated nodes. The policy documents are rendered by the
// Kubernetes adapter; this controller applies them at startup and every resync, and the drift watch
// restores a deleted or edited policy or binding.
package admission

import (
	"context"
	"errors"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// Ref is the singleton the engine reconciles.
var Ref = engine.NewRef(engine.KindAdmission, "afs-admission", "")

// Spec is what the policies depend on.
type Spec struct {
	// ReconcilerUsers are the Kubernetes usernames allowed to change network policies and AFS labels:
	// the reconciler's own identity (for EKS, its access entry's username).
	ReconcilerUsers []string
}

// ApplyResult lists the objects an apply created or changed, by engine.ObjectName.
type ApplyResult struct{ Changed []string }

// Cluster applies the admission policies and their bindings. Implementations are idempotent.
type Cluster interface {
	ApplyAdmission(ctx context.Context, s Spec) (ApplyResult, error)
}

// Controller reconciles the admission singleton (engine.KindAdmission).
type Controller struct {
	cluster Cluster
	spec    Spec
}

// NewController returns a Controller. At least one reconciler user is required: without it nobody could
// manage network policies.
func NewController(cluster Cluster, spec Spec) (*Controller, error) {
	if cluster == nil {
		return nil, errors.New("admission: a cluster is required")
	}
	if len(spec.ReconcilerUsers) == 0 {
		return nil, errors.New("admission: at least one reconciler user is required")
	}
	return &Controller{cluster: cluster, spec: spec}, nil
}

// Kind implements engine.Controller.
func (*Controller) Kind() engine.Kind { return engine.KindAdmission }

// Reconcile implements engine.Controller. A singleton has no status row.
func (c *Controller) Reconcile(ctx context.Context, _ engine.Ref) (engine.Outcome, error) {
	res, err := c.cluster.ApplyAdmission(ctx, c.spec)
	if err != nil {
		return engine.Outcome{}, engine.WithCondition("AdmissionReady", err)
	}
	return engine.Outcome{NoStatus: true, Changed: res.Changed}, nil
}

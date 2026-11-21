package team

import (
	"context"
	"errors"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// PlatformRef is the singleton the engine reconciles for the cluster-wide objects.
var PlatformRef = engine.NewRef(engine.KindPlatform, "afs-platform", "")

// PlatformController reconciles the cluster-wide objects team environments rely on: the Cilium policy
// on afs-gateway that admits only pods of team namespaces (D57). It runs at startup and at every
// resync, and the drift watch reverts edits to the policy.
type PlatformController struct {
	cluster PlatformCluster
	spec    PlatformSpec
}

// NewPlatformController returns a PlatformController; zero Gateway fields take the defaults.
func NewPlatformController(cluster PlatformCluster, spec PlatformSpec) (*PlatformController, error) {
	if cluster == nil {
		return nil, errors.New("team: a platform cluster is required")
	}
	if spec.GatewayNamespace == "" {
		spec.GatewayNamespace = DefaultGatewayNamespace
	}
	if spec.GatewayName == "" {
		spec.GatewayName = DefaultGatewayName
	}
	if spec.GatewayPort == 0 {
		spec.GatewayPort = DefaultGatewayPort
	}
	return &PlatformController{cluster: cluster, spec: spec}, nil
}

// Kind implements engine.Controller.
func (*PlatformController) Kind() engine.Kind { return engine.KindPlatform }

// Reconcile implements engine.Controller. A singleton has no status row.
func (c *PlatformController) Reconcile(ctx context.Context, _ engine.Ref) (engine.Outcome, error) {
	res, err := c.cluster.ApplyPlatform(ctx, c.spec)
	if err != nil {
		return engine.Outcome{}, engine.WithCondition("PlatformReady", err)
	}
	return engine.Outcome{NoStatus: true, Changed: res.Changed}, nil
}

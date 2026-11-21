package k8s

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/k8s/render"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/team"
)

// GatewayCASource locates the ConfigMap holding the gateway's CA bundle, which cert-manager keeps
// current; the reconciler copies it into every team namespace.
type GatewayCASource struct {
	Namespace string
	Name      string
}

// ErrGatewayCAMissing is returned when the source ConfigMap or its key is absent.
var ErrGatewayCAMissing = errors.New("k8s: gateway CA bundle not found")

// TeamCluster implements team.Cluster and team.PlatformCluster: it renders the team's objects and
// applies them with server-side apply.
type TeamCluster struct {
	client *Client
	ca     GatewayCASource
}

// NewTeamCluster returns a TeamCluster.
func NewTeamCluster(c *Client, ca GatewayCASource) *TeamCluster {
	return &TeamCluster{client: c, ca: ca}
}

var (
	_ team.Cluster         = (*TeamCluster)(nil)
	_ team.PlatformCluster = (*TeamCluster)(nil)
)

var gvkConfigMap = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

// ApplyTeam implements team.Cluster. The namespace goes first; if it cannot be applied nothing else
// is attempted. Objects after it are applied independently and the first error is returned.
func (c *TeamCluster) ApplyTeam(ctx context.Context, env team.Environment) (team.ApplyResult, error) {
	var res team.ApplyResult
	ns, err := c.client.Apply(ctx, render.Namespace(env.Team))
	if ns.Modified() {
		res.Changed = append(res.Changed, ns.Name)
	}
	if err != nil {
		return res, fmt.Errorf("namespace %s: %w", env.Team.Namespace, err)
	}

	objs := []*unstructured.Unstructured{render.ResourceQuota(env.Team), render.LimitRange(env.Team)}
	objs = append(objs, render.DefaultDeny(env.Team)...)
	objs = append(objs, render.CiliumTeamPolicy(env), render.SecretStore(env))
	var firstErr error
	if bundle, err := c.gatewayCA(ctx); err != nil {
		firstErr = err
	} else {
		objs = append(objs, render.GatewayCA(env.Team, bundle))
	}
	for _, o := range objs {
		r, err := c.client.Apply(ctx, o)
		if r.Modified() {
			res.Changed = append(res.Changed, r.Name)
		}
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", o.GetKind(), err)
		}
	}
	return res, firstErr
}

// ApplyPlatform implements team.PlatformCluster.
func (c *TeamCluster) ApplyPlatform(ctx context.Context, p team.PlatformSpec) (team.ApplyResult, error) {
	r, err := c.client.Apply(ctx, render.CiliumGatewayIngress(p))
	var res team.ApplyResult
	if r.Modified() {
		res.Changed = append(res.Changed, r.Name)
	}
	if err != nil {
		return res, fmt.Errorf("gateway ingress policy: %w", err)
	}
	return res, nil
}

func (c *TeamCluster) gatewayCA(ctx context.Context) (string, error) {
	src, err := c.client.Get(ctx, gvkConfigMap, c.ca.Namespace, c.ca.Name)
	if err != nil {
		return "", err
	}
	if src == nil {
		return "", &engine.CodedError{Code: "GatewayCAMissing", Err: fmt.Errorf("%w: ConfigMap %s/%s", ErrGatewayCAMissing, c.ca.Namespace, c.ca.Name)}
	}
	data, _, _ := unstructured.NestedStringMap(src.Object, "data")
	if data[render.GatewayCAKey] == "" {
		return "", &engine.CodedError{Code: "GatewayCAMissing", Err: fmt.Errorf("%w: key %s in ConfigMap %s/%s", ErrGatewayCAMissing, render.GatewayCAKey, c.ca.Namespace, c.ca.Name)}
	}
	return data[render.GatewayCAKey], nil
}

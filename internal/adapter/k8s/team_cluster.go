package k8s

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
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
	if env.Team.Gateway.PerNamespace() {
		gw, err := render.TeamGateway(env)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		objs = append(objs, gw...)
	}
	hosted, err := render.McpHosted(env)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	objs = append(objs, hosted...)
	for _, o := range objs {
		r, err := c.client.Apply(ctx, o)
		if r.Modified() {
			res.Changed = append(res.Changed, r.Name)
		}
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", o.GetKind(), err)
		}
	}
	if !env.Team.Gateway.PerNamespace() {
		if err := c.removeTeamGateway(ctx, env.Team.Namespace); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := c.removeMcpHosted(ctx, env.Team.Namespace, render.McpHostedSlugs(env)); err != nil && firstErr == nil {
		firstErr = err
	}
	return res, firstErr
}

// removeMcpHosted deletes the objects of hosted MCP servers the reconciler made whose server is no longer
// rendered (switched off, rejected, removed, or the team is back on the shared gateway).
func (c *TeamCluster) removeMcpHosted(ctx context.Context, ns string, keep []string) error {
	for _, gvk := range render.McpHostedGVKs {
		live, err := c.client.List(ctx, gvk, ns, render.LabelMcpServer)
		if err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}
			return fmt.Errorf("hosted MCP %s: %w", gvk.Kind, err)
		}
		for i := range live {
			o := &live[i]
			if !render.IsManaged(o) || slices.Contains(keep, o.GetLabels()[render.LabelMcpServer]) {
				continue
			}
			if err := c.client.Delete(ctx, gvk, ns, o.GetName()); err != nil {
				return fmt.Errorf("hosted MCP %s %s: %w", gvk.Kind, o.GetName(), err)
			}
		}
	}
	return nil
}

// removeTeamGateway deletes the objects of a team gateway the reconciler made, when the team is back on the
// shared gateway. An object that is absent or not managed by the reconciler is left alone.
func (c *TeamCluster) removeTeamGateway(ctx context.Context, ns string) error {
	for _, o := range render.TeamGatewayGVKs {
		live, err := c.client.Get(ctx, o.GVK, ns, o.Name)
		if err != nil {
			if meta.IsNoMatchError(err) {
				continue // a CRD that is not installed holds no object
			}
			return fmt.Errorf("team gateway %s: %w", o.GVK.Kind, err)
		}
		if live == nil || !render.IsManaged(live) {
			continue
		}
		if err := c.client.Delete(ctx, o.GVK, ns, o.Name); err != nil {
			return fmt.Errorf("team gateway %s: %w", o.GVK.Kind, err)
		}
	}
	return nil
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

// teamNamespacePrefix is the prefix of every team namespace (afs-t-<slug>).
const teamNamespacePrefix = "afs-t-"

var gvkNamespaceObj = schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}

// DeleteNamespace implements lifecycle.Namespaces: it asks for the deletion of a team namespace the
// reconciler created and reports whether it is already gone. A namespace outside afs-t-* or without the
// managed-by label is refused (ErrNotManaged); one that does not exist is gone. A terminating
// namespace is not gone: the finalizer checks again on its next pass.
func (c *TeamCluster) DeleteNamespace(ctx context.Context, name string) (bool, error) {
	if !strings.HasPrefix(name, teamNamespacePrefix) {
		return false, fmt.Errorf("%w: namespace %s is not a team namespace", ErrNotManaged, name)
	}
	live, err := c.client.Get(ctx, gvkNamespaceObj, "", name)
	if err != nil {
		return false, err
	}
	if live == nil {
		return true, nil
	}
	if !render.IsManaged(live) {
		return false, fmt.Errorf("%w: namespace %s", ErrNotManaged, name)
	}
	if live.GetDeletionTimestamp() != nil {
		return false, nil
	}
	if err := c.client.Delete(ctx, gvkNamespaceObj, "", name); err != nil {
		return false, err
	}
	return false, nil
}

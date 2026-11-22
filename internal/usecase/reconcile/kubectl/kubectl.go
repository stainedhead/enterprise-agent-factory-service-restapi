// Package kubectl reconciles viewer kubectl access (RC-13, M5-E2, RC-US-11.1, ACC-5): when a team's
// viewerKubectl guardrail is on, the people who sign in through an identity provider that federates to an
// IAM role can run read-only kubectl in the team's namespace; when it is off, they cannot.
//
// Two objects carry the access, and the reconciler keeps both:
//
//   - an EKS access entry for the provider's federated role, mapped to one Kubernetes group that is
//     specific to that role (GroupFor). An access entry is per principal and per cluster, so one entry
//     serves every team that uses the role; it exists while at least one team needs the role.
//   - in each team namespace, a Role with get, list and watch on workloads, events and logs only (no
//     secrets, exec, attach, port-forward or write) and a RoleBinding of that Role to the groups of the
//     team's roles.
//
// Which role belongs to which provider is configuration (OQ-9, ProviderRoles): a provider with no
// configured role gives no access, so a team whose providers have none gets no access entry and no
// RoleBinding. Turning the flag off removes the RoleBinding first (access ends with that call) and then
// the entry when no other team needs it.
//
// The resource is the team (engine kind Kind, id = team id). A pass is cheap and idempotent: it applies or
// removes the team's two namespace objects and then makes the cluster's viewer access entries equal to what
// every team with the flag on needs, under a lock so two teams' passes cannot interleave.
package kubectl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// Kind is the engine kind of a team's viewer kubectl access. Its id is the team id.
const Kind engine.Kind = "kubectl"

// DefaultProvider is the ProviderRoles key of the default identity provider.
const DefaultProvider = "default"

// ResyncEvery is how often a team's access is reconciled with no change at all, to repair a deleted entry
// or RoleBinding.
const ResyncEvery = 5 * time.Minute

// GroupFor names the Kubernetes group an IAM role's access entry maps to. It is a function of the role ARN,
// so the entry and every namespace RoleBinding agree without storing anything.
func GroupFor(roleARN string) string {
	sum := sha256.Sum256([]byte(roleARN))
	return "afs-kubectl-viewers-" + hex.EncodeToString(sum[:])[:12]
}

// Provider is an enabled identity provider a team's people sign in through.
type Provider struct {
	ID        string
	IsDefault bool
}

// Team is what the reconciler needs of a team.
type Team struct {
	ID        string
	Slug      string
	Namespace string
	// State is active, suspended, deleting or deleted.
	State         string
	ViewerKubectl bool
	// Providers are the enabled identity providers the team's people sign in through: the one the team
	// chose, or the service default when it chose none (identity providers are service-level, ADR-013),
	// so at most one today; a list keeps the roles of several providers possible.
	Providers []Provider
}

// Live reports whether the team's namespace is expected to exist.
func (t Team) live() bool { return t.State == "active" || t.State == "suspended" }

// Source reads teams.
type Source interface {
	// LoadTeam returns the team, or a domain NotFound error.
	LoadTeam(ctx context.Context, teamID string) (Team, error)
	// ListViewerTeams returns every live team whose viewerKubectl is on, with its providers.
	ListViewerTeams(ctx context.Context) ([]Team, error)
}

// ApplyResult lists the objects an apply or a removal changed, by engine.ObjectName.
type ApplyResult struct{ Changed []string }

// Cluster applies the namespace objects. Implementations are idempotent.
type Cluster interface {
	// ApplyViewerAccess applies the read-only Role and the RoleBinding to the groups, in the namespace.
	ApplyViewerAccess(ctx context.Context, namespace string, t Team, groups []string) (ApplyResult, error)
	// RemoveViewerAccess deletes the Role and the RoleBinding; a missing one is not an error.
	RemoveViewerAccess(ctx context.Context, namespace string) (ApplyResult, error)
}

// Entries keeps the EKS access entries of the viewer roles.
type Entries interface {
	// SyncViewerEntries makes the entries AFS manages for viewer kubectl exactly want (role ARN to
	// Kubernetes group): it creates the missing ones, corrects a wrong group and deletes the managed
	// ones not wanted. It returns what it changed.
	SyncViewerEntries(ctx context.Context, want map[string]string) ([]string, error)
}

// Config configures the Controller.
type Config struct {
	Source  Source
	Cluster Cluster
	Entries Entries
	// ProviderRoles maps an identity provider id, or DefaultProvider for the default provider, to the
	// ARN of the IAM role the provider federates to for kubectl (AFS_VIEWER_KUBECTL_ROLES). Empty: no
	// access is ever created (OQ-9 default).
	ProviderRoles map[string]string
}

// Controller reconciles Kind.
type Controller struct {
	cfg Config
	mu  sync.Mutex // serializes the cluster-wide entry sync
}

var _ engine.Controller = (*Controller)(nil)

// NewController validates cfg.
func NewController(cfg Config) (*Controller, error) {
	if cfg.Source == nil || cfg.Cluster == nil || cfg.Entries == nil {
		return nil, errors.New("kubectl: Source, Cluster and Entries are required")
	}
	return &Controller{cfg: cfg}, nil
}

// Kind implements engine.Controller.
func (*Controller) Kind() engine.Kind { return Kind }

// Routes are the change routes: a team's flag, state or namespace, and the identity providers (a service
// level provider reconciles every team, a team's own one that team).
func Routes() []engine.Route {
	own := []engine.Target{{Kind: Kind}}
	return []engine.Route{
		{Table: "teams", Targets: own},
		{Table: "identity_providers", Targets: own, FanOut: []engine.Kind{Kind}},
	}
}

// RolesFor returns the distinct role ARNs the team's providers federate to, sorted.
func RolesFor(t Team, providerRoles map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range t.Providers {
		arn := providerRoles[p.ID]
		if arn == "" && p.IsDefault {
			arn = providerRoles[DefaultProvider]
		}
		if arn != "" && !seen[arn] {
			seen[arn] = true
			out = append(out, arn)
		}
	}
	sort.Strings(out)
	return out
}

// Reconcile implements engine.Controller.
func (c *Controller) Reconcile(ctx context.Context, ref engine.Ref) (engine.Outcome, error) {
	out := engine.Outcome{NoStatus: true, Requeue: ResyncEvery}
	t, err := c.cfg.Source.LoadTeam(ctx, ref.ID)
	missing := false
	if err != nil {
		if !errors.Is(err, &domain.Error{Kind: domain.KindNotFound}) {
			return engine.Outcome{}, err
		}
		missing = true
	}
	var errs []error
	switch {
	case missing || !t.live():
		// The namespace goes with the team (RC-17); there is nothing to apply, and the entry sync below
		// drops the role once no team needs it.
	case t.ViewerKubectl && len(RolesFor(t, c.cfg.ProviderRoles)) > 0:
		var groups []string
		for _, arn := range RolesFor(t, c.cfg.ProviderRoles) {
			groups = append(groups, GroupFor(arn))
		}
		res, err := c.cfg.Cluster.ApplyViewerAccess(ctx, t.Namespace, t, groups)
		out.Changed = append(out.Changed, res.Changed...)
		if err != nil {
			errs = append(errs, fmt.Errorf("apply viewer access: %w", err))
		}
	default:
		// Off, or on with no role configured for any of the team's providers: no access at all.
		res, err := c.cfg.Cluster.RemoveViewerAccess(ctx, t.Namespace)
		out.Changed = append(out.Changed, res.Changed...)
		if err != nil {
			errs = append(errs, fmt.Errorf("remove viewer access: %w", err))
		}
	}
	// The binding is applied or removed before the entry is synced, so turning access off ends it at the
	// namespace at once and the entry follows.
	changed, err := c.syncEntries(ctx)
	out.Changed = append(out.Changed, changed...)
	if err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return out, errors.Join(errs...)
	}
	return out, nil
}

// syncEntries makes the access entries equal what every team with the flag on needs.
func (c *Controller) syncEntries(ctx context.Context) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	teams, err := c.cfg.Source.ListViewerTeams(ctx)
	if err != nil {
		return nil, fmt.Errorf("list teams with viewer kubectl: %w", err)
	}
	want := map[string]string{}
	for _, t := range teams {
		if !t.ViewerKubectl || !t.live() {
			continue
		}
		for _, arn := range RolesFor(t, c.cfg.ProviderRoles) {
			want[arn] = GroupFor(arn)
		}
	}
	changed, err := c.cfg.Entries.SyncViewerEntries(ctx, want)
	if err != nil {
		return changed, fmt.Errorf("sync access entries: %w", err)
	}
	return changed, nil
}

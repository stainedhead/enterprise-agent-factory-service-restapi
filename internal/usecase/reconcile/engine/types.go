package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Kind names a reconciled resource kind. It is the resource_kind of resource_status.
type Kind string

// Kinds known to the engine. Later milestones add their own.
const (
	KindTeam      Kind = "team"
	KindAgent     Kind = "agent"
	KindPool      Kind = "pool"
	KindAdmission Kind = "admission"
)

// Key identifies a work item: one resource of one kind. Two additions of the same key before it runs
// coalesce into one reconcile.
type Key struct {
	Kind Kind
	ID   string
}

// String returns kind/id.
func (k Key) String() string { return string(k.Kind) + "/" + k.ID }

// Ref is a Key plus the team that owns the resource. The team orders work: two resources of one team
// are never reconciled at the same time. Resources without a team (singletons) have an empty TeamID.
type Ref struct {
	Key
	TeamID string
}

// NewRef builds a Ref.
func NewRef(kind Kind, id, teamID string) Ref {
	return Ref{Key: Key{Kind: kind, ID: id}, TeamID: teamID}
}

// Change is one notification of channel afs_desired_state, payload "<table>:<team_id>:<id>" (database
// contract §4). TeamID is empty for service-level tables (agent_templates, models, service_tags, ...).
type Change struct {
	Table  string
	TeamID string
	ID     string
}

// ParseChange parses a notification payload.
func ParseChange(payload string) (Change, error) {
	parts := strings.SplitN(payload, ":", 3)
	if len(parts) != 3 || parts[0] == "" {
		return Change{}, fmt.Errorf("engine: malformed change payload %q", payload)
	}
	return Change{Table: parts[0], TeamID: parts[1], ID: parts[2]}, nil
}

// Target says which resource a change in a table concerns.
type Target struct {
	Kind Kind
	// Self is true when the changed row is the resource (kind, change.ID). False means the change
	// belongs to the team resource named by the payload's team part, whatever the changed row is.
	Self bool
}

// Route maps one table to the resources to reconcile when a row of it changes.
type Route struct {
	Table   string
	Targets []Target
	// FanOut lists the kinds to reconcile in full when the change has an empty team part (a
	// service-level table). A service-level change without FanOut is ignored.
	FanOut []Kind
}

// Registry is the table → controller routing table.
type Registry struct{ routes map[string]Route }

// NewRegistry builds a registry; a table may be routed once.
func NewRegistry(routes ...Route) (*Registry, error) {
	r := &Registry{routes: make(map[string]Route, len(routes))}
	for _, rt := range routes {
		if rt.Table == "" {
			return nil, fmt.Errorf("engine: route without a table")
		}
		if _, dup := r.routes[rt.Table]; dup {
			return nil, fmt.Errorf("engine: table %q routed twice", rt.Table)
		}
		r.routes[rt.Table] = rt
	}
	return r, nil
}

// Kinds returns the kinds some route reaches, sorted by first appearance is not guaranteed.
func (r *Registry) Kinds() []Kind {
	seen := map[Kind]bool{}
	var out []Kind
	add := func(k Kind) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, rt := range r.routes {
		for _, t := range rt.Targets {
			add(t.Kind)
		}
		for _, k := range rt.FanOut {
			add(k)
		}
	}
	return out
}

// Resolve returns the resources a change concerns. fan lists a kind's resources for service-level
// changes.
func (r *Registry) Resolve(ctx context.Context, c Change, fan func(context.Context, []Kind) ([]Ref, error)) ([]Ref, error) {
	rt, ok := r.routes[c.Table]
	if !ok {
		return nil, nil
	}
	if c.TeamID == "" {
		if len(rt.FanOut) == 0 {
			return nil, nil
		}
		return fan(ctx, rt.FanOut)
	}
	var out []Ref
	for _, t := range rt.Targets {
		switch {
		case t.Self && c.ID != "":
			out = append(out, NewRef(t.Kind, c.ID, c.TeamID))
		case !t.Self:
			out = append(out, NewRef(t.Kind, c.TeamID, c.TeamID))
		}
	}
	return out, nil
}

// Outcome is what a controller reports after a successful reconcile.
type Outcome struct {
	// Generation is the spec generation that was applied; it becomes observedGeneration.
	Generation int64
	// Phase defaults to Ready. Use Reconciling with Requeue while waiting for something asynchronous.
	Phase domain.Phase
	// Conditions replace the resource's conditions (Type, Status, Reason, Message; the engine keeps
	// lastTransitionTime when a condition's status did not change).
	Conditions []domain.Condition
	// Requeue asks to run again after this long (zero: only on change).
	Requeue time.Duration
	// NoStatus writes no resource_status row (the resource is gone, or it is a singleton).
	NoStatus bool
	// Changed names the objects this pass created or modified. When a drift watch flagged one of
	// them, the engine records it as corrected.
	Changed []string
}

// Controller reconciles one kind.
type Controller interface {
	Kind() Kind
	// Reconcile brings the resource to its desired state. It must be idempotent. An error is
	// retried with backoff; see ConditionError and Terminal for how it is reported.
	Reconcile(ctx context.Context, ref Ref) (Outcome, error)
}

// Lister finds resources outside any notification.
type Lister interface {
	// List returns every live resource of the kinds (full resync).
	List(ctx context.Context, kinds []Kind) ([]Ref, error)
	// Stale returns the resources whose generation differs from the observed generation, or that
	// have no status yet (the 30 s backstop).
	Stale(ctx context.Context, kinds []Kind) ([]Ref, error)
}

// FeedHandler receives what a ChangeFeed sees.
type FeedHandler struct {
	// Change is called for every notification.
	Change func(Change)
	// Connected is called after each (re)connection; the engine polls then, because notifications
	// sent while disconnected are lost.
	Connected func()
}

// ChangeFeed delivers notifications of channel afs_desired_state.
type ChangeFeed interface {
	// Listen blocks until ctx ends. It reconnects by itself and calls h.Connected after each
	// connection.
	Listen(ctx context.Context, h FeedHandler) error
}

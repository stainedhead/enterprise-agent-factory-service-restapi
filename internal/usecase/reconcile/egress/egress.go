// Package egress reconciles the per-agent network egress allowances (RC-US-5.1, EXT-5): for every agent
// of a team, a Cilium policy that lets the agent reach the endpoints of the external systems it may use
// and nothing else, on top of the team's default deny. The catalog is therefore enforced in the network,
// not only in the UI.
//
// A system is off for an agent when the service, the team or the agent switched it off
// (agent_external_access.disabled_at); an off system contributes no endpoint. A change of a system, an
// opt-out or an agent reconciles the team's egress, and a policy changes without touching any pod, so
// switching a system off removes its endpoints within the reconcile time (well inside 60 s) and
// restarts nothing.
//
// The resource is the team: engine kind Kind, id = team id. Agents' EgressPolicyReady condition is
// written onto each agent's status through AgentConditions; the team-level row also carries it.
package egress

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// Kind is the engine kind of a team's egress. Its id is the team id.
const Kind engine.Kind = "egress"

// DefaultPort is the port of an endpoint that names none.
const DefaultPort = 443

// Disabled levels, as agent_external_access.disabled_at reports them.
const (
	OffService = "service"
	OffTeam    = "team"
	OffAgent   = "agent"
	// OffPool is the level of a system a pool opted out of (pool_external_access).
	OffPool = "pool"
)

// System is one external system as an agent sees it.
type System struct {
	ID        string
	Slug      string
	Category  string
	Endpoints []string
	// DisabledAt is "" when the agent may use the system, else the level that switched it off.
	DisabledAt string
}

// Agent is an agent and the systems of its team's view.
type Agent struct {
	ID string
	// State is enabled, paused, deleting or deleted.
	State   string
	Systems []System
}

// Desired is a team's egress state.
type Desired struct {
	TeamID    string
	Namespace string
	// State is active, suspended, deleting or deleted.
	State  string
	Agents []Agent
}

// Endpoint is one network endpoint an agent may reach.
type Endpoint struct {
	// Host is a host name, or "*.domain" for every host below domain.
	Host string
	Port int
}

// Wildcard reports whether the host is a "*.domain" pattern.
func (e Endpoint) Wildcard() bool { return strings.HasPrefix(e.Host, "*.") }

// Rule is the allowance one system gives an agent.
type Rule struct {
	SystemID  string
	Slug      string
	Endpoints []Endpoint
}

// AgentPolicy is the egress allowance of one agent: the rules of the systems that are on for it, in
// slug order. No rules means the agent may reach nothing beyond the team's baseline.
type AgentPolicy struct {
	TeamID  string
	AgentID string
	Rules   []Rule
}

// ApplyResult lists the objects an apply created or changed, by engine.ObjectName.
type ApplyResult struct{ Changed []string }

// Cluster applies and prunes the agents' policies. Implementations are idempotent.
type Cluster interface {
	// ApplyAgentPolicy applies the policy of one agent in the team's namespace.
	ApplyAgentPolicy(ctx context.Context, namespace string, p AgentPolicy) (ApplyResult, error)
	// PruneAgentPolicies deletes the managed egress policies in the namespace whose agent is not in keep.
	PruneAgentPolicies(ctx context.Context, namespace, teamID string, keep []string) (ApplyResult, error)
}

// Source reads a team's egress state.
type Source interface {
	// LoadEgress returns the team, its agents and what each may reach, or a domain NotFound error.
	LoadEgress(ctx context.Context, teamID string) (Desired, error)
}

// AgentConditions writes a condition onto an agent's status without touching its phase. A missing
// status row is not an error: the agent controller creates it and the next pass writes the condition.
type AgentConditions interface {
	SetCondition(ctx context.Context, agentID string, c domain.Condition) error
}

// Config configures the Controller.
type Config struct {
	Source  Source
	Cluster Cluster
	// Agents receives each agent's EgressPolicyReady; optional.
	Agents AgentConditions
}

// Controller reconciles Kind.
type Controller struct{ cfg Config }

// NewController validates cfg.
func NewController(cfg Config) (*Controller, error) {
	if cfg.Source == nil || cfg.Cluster == nil {
		return nil, errors.New("egress: Source and Cluster are required")
	}
	return &Controller{cfg: cfg}, nil
}

// Kind implements engine.Controller.
func (c *Controller) Kind() engine.Kind { return Kind }

// Routes are the change routes of the egress. The agents table is routed here too, but another
// controller owns it: the wiring merges these targets into that table's route (see AgentsTarget).
func Routes() []engine.Route {
	return []engine.Route{
		{Table: "external_systems", Targets: []engine.Target{{Kind: Kind}}, FanOut: []engine.Kind{Kind}},
		{Table: "external_system_optouts", Targets: []engine.Target{{Kind: Kind}}},
	}
}

// AgentsTarget is the target to add to the route of table agents: a new, paused or deleted agent
// changes its team's egress.
func AgentsTarget() engine.Target { return engine.Target{Kind: Kind} }

// Reconcile implements engine.Controller.
func (c *Controller) Reconcile(ctx context.Context, ref engine.Ref) (engine.Outcome, error) {
	d, err := c.cfg.Source.LoadEgress(ctx, ref.ID)
	if err != nil {
		if errors.Is(err, &domain.Error{Kind: domain.KindNotFound}) {
			return engine.Outcome{NoStatus: true}, nil
		}
		return engine.Outcome{}, err
	}
	if d.State == "deleted" || d.State == "deleting" {
		// The namespace's teardown removes the policies with everything else.
		return engine.Outcome{NoStatus: true}, nil
	}

	var (
		changed []string
		keep    []string
		failed  []error
		refused []error
	)
	for _, a := range d.Agents {
		if a.State == "deleting" || a.State == "deleted" {
			continue
		}
		keep = append(keep, a.ID)
		pol, perr := PolicyFor(d.TeamID, a)
		if perr != nil {
			refused = append(refused, fmt.Errorf("agent %s: %w", a.ID, perr))
			c.condition(ctx, a.ID, domain.Condition{Type: domain.CondEgressPolicyReady, Status: domain.ConditionFalse, Reason: "InvalidEndpoint", Message: perr.Error()})
			continue
		}
		res, aerr := c.cfg.Cluster.ApplyAgentPolicy(ctx, d.Namespace, pol)
		changed = append(changed, res.Changed...)
		if aerr != nil {
			failed = append(failed, fmt.Errorf("agent %s: %w", a.ID, aerr))
			c.condition(ctx, a.ID, domain.Condition{Type: domain.CondEgressPolicyReady, Status: domain.ConditionFalse, Reason: "ApplyFailed", Message: "the egress policy could not be applied"})
			continue
		}
		c.condition(ctx, a.ID, domain.Condition{Type: domain.CondEgressPolicyReady, Status: domain.ConditionTrue, Reason: "PolicyApplied",
			Message: allowedSummary(pol)})
	}
	sort.Strings(keep)
	if res, perr := c.cfg.Cluster.PruneAgentPolicies(ctx, d.Namespace, d.TeamID, keep); perr != nil {
		failed = append(failed, fmt.Errorf("prune: %w", perr))
	} else {
		changed = append(changed, res.Changed...)
	}

	out := engine.Outcome{Changed: changed}
	switch {
	case len(failed) > 0:
		return out, engine.WithCondition(domain.CondEgressPolicyReady, errors.Join(append(failed, refused...)...))
	case len(refused) > 0:
		out.Conditions = []domain.Condition{{Type: domain.CondEgressPolicyReady, Status: domain.ConditionFalse, Reason: "InvalidEndpoint", Message: errors.Join(refused...).Error()}}
		return out, engine.Terminal(engine.WithCondition(domain.CondEgressPolicyReady, errors.Join(refused...)))
	}
	out.Conditions = []domain.Condition{{Type: domain.CondEgressPolicyReady, Status: domain.ConditionTrue, Reason: "PolicyApplied"}}
	return out, nil
}

func (c *Controller) condition(ctx context.Context, agentID string, cond domain.Condition) {
	if c.cfg.Agents == nil {
		return
	}
	// A status failure must not fail the reconcile: the next pass rewrites it.
	_ = c.cfg.Agents.SetCondition(ctx, agentID, cond)
}

func allowedSummary(p AgentPolicy) string {
	if len(p.Rules) == 0 {
		return "no external system is on for this agent"
	}
	slugs := make([]string, len(p.Rules))
	for i, r := range p.Rules {
		slugs[i] = r.Slug
	}
	return "allowed: " + strings.Join(slugs, ", ")
}

// PolicyFor builds the policy of one agent from its systems: systems that are off contribute nothing.
// It returns an error naming an endpoint that cannot be rendered; the rules before it are not applied.
func PolicyFor(teamID string, a Agent) (AgentPolicy, error) {
	p := AgentPolicy{TeamID: teamID, AgentID: a.ID}
	systems := append([]System(nil), a.Systems...)
	sort.Slice(systems, func(i, j int) bool { return systems[i].Slug < systems[j].Slug })
	var errs []error
	for _, s := range systems {
		if s.DisabledAt != "" {
			continue
		}
		r := Rule{SystemID: s.ID, Slug: s.Slug}
		for _, raw := range s.Endpoints {
			e, err := ParseEndpoint(raw)
			if err != nil {
				errs = append(errs, fmt.Errorf("system %s: %w", s.Slug, err))
				continue
			}
			r.Endpoints = append(r.Endpoints, e)
		}
		if len(r.Endpoints) > 0 {
			p.Rules = append(p.Rules, r)
		}
	}
	return p, errors.Join(errs...)
}

// ParseEndpoint parses 'host', 'host:port' or '*.domain[:port]'. Hosts are lower case; the port
// defaults to DefaultPort. A host with a path, a scheme, an address or another wildcard is refused,
// because the policy matches names, and a name is all that can be matched.
func ParseEndpoint(s string) (Endpoint, error) {
	host, port := s, DefaultPort
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		host = s[:i]
		n, err := strconv.Atoi(s[i+1:])
		if err != nil || n < 1 || n > 65535 {
			return Endpoint{}, fmt.Errorf("endpoint %q: invalid port", s)
		}
		port = n
	}
	body := strings.TrimPrefix(host, "*.")
	if body == "" || body != strings.ToLower(body) || strings.ContainsAny(body, "*/ @?#") || !strings.Contains(body, ".") {
		return Endpoint{}, fmt.Errorf("endpoint %q: not a host name or *.domain", s)
	}
	for _, label := range strings.Split(body, ".") {
		if label == "" || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return Endpoint{}, fmt.Errorf("endpoint %q: not a host name or *.domain", s)
		}
	}
	return Endpoint{Host: host, Port: port}, nil
}

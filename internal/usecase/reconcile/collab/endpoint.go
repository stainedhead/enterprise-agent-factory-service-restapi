package collab

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
)

// KindTeamCollaboration is the resource_kind of a team's collaboration status (RC-24).
const KindTeamCollaboration engine.Kind = "team_collaboration"

// ConditionEndpointReady and ConditionPaperclipReady are the condition types of a team's collaboration status.
const (
	ConditionEndpointReady  = "EndpointReady"
	ConditionPaperclipReady = "PaperclipReady"
	ConditionUnmanaged      = "UnmanagedPaperclipAgents"
)

// Reasons of collaboration.endpoint_state_changed (API contract 7).
const (
	ReasonReady           = "Ready"
	ReasonStarting        = "Starting"
	ReasonPaused          = "Paused"
	ReasonTeamSuspended   = "TeamSuspended"
	ReasonServiceOff      = "ServiceOff"
	ReasonGuardrailOff    = "GuardrailOff"
	ReasonBaseDomainUnset = "BaseDomainUnset"
	ReasonRouteNotPublish = "RouteNotPublished"
)

// TeamState is a team's collaboration as the reconciler reads it.
type TeamState struct {
	TeamID, AgentID, TeamSlug string
	TeamState, AgentState     string
	// PodReady is true when the collaboration agent's pod is ready and the Service `paperclip` has ready
	// endpoints (the agent controller's observation).
	PodReady         bool
	BaseDomain       string
	PublicEndpoints  bool
	MayExpose        bool
	WebUIEnabled     bool
	RESTAPIEnabled   bool
	ProviderClient   bool
	Mode             domain.RegistrationMode
	Generation       int64
	PrevReady        *bool  // nil when no status row exists yet
	PrevReason       string // the last EndpointReady reason
	PrevPaperclip    *bool
	PaperclipHealthy *bool // nil when not probed
	Unmanaged        []string
}

// Hostname is the team's collaboration hostname, empty while the base domain is unset.
func (s TeamState) Hostname() string {
	if s.BaseDomain == "" {
		return ""
	}
	return s.TeamSlug + "." + s.BaseDomain
}

// Endpoint is the computed EndpointReady condition.
type Endpoint struct {
	Ready  bool
	Reason string
	WebUI  bool
	REST   bool
}

// ComputeEndpoint decides EndpointReady and its reason (RC-24). The order is the order in which a person
// fixes things: the team, the agent, the pod, then the service-level switches and the team's choices.
func ComputeEndpoint(s TeamState) Endpoint {
	shared := s.MayExpose && s.PublicEndpoints && s.BaseDomain != ""
	e := Endpoint{WebUI: s.WebUIEnabled && shared && s.ProviderClient, REST: s.RESTAPIEnabled && shared}
	switch {
	case s.TeamState == "suspended":
		e.Reason = ReasonTeamSuspended
	case s.AgentState == string(domain.AgentPaused):
		e.Reason = ReasonPaused
	case !s.PodReady:
		e.Reason = ReasonStarting
	case !s.PublicEndpoints:
		e.Reason = ReasonServiceOff
	case !s.MayExpose:
		e.Reason = ReasonGuardrailOff
	case s.BaseDomain == "":
		e.Reason = ReasonBaseDomainUnset
	case !e.WebUI && !e.REST:
		e.Reason = ReasonRouteNotPublish
	default:
		e.Reason, e.Ready = ReasonReady, true
	}
	return e
}

// Source reads the collaboration states and the links.
type Source interface {
	// States returns one TeamState per live team that has a collaboration agent.
	States(ctx context.Context) ([]TeamState, error)
}

// Events puts an event on the outbox.
type Events interface {
	Publish(ctx context.Context, typ domain.EventType, teamID, kind, id, agentID string, data map[string]any) error
}

// Controller maintains EndpointReady and PaperclipReady for every team with a collaboration agent.
type Controller struct {
	Source Source
	Status engine.StatusWriter
	Events Events
	Now    func() time.Time
	Log    *slog.Logger
}

// Sync runs once: for each team it writes the status and, when EndpointReady changed, the event.
func (c *Controller) Sync(ctx context.Context) error {
	states, err := c.Source.States(ctx)
	if err != nil {
		return fmt.Errorf("collab: read states: %w", err)
	}
	var failed int
	for _, s := range states {
		if err := c.syncOne(ctx, s); err != nil {
			failed++
			c.log().Warn("collaboration status failed", "team", s.TeamID, "err", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("collab: %d of %d teams failed", failed, len(states))
	}
	return nil
}

func (c *Controller) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

func (c *Controller) syncOne(ctx context.Context, s TeamState) error {
	now := c.Now()
	ep := ComputeEndpoint(s)
	conds := []domain.Condition{condition(ConditionEndpointReady, ep.Ready, ep.Reason, hostMessage(s, ep), now)}
	if s.PaperclipHealthy != nil {
		conds = append(conds, condition(ConditionPaperclipReady, *s.PaperclipHealthy, boolReason(*s.PaperclipHealthy, "Healthy", "NotAnswering"), "", now))
	}
	phase := domain.PhaseReady
	if !ep.Ready {
		phase = domain.PhasePending
	}
	gen := s.Generation
	if err := c.Status.Apply(ctx, engine.Report{Ref: engine.NewRef(KindTeamCollaboration, s.TeamID, s.TeamID), Generation: &gen,
		Phase: phase, Conditions: conds, ReconciledAt: now}); err != nil {
		return err
	}
	if s.PrevReady != nil && *s.PrevReady == ep.Ready && s.PrevReason == ep.Reason {
		return nil
	}
	if s.PrevReady == nil && !ep.Ready && ep.Reason == ReasonStarting {
		return nil // a new endpoint that is starting has not changed state
	}
	prev := s.PrevReady != nil && *s.PrevReady
	return c.Events.Publish(ctx, domain.EventCollaborationEndpointStateChanged, s.TeamID, string(KindTeamCollaboration), s.TeamID, s.AgentID,
		map[string]any{"teamId": s.TeamID, "agentId": s.AgentID, "hostname": s.Hostname(), "ready": ep.Ready, "previousReady": prev,
			"reason": ep.Reason, "effective": map[string]any{"webUi": ep.WebUI, "restApi": ep.REST}})
}

func hostMessage(s TeamState, ep Endpoint) string {
	if ep.Ready && s.Hostname() != "" {
		return "serving " + s.Hostname()
	}
	return ""
}

func boolReason(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

func condition(typ string, ok bool, reason, msg string, at time.Time) domain.Condition {
	st := domain.ConditionFalse
	if ok {
		st = domain.ConditionTrue
	}
	return domain.Condition{Type: typ, Status: st, Reason: reason, Message: msg, LastTransitionTime: at.UTC()}
}

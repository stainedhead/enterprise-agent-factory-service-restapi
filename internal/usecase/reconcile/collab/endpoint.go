package collab

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
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

// TeamState is a team's orchestration as the reconciler reads it.
type TeamState struct {
	TeamID, AgentID, TeamSlug string
	TeamState, AgentState     string
	// Tool is the collaboration tool the orchestration agent runs.
	Tool domain.CollabTool
	// PodReady is true when the orchestration agent's pod is ready and its Service has ready endpoints
	// (the agent controller's observation).
	PodReady bool
	// Settings are the orchestration.* switches and base domains.
	Settings domain.OrchestrationSettings
	// WebUIExposure and WebAPIExposure are the team's stored choices; Limit is maxCollaborationExposure.
	WebUIExposure  domain.Exposure
	WebAPIExposure domain.Exposure
	Limit          domain.Exposure
	ProviderClient bool
	Mode           domain.RegistrationMode
	Generation     int64
	// ToolChanging is true while a tool change is in progress (the pod is being replaced).
	ToolChanging     bool
	PrevReady        *bool  // nil when no status row exists yet
	PrevReason       string // the last EndpointReady reason
	PrevPaperclip    *bool
	PaperclipHealthy *bool // nil when not probed
	// PrevSurfaces is the last reported effective level and readiness of each surface (webUi, webApi).
	PrevSurfaces map[domain.Surface]PrevSurface
	Unmanaged    []string
}

// PrevSurface is a surface's last reported state.
type PrevSurface struct {
	Effective domain.Exposure
	Ready     bool
}

// Surfaces computes the two surfaces of the team under its settings.
func (s TeamState) Surfaces() (webUI, webAPI domain.SurfaceState) {
	return domain.ComputeSurface(s.Tool, domain.SurfaceWebUI, s.WebUIExposure, s.Limit, s.ProviderClient, s.TeamSlug, s.Settings),
		domain.ComputeSurface(s.Tool, domain.SurfaceWebAPI, s.WebAPIExposure, s.Limit, s.ProviderClient, s.TeamSlug, s.Settings)
}

// Hostname is the hostname the deprecated endpoint event reports: that of the Web UI's effective level,
// else the Web API's, else the public one while the public base domain is set, else empty.
func (s TeamState) Hostname() string {
	ui, api := s.Surfaces()
	switch {
	case ui.Hostname != "":
		return ui.Hostname
	case api.Hostname != "":
		return api.Hostname
	case s.Settings.PublicBaseDomain != "":
		return s.TeamSlug + "." + s.Settings.PublicBaseDomain
	}
	return ""
}

// Endpoint is the computed EndpointReady condition.
type Endpoint struct {
	Ready  bool
	Reason string
	WebUI  bool
	REST   bool
}

// ComputeEndpoint decides EndpointReady and its reason (RC-24). The order is the order in which a person
// fixes things: the team, the agent, the pod, then the service-level switches and the team's choices. A
// tool without a web surface (Buzz) is ready when its pod is; it has no route to publish.
func ComputeEndpoint(s TeamState) Endpoint {
	ui, api := s.Surfaces()
	e := Endpoint{WebUI: ui.Effective != domain.ExposureOff, REST: api.Effective != domain.ExposureOff}
	has := func(r string) bool { return slices.Contains(ui.Reasons, r) || slices.Contains(api.Reasons, r) }
	noSurface := !s.Tool.Supports(domain.SurfaceWebUI) && !s.Tool.Supports(domain.SurfaceWebAPI)
	switch {
	case s.TeamState == "suspended":
		e.Reason = ReasonTeamSuspended
	case s.AgentState == string(domain.AgentPaused):
		e.Reason = ReasonPaused
	case !s.PodReady || s.ToolChanging:
		e.Reason = ReasonStarting
	case noSurface:
		e.Reason, e.Ready = ReasonReady, true
	case e.WebUI || e.REST:
		e.Reason, e.Ready = ReasonReady, true
	case has(domain.ReasonServiceSwitchPublic) || has(domain.ReasonServiceSwitchPrivate):
		e.Reason = ReasonServiceOff
	case has(domain.ReasonTeamLimit) || s.Limit == domain.ExposureOff:
		e.Reason = ReasonGuardrailOff
	case has(domain.ReasonBaseDomainUnsetNew):
		e.Reason = ReasonBaseDomainUnset
	default:
		e.Reason = ReasonRouteNotPublish
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
	uiState, apiState := s.Surfaces()
	for _, sf := range surfaceStates(s, ep, uiState, apiState) {
		conds = append(conds, condition(sf.cond, sf.ready, string(sf.state.Effective), sf.state.Hostname, now))
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
	if err := c.surfaceEvents(ctx, s, ep, uiState, apiState); err != nil {
		return err
	}
	if s.PrevReady != nil && *s.PrevReady == ep.Ready && s.PrevReason == ep.Reason {
		return nil
	}
	if s.PrevReady == nil && !ep.Ready && ep.Reason == ReasonStarting {
		return nil // a new endpoint that is starting has not changed state
	}
	prev := s.PrevReady != nil && *s.PrevReady
	// The deprecated event is still emitted until 2.0.0.
	return c.Events.Publish(ctx, domain.EventCollaborationEndpointStateChanged, s.TeamID, string(KindTeamCollaboration), s.TeamID, s.AgentID,
		map[string]any{"teamId": s.TeamID, "agentId": s.AgentID, "hostname": s.Hostname(), "ready": ep.Ready, "previousReady": prev,
			"reason": ep.Reason, "effective": map[string]any{"webUi": ep.WebUI, "restApi": ep.REST}})
}

// Condition types that carry each surface's last reported effective level (the reason) and readiness.
const (
	ConditionWebUIEndpoint  = "WebUiEndpoint"
	ConditionWebAPIEndpoint = "WebApiEndpoint"
)

type surfaceReport struct {
	surface domain.Surface
	cond    string
	state   domain.SurfaceState
	ready   bool
}

func surfaceStates(s TeamState, ep Endpoint, ui, api domain.SurfaceState) []surfaceReport {
	ready := func(st domain.SurfaceState) bool { return ep.Ready && st.Effective != domain.ExposureOff }
	return []surfaceReport{
		{domain.SurfaceWebUI, ConditionWebUIEndpoint, ui, ready(ui)},
		{domain.SurfaceWebAPI, ConditionWebAPIEndpoint, api, ready(api)},
	}
}

// surfaceEvents publishes orchestration.endpoint_state_changed for each surface whose effective level or
// readiness changed since the last report. A surface the tool lacks that was never reported stays quiet.
func (c *Controller) surfaceEvents(ctx context.Context, s TeamState, ep Endpoint, ui, api domain.SurfaceState) error {
	for _, sf := range surfaceStates(s, ep, ui, api) {
		prev, had := s.PrevSurfaces[sf.surface]
		if had && prev.Effective == sf.state.Effective && prev.Ready == sf.ready {
			continue
		}
		if !had && (sf.state.Effective == domain.ExposureOff || ep.Reason == ReasonStarting) {
			continue // never reported and nothing to announce: off, or a new endpoint that is still starting
		}
		prevEff := domain.ExposureOff
		if had {
			prevEff = prev.Effective
		}
		reasons := sf.state.Reasons
		if reasons == nil {
			reasons = []string{}
		}
		if err := c.Events.Publish(ctx, domain.EventOrchestrationEndpointStateChanged, s.TeamID, string(KindTeamCollaboration), s.TeamID, s.AgentID,
			map[string]any{"teamId": s.TeamID, "agentId": s.AgentID, "surface": string(sf.surface), "exposure": string(sf.state.Exposure),
				"effective": string(sf.state.Effective), "previousEffective": string(prevEff), "reasons": reasons,
				"hostname": sf.state.Hostname, "ready": sf.ready}); err != nil {
			return err
		}
	}
	return nil
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

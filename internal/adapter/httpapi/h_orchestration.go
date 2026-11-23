package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/collab"
)

// surfaceBody is one surface of TeamOrchestration (API contract §4.4d).
type surfaceBody struct {
	Supported bool     `json:"supported"`
	Exposure  string   `json:"exposure"`
	Effective string   `json:"effective"`
	Reasons   []string `json:"reasons"`
	Hostname  *string  `json:"hostname"`
	URL       *string  `json:"url"`
}

func toSurfaceBody(st domain.SurfaceState, surface domain.Surface) surfaceBody {
	b := surfaceBody{Supported: st.Supported, Exposure: string(st.Exposure), Effective: string(st.Effective), Reasons: st.Reasons}
	if b.Reasons == nil {
		b.Reasons = []string{}
	}
	if st.Hostname != "" {
		h, u := st.Hostname, st.URL(surface)
		b.Hostname, b.URL = &h, &u
	}
	return b
}

// teamOrchestrationBody is the wire shape of TeamOrchestration.
type teamOrchestrationBody struct {
	TeamID              string      `json:"teamId"`
	AgentID             string      `json:"agentId"`
	AgentName           string      `json:"agentName"`
	Phase               string      `json:"phase"`
	Tool                string      `json:"tool"`
	ToolVersion         *string     `json:"toolVersion"`
	RegistrationMode    string      `json:"registrationMode"`
	Limit               string      `json:"limit"`
	WebUI               surfaceBody `json:"webUi"`
	WebAPI              surfaceBody `json:"webApi"`
	ConnectedAgents     int         `json:"connectedAgents"`
	UnmanagedToolAgents []string    `json:"unmanagedToolAgents"`
	Version             int64       `json:"version"`
	Generation          int64       `json:"generation"`
	Status              statusBody  `json:"status"`
	CreatedAt           string      `json:"createdAt"`
	CreatedBy           string      `json:"createdBy"`
	UpdatedAt           string      `json:"updatedAt"`
}

// toTeamOrchestrationBody renders the resource. pending reports a write that the reconciler has not
// observed yet as Pending.
func toTeamOrchestrationBody(v collab.View, pending bool) teamOrchestrationBody {
	c := v.Collab
	b := teamOrchestrationBody{TeamID: c.TeamID, AgentID: c.AgentID, AgentName: c.AgentName, Phase: string(c.AgentPhase), Tool: string(c.Tool),
		RegistrationMode: string(c.Mode), Limit: string(c.Limit), WebUI: toSurfaceBody(v.WebUI, domain.SurfaceWebUI),
		WebAPI: toSurfaceBody(v.WebAPI, domain.SurfaceWebAPI), ConnectedAgents: c.Connected, UnmanagedToolAgents: c.Unmanaged,
		Version: int64(c.Version), Generation: int64(c.Generation), CreatedAt: w2Time(c.CreatedAt), CreatedBy: c.CreatedBy, UpdatedAt: w2Time(c.UpdatedAt)}
	if b.Phase == "" {
		b.Phase = string(domain.PhasePending)
	}
	if b.UnmanagedToolAgents == nil {
		b.UnmanagedToolAgents = []string{}
	}
	if c.ToolVersion != "" {
		tv := c.ToolVersion
		b.ToolVersion = &tv
	}
	st := c.Status
	if pending && st.ObservedGeneration < int64(c.Generation) {
		st.Phase = domain.PhasePending
	}
	b.Status = toStatusBody(st)
	return b
}

func (a *CollaborationAPI) getOrchestration(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.Svc.Get(r.Context(), c, r.PathValue("teamId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, v.Collab.Version)
	WriteJSON(w, http.StatusOK, toTeamOrchestrationBody(v, false))
}

type surfaceRequest struct {
	Exposure *string `json:"exposure"`
}

type putOrchestrationRequest struct {
	WebUI            *surfaceRequest `json:"webUi"`
	WebAPI           *surfaceRequest `json:"webApi"`
	RegistrationMode *string         `json:"registrationMode"`
}

func (a *CollaborationAPI) putOrchestration(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatchOptional(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req putOrchestrationRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	var fe []domain.FieldError
	if req.WebUI == nil || req.WebUI.Exposure == nil {
		fe = append(fe, domain.FieldError{Field: "webUi.exposure", Reason: "is required"})
	}
	if req.WebAPI == nil || req.WebAPI.Exposure == nil {
		fe = append(fe, domain.FieldError{Field: "webApi.exposure", Reason: "is required"})
	}
	if len(fe) > 0 {
		WriteError(w, r, domain.Validation(fe...))
		return
	}
	p := domain.PutOrchestrationParams{WebUI: domain.Exposure(*req.WebUI.Exposure), WebAPI: domain.Exposure(*req.WebAPI.Exposure)}
	if req.RegistrationMode != nil {
		p.Mode = domain.RegistrationMode(*req.RegistrationMode)
	}
	v, err := a.Svc.Put(r.Context(), c, r.PathValue("teamId"), p, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, v.Collab.Version)
	WriteJSON(w, http.StatusOK, toTeamOrchestrationBody(v, true))
}

type changeToolRequest struct {
	Tool string `json:"tool"`
}

func (a *CollaborationAPI) changeTool(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := r.PathValue("teamId")
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req changeToolRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	tool := domain.CollabTool(req.Tool)
	if !tool.Valid() {
		WriteError(w, r, bad("tool", "must be paperclip or buzz"))
		return
	}
	idem, err := w2Idem(r, "ChangeCollaborationTool", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusAccepted, toTeamOrchestrationBody(v.(collab.View), true))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.ChangeTool(r.Context(), c, teamID, tool, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.View.Collab.Version)
	WriteJSON(w, http.StatusAccepted, toTeamOrchestrationBody(res.View, true))
}

// orchestrationEndpointBody is one surface of a team in ListOrchestrationEndpoints.
type orchestrationEndpointBody struct {
	TeamID       string   `json:"teamId"`
	Slug         string   `json:"slug"`
	TeamState    string   `json:"teamState"`
	AgentID      string   `json:"agentId"`
	Tool         string   `json:"tool"`
	Surface      string   `json:"surface"`
	Exposure     string   `json:"exposure"`
	Effective    string   `json:"effective"`
	Reasons      []string `json:"reasons"`
	Limit        string   `json:"limit"`
	Hostname     *string  `json:"hostname"`
	Phase        string   `json:"phase"`
	LastAccessAt *string  `json:"lastAccessAt"`
}

func toOrchestrationEndpointBody(e collab.Endpoint) orchestrationEndpointBody {
	c := e.Row.Collab
	b := orchestrationEndpointBody{TeamID: c.TeamID, Slug: c.TeamSlug, TeamState: string(c.TeamState), AgentID: c.AgentID, Tool: string(c.Tool),
		Surface: string(e.Surface), Exposure: string(e.State.Exposure), Effective: string(e.State.Effective), Reasons: e.State.Reasons,
		Limit: string(c.Limit), Phase: string(e.Row.Phase), LastAccessAt: w2TimePtr(e.Row.LastAccessAt)}
	if b.Reasons == nil {
		b.Reasons = []string{}
	}
	if e.State.Hostname != "" {
		h := e.State.Hostname
		b.Hostname = &h
	}
	return b
}

type migrationBlockedBody struct {
	TeamID string `json:"teamId"`
	Reason string `json:"reason"`
}

type migrationBody struct {
	State                  string                 `json:"state"`
	TeamsTotal             int                    `json:"teamsTotal"`
	TeamsWithOrchestration int                    `json:"teamsWithOrchestration"`
	TeamsPending           int                    `json:"teamsPending"`
	Blocked                []migrationBlockedBody `json:"blocked"`
}

type orchestrationEndpointsBody struct {
	Items     []orchestrationEndpointBody `json:"items"`
	NextToken string                      `json:"nextToken,omitempty"`
	Migration migrationBody               `json:"migration"`
}

func (a *CollaborationAPI) orchestrationEndpoints(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	page, err := w2Page(a.Pages, r, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	q := r.URL.Query()
	f := collab.EndpointFilter{TeamID: q.Get("teamId"), State: domain.Phase(q.Get("state")), Exposure: domain.Exposure(q.Get("exposure")),
		Tool: domain.CollabTool(q.Get("tool"))}
	switch f.State {
	case "", domain.PhaseReady, domain.PhaseDegraded, domain.PhaseFailed, domain.PhasePending:
	default:
		WriteError(w, r, bad("state", "must be Ready, Degraded, Failed or Pending"))
		return
	}
	if f.Exposure != "" && f.Exposure != domain.ExposurePrivate && f.Exposure != domain.ExposurePublic {
		WriteError(w, r, bad("exposure", "must be private or public"))
		return
	}
	if f.Tool != "" && !f.Tool.Valid() {
		WriteError(w, r, bad("tool", "must be paperclip or buzz"))
		return
	}
	res, err := a.Svc.Endpoints(r.Context(), c, f, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	token := w2Next(a.Pages, r, c, res.Next)
	if WantsCSV(r) {
		cells := make([][]string, 0, len(res.Items))
		for _, e := range res.Items {
			b := toOrchestrationEndpointBody(e)
			host, last := "", ""
			if b.Hostname != nil {
				host = *b.Hostname
			}
			if b.LastAccessAt != nil {
				last = *b.LastAccessAt
			}
			cells = append(cells, []string{b.TeamID, b.Slug, b.TeamState, b.AgentID, b.Tool, b.Surface, b.Exposure, b.Effective, b.Limit,
				host, b.Phase, last})
		}
		if err := WriteCSV(w, []string{"teamId", "slug", "teamState", "agentId", "tool", "surface", "exposure", "effective", "limit",
			"hostname", "phase", "lastAccessAt"}, cells, token); err != nil {
			WriteError(w, r, domain.Internal(err))
		}
		return
	}
	out := orchestrationEndpointsBody{Items: make([]orchestrationEndpointBody, 0, len(res.Items)), NextToken: token}
	for _, e := range res.Items {
		out.Items = append(out.Items, toOrchestrationEndpointBody(e))
	}
	m := res.Migration
	out.Migration = migrationBody{State: m.State, TeamsTotal: m.TeamsTotal, TeamsWithOrchestration: m.TeamsWithOrchestration,
		TeamsPending: m.TeamsPending, Blocked: make([]migrationBlockedBody, 0, len(m.Blocked))}
	for _, b := range m.Blocked {
		out.Migration.Blocked = append(out.Migration.Blocked, migrationBlockedBody{TeamID: b.TeamID, Reason: b.Reason})
	}
	WriteJSON(w, http.StatusOK, out)
}

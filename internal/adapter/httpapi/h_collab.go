package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/collab"
)

// CollaborationAPI serves the orchestration operations (API contract §4.4d), the deprecated 1.0.1
// collaboration aliases and the collaboration link operations (§4.4b, RA-17).
type CollaborationAPI struct {
	Svc   *collab.Service
	Pages PageTokens
}

// BindCollaboration binds the collaboration handlers as the collaboration area.
func BindCollaboration(a *CollaborationAPI) Registrar { return a.Register }

// Register binds the orchestration and collaboration operations.
func (a *CollaborationAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"GetTeamOrchestration": a.getOrchestration, "PutTeamOrchestration": a.putOrchestration,
		"ChangeCollaborationTool": a.changeTool, "ListOrchestrationEndpoints": a.orchestrationEndpoints,
		"GetTeamCollaboration": a.get, "PutTeamCollaboration": a.put, "ListCollaborationLinks": a.links,
		"ConnectAgentToCollaboration": a.connect, "DisconnectAgentFromCollaboration": a.disconnect,
		"RotateCollaborationKeys": a.rotate, "ListCollaborationEndpoints": a.endpoints,
	})
}

// teamCollaborationBody is the wire shape of the deprecated TeamCollaboration (API contract §4.4b as
// kept by §4.4d).
type teamCollaborationBody struct {
	TeamID                   string        `json:"teamId"`
	AgentID                  string        `json:"agentId"`
	Hostname                 *string       `json:"hostname"`
	WebUIURL                 *string       `json:"webUiUrl"`
	RESTAPIURL               *string       `json:"restApiUrl"`
	WebUIEnabled             bool          `json:"webUiEnabled"`
	RESTAPIEnabled           bool          `json:"restApiEnabled"`
	RegistrationMode         string        `json:"registrationMode"`
	Effective                effectiveBody `json:"effective"`
	UnmanagedPaperclipAgents []string      `json:"unmanagedPaperclipAgents"`
	Version                  int64         `json:"version"`
	Generation               int64         `json:"generation"`
	Status                   statusBody    `json:"status"`
	CreatedAt                string        `json:"createdAt"`
	CreatedBy                string        `json:"createdBy"`
	UpdatedAt                string        `json:"updatedAt"`
}

type effectiveBody struct {
	WebUI   bool     `json:"webUi"`
	RESTAPI bool     `json:"restApi"`
	Reasons []string `json:"reasons"`
}

func toTeamCollaborationBody(v collab.View, pending bool) teamCollaborationBody {
	c := v.Collab
	eff := domain.LegacyOf(v.WebUI, v.WebAPI)
	b := teamCollaborationBody{TeamID: c.TeamID, AgentID: c.AgentID, WebUIEnabled: c.WebUIExposure != domain.ExposureOff,
		RESTAPIEnabled: c.WebAPIExposure != domain.ExposureOff, RegistrationMode: string(c.Mode),
		Effective:                effectiveBody{WebUI: eff.WebUI, RESTAPI: eff.RESTAPI, Reasons: eff.Reasons},
		UnmanagedPaperclipAgents: c.Unmanaged,
		Version:                  int64(c.Version), Generation: int64(c.Generation), CreatedAt: w2Time(c.CreatedAt), CreatedBy: c.CreatedBy, UpdatedAt: w2Time(c.UpdatedAt)}
	if b.UnmanagedPaperclipAgents == nil {
		b.UnmanagedPaperclipAgents = []string{}
	}
	st := c.Status
	if pending && st.ObservedGeneration < int64(c.Generation) {
		st.Phase = domain.PhasePending
	}
	b.Status = toStatusBody(st)
	if host := c.LegacyHostname(v.Settings); host != "" {
		web, rest := "https://"+host+"/", "https://"+host+"/api"
		b.Hostname, b.WebUIURL, b.RESTAPIURL = &host, &web, &rest
	}
	return b
}

// collaborationLinkBody is the wire shape of CollaborationLink.
type collaborationLinkBody struct {
	AgentID          string  `json:"agentId"`
	AgentName        string  `json:"agentName"`
	Connect          bool    `json:"connect"`
	PaperclipAgentID *string `json:"paperclipAgentId"`
	LinkState        string  `json:"linkState"`
	LastSyncedAt     *string `json:"lastSyncedAt"`
	LastError        *string `json:"lastError"`
	Version          int64   `json:"version"`
}

func toCollaborationLinkBody(l domain.CollaborationLink) collaborationLinkBody {
	b := collaborationLinkBody{AgentID: l.AgentID, AgentName: l.AgentName, Connect: l.Connect, LinkState: string(l.LinkState),
		LastSyncedAt: w2TimePtr(l.LastSyncedAt), Version: int64(l.Version)}
	if l.PaperclipAgentID != "" {
		p := l.PaperclipAgentID
		b.PaperclipAgentID = &p
	}
	if l.LastError != "" {
		e := l.LastError
		b.LastError = &e
	}
	return b
}

type collaborationEndpointBody struct {
	TeamID         string  `json:"teamId"`
	Slug           string  `json:"slug"`
	TeamState      string  `json:"teamState"`
	Hostname       *string `json:"hostname"`
	AgentID        string  `json:"agentId"`
	WebUIEnabled   bool    `json:"webUiEnabled"`
	RESTAPIEnabled bool    `json:"restApiEnabled"`
	Effective      struct {
		WebUI   bool `json:"webUi"`
		RESTAPI bool `json:"restApi"`
	} `json:"effective"`
	MayExposeCollaboration bool    `json:"mayExposeCollaboration"`
	Phase                  string  `json:"phase"`
	LastAccessAt           *string `json:"lastAccessAt"`
}

// toCollaborationEndpointBody maps one team's two surfaces to the deprecated per-team row: booleans for
// the stored choice, and effective true only for public (the 1.0.1 meaning).
func toCollaborationEndpointBody(c domain.TeamCollaboration, phase domain.Phase, last *time.Time, ui, api domain.SurfaceState) collaborationEndpointBody {
	b := collaborationEndpointBody{TeamID: c.TeamID, Slug: c.TeamSlug, TeamState: string(c.TeamState), AgentID: c.AgentID,
		WebUIEnabled: c.WebUIExposure != domain.ExposureOff, RESTAPIEnabled: c.WebAPIExposure != domain.ExposureOff,
		MayExposeCollaboration: c.Limit != domain.ExposureOff, Phase: string(phase), LastAccessAt: w2TimePtr(last)}
	b.Effective.WebUI, b.Effective.RESTAPI = ui.Effective == domain.ExposurePublic, api.Effective == domain.ExposurePublic
	switch {
	case ui.Effective == domain.ExposurePublic && ui.Hostname != "":
		h := ui.Hostname
		b.Hostname = &h
	case api.Effective == domain.ExposurePublic && api.Hostname != "":
		h := api.Hostname
		b.Hostname = &h
	}
	return b
}

func (a *CollaborationAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := r.PathValue("teamId")
	DefaultDeprecation.Apply(w, "/v1/teams/"+teamID+"/orchestration")
	v, err := a.Svc.Get(r.Context(), c, teamID)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, v.Collab.Version)
	WriteJSON(w, http.StatusOK, toTeamCollaborationBody(v, false))
}

type putCollaborationRequest struct {
	WebUIEnabled     *bool   `json:"webUiEnabled"`
	RESTAPIEnabled   *bool   `json:"restApiEnabled"`
	RegistrationMode *string `json:"registrationMode"`
}

// put is the deprecated PutTeamCollaboration: the booleans become levels (§4.4d).
func (a *CollaborationAPI) put(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := r.PathValue("teamId")
	DefaultDeprecation.Apply(w, "/v1/teams/"+teamID+"/orchestration")
	expect, err := w2IfMatchOptional(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req putCollaborationRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	var fe []domain.FieldError
	if req.WebUIEnabled == nil {
		fe = append(fe, domain.FieldError{Field: "webUiEnabled", Reason: "is required"})
	}
	if req.RESTAPIEnabled == nil {
		fe = append(fe, domain.FieldError{Field: "restApiEnabled", Reason: "is required"})
	}
	if len(fe) > 0 {
		WriteError(w, r, domain.Validation(fe...))
		return
	}
	var mode domain.RegistrationMode
	if req.RegistrationMode != nil {
		mode = domain.RegistrationMode(*req.RegistrationMode)
	}
	v, err := a.Svc.LegacyPut(r.Context(), c, teamID, *req.WebUIEnabled, *req.RESTAPIEnabled, mode, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, v.Collab.Version)
	WriteJSON(w, http.StatusOK, toTeamCollaborationBody(v, true))
}

func (a *CollaborationAPI) links(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := collab.LinkFilter{State: domain.LinkState(q.Get("linkState"))}
	if f.State != "" && !f.State.Valid() {
		WriteError(w, r, bad("linkState", "must be pending, linked, failed or disabled"))
		return
	}
	if s := q.Get("connect"); s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			WriteError(w, r, bad("connect", "must be true or false"))
			return
		}
		f.Connect = &b
	}
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.CollaborationLink, string, error) {
		return a.Svc.Links(r.Context(), c, r.PathValue("teamId"), f, p)
	}, toCollaborationLinkBody)
}

type connectRequest struct {
	PaperclipAgentID string `json:"paperclipAgentId"`
}

func (a *CollaborationAPI) connect(w http.ResponseWriter, r *http.Request) { a.setConnect(w, r, true) }

func (a *CollaborationAPI) disconnect(w http.ResponseWriter, r *http.Request) {
	a.setConnect(w, r, false)
}

func (a *CollaborationAPI) setConnect(w http.ResponseWriter, r *http.Request, connect bool) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	teamID, agentID := r.PathValue("teamId"), r.PathValue("agentId")
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req connectRequest
	if len(raw) > 0 {
		if err := Decode(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	op := "DisconnectAgentFromCollaboration"
	if connect {
		op = "ConnectAgentToCollaboration"
	}
	idem, err := w2Idem(r, op, teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusOK, toCollaborationLinkBody(v.(domain.CollaborationLink)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var res collab.LinkResult
	if connect {
		res, err = a.Svc.Connect(r.Context(), c, teamID, agentID, req.PaperclipAgentID, idem)
	} else {
		res, err = a.Svc.Disconnect(r.Context(), c, teamID, agentID, idem)
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	WriteJSON(w, http.StatusOK, toCollaborationLinkBody(res.Link))
}

type rotateRequest struct {
	Names []string `json:"names"`
}

type rotatedBody struct {
	SecretID  string `json:"secretId"`
	Name      string `json:"name"`
	VersionID string `json:"versionId"`
}

type rotateResultBody struct {
	Rotated             []rotatedBody `json:"rotated"`
	SignsOutEveryone    bool          `json:"signsOutEveryone"`
	AgentRolloutPending bool          `json:"agentRolloutPending"`
}

func toRotateBody(r collab.RotateResult) rotateResultBody {
	b := rotateResultBody{Rotated: make([]rotatedBody, 0, len(r.Rotated)), SignsOutEveryone: true, AgentRolloutPending: true}
	for _, x := range r.Rotated {
		b.Rotated = append(b.Rotated, rotatedBody{SecretID: x.SecretID, Name: x.Name, VersionID: x.VersionID})
	}
	return b
}

func (a *CollaborationAPI) rotate(w http.ResponseWriter, r *http.Request) {
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
	var req rotateRequest
	if len(raw) > 0 {
		if err := Decode(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	idem, err := w2Idem(r, "RotateCollaborationKeys", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusOK, toRotateBody(v.(collab.RotateResult)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Rotate(r.Context(), c, teamID, req.Names, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	WriteJSON(w, http.StatusOK, toRotateBody(res))
}

// endpoints is the deprecated ListCollaborationEndpoints: one row per team with a surface not off.
func (a *CollaborationAPI) endpoints(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	DefaultDeprecation.Apply(w, "/v1/admin/orchestration-endpoints")
	page, err := w2Page(a.Pages, r, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	q := r.URL.Query()
	f := collab.EndpointFilter{TeamID: q.Get("teamId"), State: domain.Phase(q.Get("state"))}
	switch f.State {
	case "", domain.PhaseReady, domain.PhaseDegraded, domain.PhaseFailed, domain.PhasePending:
	default:
		WriteError(w, r, bad("state", "must be Ready, Degraded, Failed or Pending"))
		return
	}
	res, err := a.Svc.Endpoints(r.Context(), c, f, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The service returns one item per surface; fold the two surfaces of a team into one legacy row.
	var rows []collaborationEndpointBody
	for i := 0; i < len(res.Items); i++ {
		it := res.Items[i]
		ui, api := it.State, domain.SurfaceState{Effective: domain.ExposureOff}
		if it.Surface == domain.SurfaceWebAPI {
			ui, api = domain.SurfaceState{Effective: domain.ExposureOff}, it.State
		}
		if i+1 < len(res.Items) && res.Items[i+1].Row.Collab.TeamID == it.Row.Collab.TeamID {
			if res.Items[i+1].Surface == domain.SurfaceWebAPI {
				api = res.Items[i+1].State
			} else {
				ui = res.Items[i+1].State
			}
			i++
		}
		rows = append(rows, toCollaborationEndpointBody(it.Row.Collab, it.Row.Phase, it.Row.LastAccessAt, ui, api))
	}
	token := w2Next(a.Pages, r, c, res.Next)
	if WantsCSV(r) {
		cells := make([][]string, 0, len(rows))
		for _, b := range rows {
			host, last := "", ""
			if b.Hostname != nil {
				host = *b.Hostname
			}
			if b.LastAccessAt != nil {
				last = *b.LastAccessAt
			}
			cells = append(cells, []string{b.TeamID, b.Slug, b.TeamState, host, b.AgentID, strconv.FormatBool(b.WebUIEnabled),
				strconv.FormatBool(b.RESTAPIEnabled), strconv.FormatBool(b.MayExposeCollaboration), b.Phase, last})
		}
		if err := WriteCSV(w, []string{"teamId", "slug", "teamState", "hostname", "agentId", "webUiEnabled", "restApiEnabled",
			"mayExposeCollaboration", "phase", "lastAccessAt"}, cells, token); err != nil {
			WriteError(w, r, domain.Internal(err))
		}
		return
	}
	out := w2ListBody[collaborationEndpointBody]{Items: make([]collaborationEndpointBody, 0, len(rows)), NextToken: token}
	out.Items = append(out.Items, rows...)
	WriteJSON(w, http.StatusOK, out)
}

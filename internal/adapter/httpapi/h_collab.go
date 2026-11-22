package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/collab"
)

// CollaborationAPI serves the seven Paperclip collaboration operations (API contract §4.4b, RA-17).
type CollaborationAPI struct {
	Svc   *collab.Service
	Pages PageTokens
}

// BindCollaboration binds the collaboration handlers as the collaboration area.
func BindCollaboration(a *CollaborationAPI) Registrar { return a.Register }

// Register binds the collaboration operations.
func (a *CollaborationAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"GetTeamCollaboration": a.get, "PutTeamCollaboration": a.put, "ListCollaborationLinks": a.links,
		"ConnectAgentToCollaboration": a.connect, "DisconnectAgentFromCollaboration": a.disconnect,
		"RotateCollaborationKeys": a.rotate, "ListCollaborationEndpoints": a.endpoints,
	})
}

// teamCollaborationBody is the wire shape of TeamCollaboration (API contract §4.4b).
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

func toEffectiveBody(e domain.Effective) effectiveBody {
	r := e.Reasons
	if r == nil {
		r = []string{}
	}
	return effectiveBody{WebUI: e.WebUI, RESTAPI: e.RESTAPI, Reasons: r}
}

func toTeamCollaborationBody(v collab.View, pending bool) teamCollaborationBody {
	c := v.Collab
	b := teamCollaborationBody{TeamID: c.TeamID, AgentID: c.AgentID, WebUIEnabled: c.WebUIEnabled, RESTAPIEnabled: c.RESTAPIEnabled,
		RegistrationMode: string(c.Mode), Effective: toEffectiveBody(v.Effective), UnmanagedPaperclipAgents: c.Unmanaged,
		Version: int64(c.Version), Generation: int64(c.Generation), CreatedAt: w2Time(c.CreatedAt), CreatedBy: c.CreatedBy, UpdatedAt: w2Time(c.UpdatedAt)}
	if b.UnmanagedPaperclipAgents == nil {
		b.UnmanagedPaperclipAgents = []string{}
	}
	st := c.Status
	if pending && st.ObservedGeneration < int64(c.Generation) {
		st.Phase = domain.PhasePending
	}
	b.Status = toStatusBody(st)
	if v.Hostname != "" {
		h, web, rest := v.Hostname, "https://"+v.Hostname+"/", "https://"+v.Hostname+"/api"
		b.Hostname, b.WebUIURL, b.RESTAPIURL = &h, &web, &rest
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

func toCollaborationEndpointBody(e collab.Endpoint) collaborationEndpointBody {
	c := e.Row.Collab
	b := collaborationEndpointBody{TeamID: c.TeamID, Slug: c.TeamSlug, TeamState: string(c.TeamState), AgentID: c.AgentID,
		WebUIEnabled: c.WebUIEnabled, RESTAPIEnabled: c.RESTAPIEnabled, MayExposeCollaboration: c.MayExpose,
		Phase: string(e.Row.Phase), LastAccessAt: w2TimePtr(e.Row.LastAccessAt)}
	b.Effective.WebUI, b.Effective.RESTAPI = e.Effective.WebUI, e.Effective.RESTAPI
	if e.Hostname != "" {
		h := e.Hostname
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
	v, err := a.Svc.Get(r.Context(), c, r.PathValue("teamId"))
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

func (a *CollaborationAPI) put(w http.ResponseWriter, r *http.Request) {
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
	p := domain.PutCollaborationParams{WebUIEnabled: *req.WebUIEnabled, RESTAPIEnabled: *req.RESTAPIEnabled}
	if req.RegistrationMode != nil {
		p.Mode = domain.RegistrationMode(*req.RegistrationMode)
	}
	v, err := a.Svc.Put(r.Context(), c, r.PathValue("teamId"), p, expect)
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

func (a *CollaborationAPI) endpoints(w http.ResponseWriter, r *http.Request) {
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
	f := collab.EndpointFilter{TeamID: q.Get("teamId"), State: domain.Phase(q.Get("state"))}
	switch f.State {
	case "", domain.PhaseReady, domain.PhaseDegraded, domain.PhaseFailed, domain.PhasePending:
	default:
		WriteError(w, r, bad("state", "must be Ready, Degraded, Failed or Pending"))
		return
	}
	rows, next, err := a.Svc.Endpoints(r.Context(), c, f, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	token := w2Next(a.Pages, r, c, next)
	if WantsCSV(r) {
		cells := make([][]string, 0, len(rows))
		for _, e := range rows {
			b := toCollaborationEndpointBody(e)
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
	for _, e := range rows {
		out.Items = append(out.Items, toCollaborationEndpointBody(e))
	}
	WriteJSON(w, http.StatusOK, out)
}

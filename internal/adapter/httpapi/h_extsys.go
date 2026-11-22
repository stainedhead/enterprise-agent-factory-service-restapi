package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/extsys"
)

// ExtSystemsAPI serves the external system operations of M2-E: the systems a team and its agents
// can reach, the team's own declarations and the on/off state at team and agent level.
type ExtSystemsAPI struct {
	Systems *extsys.Service
	Pages   PageTokens
}

// BindExtSystems binds the external system handlers as the extsys area.
func BindExtSystems(a *ExtSystemsAPI) Registrar { return a.Register }

// Register binds the external system operations.
func (a *ExtSystemsAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListTeamExternalSystems": a.listTeam, "SetTeamExternalSystemState": a.setTeamState,
		"DeclareTeamExternalSystem": a.declare, "UpdateTeamExternalSystem": a.update, "DeleteTeamExternalSystem": a.delete,
		"ListAgentExternalSystems": a.listAgent, "SetAgentExternalSystemState": a.setAgentState,
	})
}

func (a *ExtSystemsAPI) listTeam(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.ExternalSystemAccess, string, error) {
		return a.Systems.ListTeam(r.Context(), c, r.PathValue("teamId"), p)
	}, toExtSystemAccessBody)
}

func (a *ExtSystemsAPI) listAgent(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.ExternalSystemAccess, string, error) {
		return a.Systems.ListAgent(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), p)
	}, toExtSystemAccessBody)
}

// stateRequest decodes the {enabled} body of the state operations.
func stateRequest(r *http.Request) (bool, error) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := Decode(r, &req); err != nil {
		return false, err
	}
	if req.Enabled == nil {
		return false, domain.Validation(domain.FieldError{Field: "enabled", Reason: "is required"})
	}
	return *req.Enabled, nil
}

func (a *ExtSystemsAPI) setTeamState(w http.ResponseWriter, r *http.Request) {
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
	enabled, err := stateRequest(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.Systems.SetTeamState(r.Context(), c, r.PathValue("teamId"), r.PathValue("systemId"), enabled, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, out.Version)
	WriteJSON(w, http.StatusOK, toExtSystemAccessBody(out))
}

func (a *ExtSystemsAPI) setAgentState(w http.ResponseWriter, r *http.Request) {
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
	enabled, err := stateRequest(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.Systems.SetAgentState(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), r.PathValue("systemId"), enabled, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, out.Version)
	WriteJSON(w, http.StatusOK, toExtSystemAccessBody(out))
}

func (a *ExtSystemsAPI) declare(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Slug        string   `json:"slug"`
		Name        string   `json:"name"`
		Category    string   `json:"category"`
		Description string   `json:"description"`
		DocsURL     string   `json:"docsUrl"`
		Endpoints   []string `json:"endpoints"`
		Owner       string   `json:"owner"`
		Purpose     string   `json:"purpose"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	teamID := r.PathValue("teamId")
	idem, err := w2Idem(r, "DeclareTeamExternalSystem", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toExtSystemAccessBody(v.(domain.ExternalSystemAccess)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Systems.Declare(r.Context(), c, teamID, domain.DeclareSystemParams{Slug: req.Slug, Name: req.Name,
		Category: domain.ExtCategory(req.Category), Description: req.Description, DocsURL: req.DocsURL, Endpoints: req.Endpoints,
		Owner: req.Owner, Purpose: req.Purpose}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.System.Version)
	w.Header().Set("Location", "/v1/teams/"+teamID+"/external-systems/"+res.System.ID)
	WriteJSON(w, http.StatusCreated, toExtSystemAccessBody(res.System))
}

func (a *ExtSystemsAPI) update(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Name        *string   `json:"name"`
		Description *string   `json:"description"`
		DocsURL     *string   `json:"docsUrl"`
		Endpoints   *[]string `json:"endpoints"`
		Owner       *string   `json:"owner"`
		Purpose     *string   `json:"purpose"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	out, err := a.Systems.Update(r.Context(), c, r.PathValue("teamId"), r.PathValue("systemId"), domain.SystemUpdate{Name: req.Name,
		Description: req.Description, DocsURL: req.DocsURL, Endpoints: req.Endpoints, Owner: req.Owner, Purpose: req.Purpose}, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, out.Version)
	WriteJSON(w, http.StatusOK, toExtSystemAccessBody(out))
}

func (a *ExtSystemsAPI) delete(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	expect, err := w2IfMatch(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Systems.Delete(r.Context(), c, r.PathValue("teamId"), r.PathValue("systemId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

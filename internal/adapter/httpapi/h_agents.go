package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
)

// AgentsAPI serves the agent operations (M2-B).
type AgentsAPI struct {
	Svc   *agents.Service
	Pages PageTokens
}

// BindAgents binds the agent handlers as the agents area.
func BindAgents(a *AgentsAPI) Registrar { return a.Register }

// Register binds the agent operations.
func (a *AgentsAPI) Register(g *Registry) error {
	for id, h := range map[string]http.HandlerFunc{
		"ListAgents": a.list, "CreateAgent": a.create, "GetAgent": a.get, "UpdateAgent": a.update, "DeleteAgent": a.del,
		"PauseAgent": a.pause, "ResumeAgent": a.resume, "RestartAgent": a.restart, "GetAgentStatus": a.status,
		"ListAgentSnapshots": a.listSnapshots, "CreateAgentSnapshot": a.createSnapshot,
	} {
		if err := g.Handle(id, h); err != nil {
			return err
		}
	}
	return nil
}

func (a *AgentsAPI) list(w http.ResponseWriter, r *http.Request) {
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
	role := domain.AgentRole(r.URL.Query().Get("role"))
	if role != "" && !role.Valid() {
		WriteError(w, r, bad("role", "must be worker or orchestration"))
		return
	}
	rows, next, err := a.Svc.List(r.Context(), c, r.PathValue("teamId"), role, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[agentBody]{Items: make([]agentBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
	for _, x := range rows {
		out.Items = append(out.Items, toAgentBodyFor(c, x))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *AgentsAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	x, err := a.Svc.Get(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, x.Version)
	WriteJSON(w, http.StatusOK, toAgentBodyFor(c, x))
}

func (a *AgentsAPI) status(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	x, err := a.Svc.Get(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, struct {
		ID         string        `json:"id"`
		Kind       string        `json:"kind"`
		Generation int64         `json:"generation"`
		Status     domain.Status `json:"status"`
	}{x.ID, agents.StatusKind, int64(x.Generation), toAgentBody(x).Status})
}

func (a *AgentsAPI) create(w http.ResponseWriter, r *http.Request) {
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
	var req createAgentRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	vol, err := req.Volume.spec()
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if req.ModelID != "" {
		DefaultDeprecation.Apply(w, "")
	}
	idem, err := w2Idem(r, "CreateAgent", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toAgentBody(v.(domain.Agent)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Create(r.Context(), c, teamID, domain.NewAgentParams{Name: req.Name, Description: req.Description,
		Template: req.Template, Image: req.Image, ModelID: req.ModelID, Models: req.Models.input(), Resources: req.Resources,
		IAMIdentity: domain.IAMIdentity(req.IAMIdentity), Volume: vol, Bindings: bindingsOf(req.SecretBinds),
		Role: domain.AgentRole(req.Role), Collaboration: req.Collaboration.input(), Mcp: req.Mcp.patch()}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Agent.Version)
	w.Header().Set("Location", "/v1/teams/"+teamID+"/agents/"+res.Agent.ID)
	WriteJSON(w, http.StatusCreated, toAgentBody(res.Agent))
}

func (a *AgentsAPI) update(w http.ResponseWriter, r *http.Request) {
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
	var req updateAgentRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	p := domain.AgentPatch{Name: req.Name, Description: req.Description, Template: req.Template, Image: req.Image, ModelID: req.ModelID,
		Models: req.Models.input(), Resources: req.Resources, Collaboration: req.Collaboration.input(), Mcp: req.Mcp.patch()}
	if req.ModelID != nil {
		// The deprecated modelId alone changes only the default; with a different models.defaultModelId it
		// is refused by the domain as conflicting_fields.
		DefaultDeprecation.Apply(w, "")
	}
	if req.Role != nil {
		role := domain.AgentRole(*req.Role)
		p.Role = &role
	}
	if req.IAMIdentity != nil {
		v := domain.IAMIdentity(*req.IAMIdentity)
		p.IAMIdentity = &v
	}
	if req.Volume != nil {
		vol, err := req.Volume.spec()
		if err != nil {
			WriteError(w, r, err)
			return
		}
		p.Volume = &vol
	}
	if req.SecretBinds != nil {
		b := bindingsOf(*req.SecretBinds)
		p.Bindings = &b
	}
	x, err := a.Svc.Update(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), expect, p)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, x.Version)
	WriteJSON(w, http.StatusOK, toAgentBodyFor(c, x))
}

// del serves DeleteAgent: 202 with the agent in status Deleting (CD-1).
func (a *AgentsAPI) del(w http.ResponseWriter, r *http.Request) {
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
	x, err := a.Svc.Delete(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, x.Version)
	WriteJSON(w, http.StatusAccepted, toAgentBodyFor(c, x))
}

// verb serves PauseAgent, ResumeAgent and RestartAgent.
func (a *AgentsAPI) verb(w http.ResponseWriter, r *http.Request, op string, status int,
	call func(*http.Request, acting.Caller, string, string, *acting.Idem) (agents.Result, error)) {
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
	idem, err := w2Idem(r, op, teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(status, toAgentBody(v.(domain.Agent)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := call(r, c, teamID, r.PathValue("agentId"), idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Agent.Version)
	WriteJSON(w, status, toAgentBodyFor(c, res.Agent))
}

func (a *AgentsAPI) pause(w http.ResponseWriter, r *http.Request) {
	a.verb(w, r, "PauseAgent", http.StatusOK, func(r *http.Request, c acting.Caller, t, id string, i *acting.Idem) (agents.Result, error) {
		return a.Svc.Pause(r.Context(), c, t, id, i)
	})
}

func (a *AgentsAPI) resume(w http.ResponseWriter, r *http.Request) {
	a.verb(w, r, "ResumeAgent", http.StatusOK, func(r *http.Request, c acting.Caller, t, id string, i *acting.Idem) (agents.Result, error) {
		return a.Svc.Resume(r.Context(), c, t, id, i)
	})
}

func (a *AgentsAPI) restart(w http.ResponseWriter, r *http.Request) {
	a.verb(w, r, "RestartAgent", http.StatusAccepted, func(r *http.Request, c acting.Caller, t, id string, i *acting.Idem) (agents.Result, error) {
		return a.Svc.Restart(r.Context(), c, t, id, i)
	})
}

func (a *AgentsAPI) listSnapshots(w http.ResponseWriter, r *http.Request) {
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
	rows, next, err := a.Svc.ListSnapshots(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[snapshotBody]{Items: make([]snapshotBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
	for _, s := range rows {
		out.Items = append(out.Items, toSnapshotBody(s))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *AgentsAPI) createSnapshot(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		Description string `json:"description"`
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := Decode(r, &req); err != nil {
			WriteError(w, r, err)
			return
		}
	}
	idem, err := w2Idem(r, "CreateAgentSnapshot", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusAccepted, toSnapshotBody(v.(domain.Snapshot)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.CreateSnapshot(r.Context(), c, teamID, r.PathValue("agentId"), req.Description, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	WriteJSON(w, http.StatusAccepted, toSnapshotBody(res.Snapshot))
}

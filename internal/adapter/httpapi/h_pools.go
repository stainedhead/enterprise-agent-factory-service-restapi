package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pools"
)

// PoolsAPI serves the swarm operations of API contract §4.4a (M5-B): stages, pools, scale, pause,
// resume, handoffs, secret bindings, configuration, external systems and role links.
type PoolsAPI struct {
	Svc   *pools.Service
	Pages PageTokens
}

// BindPools binds the pool handlers as the pools area.
func BindPools(a *PoolsAPI) Registrar { return a.Register }

// Register binds the pool operations.
func (a *PoolsAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListStages": a.listStages, "CreateStage": a.createStage, "UpdateStage": a.updateStage, "DeleteStage": a.deleteStage,
		"ListPools": a.list, "CreatePool": a.create, "GetPool": a.get, "UpdatePool": a.update, "DeletePool": a.del,
		"ScalePool": a.scale, "PausePool": a.pause, "ResumePool": a.resume, "GetPoolStatus": a.status,
		"ListPoolHandoffs": a.listHandoffs, "PutPoolHandoffs": a.putHandoffs,
		"ListPoolSecretBindings": a.listBindings, "PutPoolSecretBindings": a.putBindings,
		"ListPoolConfigValues": a.listConfig, "PutPoolConfigValue": a.putConfig, "DeletePoolConfigValue": a.deleteConfig,
		"ListPoolExternalSystems": a.listSystems, "PutPoolExternalSystem": a.putSystem,
		"ListPoolRoleLinks": a.listLinks, "CreatePoolRoleLink": a.createLink, "DeletePoolRoleLink": a.deleteLink,
		"VerifyPoolRoleLink": a.verifyLink,
	})
}

// ---- stages ----

func (a *PoolsAPI) listStages(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.Stage, string, error) {
		return a.Svc.ListStages(r.Context(), c, r.PathValue("teamId"), p)
	}, toStageBody)
}

func (a *PoolsAPI) createStage(w http.ResponseWriter, r *http.Request) {
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
	var req createStageRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	idem, err := w2Idem(r, "CreateStage", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toStageBody(v.(domain.Stage)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.CreateStage(r.Context(), c, teamID, domain.NewStageParams{Name: req.Name, Position: req.Position, Description: req.Description}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Stage.Version)
	w.Header().Set("Location", "/v1/teams/"+teamID+"/stages/"+res.Stage.ID)
	WriteJSON(w, http.StatusCreated, toStageBody(res.Stage))
}

func (a *PoolsAPI) updateStage(w http.ResponseWriter, r *http.Request) {
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
	var req updateStageRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.Svc.UpdateStage(r.Context(), c, r.PathValue("teamId"), r.PathValue("stageId"), expect,
		domain.StagePatch{Position: req.Position, Description: req.Description})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, s.Version)
	WriteJSON(w, http.StatusOK, toStageBody(s))
}

func (a *PoolsAPI) deleteStage(w http.ResponseWriter, r *http.Request) {
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
	if err := a.Svc.DeleteStage(r.Context(), c, r.PathValue("teamId"), r.PathValue("stageId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- pools ----

func (a *PoolsAPI) list(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.Pool, string, error) {
		return a.Svc.List(r.Context(), c, r.PathValue("teamId"), p)
	}, toPoolBody)
}

func (a *PoolsAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	p, err := a.Svc.Get(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, p.Version)
	WriteJSON(w, http.StatusOK, toPoolBody(p))
}

func (a *PoolsAPI) status(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.Svc.GetStatus(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toPoolStatusBody(s))
}

func (a *PoolsAPI) create(w http.ResponseWriter, r *http.Request) {
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
	var req createPoolRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Workers == nil {
		WriteError(w, r, bad("workers", "is required"))
		return
	}
	idem, err := w2Idem(r, "CreatePool", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toPoolBody(v.(domain.Pool)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if req.ModelID != "" {
		DefaultDeprecation.Apply(w, "")
	}
	in := domain.NewPoolParams{Name: req.Name, Description: req.Description, Template: req.Template, Image: req.Image,
		Persona: req.Persona, ModelID: req.ModelID, Models: req.Models.input(), Workers: *req.Workers, Resources: req.Resources,
		ScratchSizeGiB: req.ScratchSizeGiB, SharedVolume: req.SharedVolume}
	if req.StageID != nil {
		in.StageID = *req.StageID
	}
	res, err := a.Svc.Create(r.Context(), c, teamID, in, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Pool.Version)
	w.Header().Set("Location", "/v1/teams/"+teamID+"/pools/"+res.Pool.ID)
	WriteJSON(w, http.StatusCreated, toPoolBody(res.Pool))
}

func (a *PoolsAPI) update(w http.ResponseWriter, r *http.Request) {
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
	var req updatePoolRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.ModelID != nil {
		DefaultDeprecation.Apply(w, "")
	}
	p, err := a.Svc.Update(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"), expect, req.patch())
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, p.Version)
	WriteJSON(w, http.StatusOK, toPoolBody(p))
}

// del serves DeletePool: 202 with the pool in status Deleting (CD-1).
func (a *PoolsAPI) del(w http.ResponseWriter, r *http.Request) {
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
	p, err := a.Svc.Delete(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"), expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, p.Version)
	WriteJSON(w, http.StatusAccepted, toPoolBody(p))
}

// verb serves ScalePool, PausePool and ResumePool.
func (a *PoolsAPI) verb(w http.ResponseWriter, r *http.Request, op string, call func(c acting.Caller, teamID, id string, idem *acting.Idem) (pools.Result, error)) {
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
		return w2Render(http.StatusOK, toPoolBody(v.(domain.Pool)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := call(c, teamID, r.PathValue("poolId"), idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Pool.Version)
	WriteJSON(w, http.StatusOK, toPoolBody(res.Pool))
}

func (a *PoolsAPI) scale(w http.ResponseWriter, r *http.Request) {
	raw, err := w2Body(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Workers *int `json:"workers"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw)) // the idempotency hash covers the body
	if req.Workers == nil {
		WriteError(w, r, bad("workers", "is required"))
		return
	}
	a.verb(w, r, "ScalePool", func(c acting.Caller, t, id string, i *acting.Idem) (pools.Result, error) {
		return a.Svc.Scale(r.Context(), c, t, id, *req.Workers, i)
	})
}

func (a *PoolsAPI) pause(w http.ResponseWriter, r *http.Request) {
	a.verb(w, r, "PausePool", func(c acting.Caller, t, id string, i *acting.Idem) (pools.Result, error) {
		return a.Svc.Pause(r.Context(), c, t, id, i)
	})
}

func (a *PoolsAPI) resume(w http.ResponseWriter, r *http.Request) {
	a.verb(w, r, "ResumePool", func(c acting.Caller, t, id string, i *acting.Idem) (pools.Result, error) {
		return a.Svc.Resume(r.Context(), c, t, id, i)
	})
}

// ---- handoffs ----

func (a *PoolsAPI) listHandoffs(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	h, err := a.Svc.ListHandoffs(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, h.Version)
	WriteJSON(w, http.StatusOK, toPoolHandoffsBody(h))
}

func (a *PoolsAPI) putHandoffs(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		ToPoolIDs *[]string `json:"toPoolIds"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.ToPoolIDs == nil {
		WriteError(w, r, bad("toPoolIds", "is required"))
		return
	}
	h, err := a.Svc.PutHandoffs(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"), expect, *req.ToPoolIDs)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, h.Version)
	WriteJSON(w, http.StatusOK, toPoolHandoffsBody(h))
}

// ---- secret bindings ----

func (a *PoolsAPI) listBindings(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.Svc.ListBindings(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, s.Version)
	WriteJSON(w, http.StatusOK, toPoolBindingsBody(s))
}

func (a *PoolsAPI) putBindings(w http.ResponseWriter, r *http.Request) {
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
	var req struct {
		Items *[]bindingRequest `json:"items"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Items == nil {
		WriteError(w, r, bad("items", "is required"))
		return
	}
	s, err := a.Svc.PutBindings(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"), expect, bindingsOf(*req.Items))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, s.Version)
	WriteJSON(w, http.StatusOK, toPoolBindingsBody(s))
}

// ---- configuration ----

func (a *PoolsAPI) listConfig(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.PoolConfigValue, string, error) {
		return a.Svc.ListConfig(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"), p)
	}, toPoolConfigBody)
}

func (a *PoolsAPI) putConfig(w http.ResponseWriter, r *http.Request) {
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
	name, err := domain.ParseConfigName("name", r.PathValue("name"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var req struct {
		Source string  `json:"source"`
		Value  *string `json:"value"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.Svc.PutConfig(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"), name,
		pools.ConfigInput{Source: domain.ConfigSource(req.Source), Value: req.Value}, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, v.Version)
	WriteJSON(w, http.StatusOK, toPoolConfigBody(v))
}

func (a *PoolsAPI) deleteConfig(w http.ResponseWriter, r *http.Request) {
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
	name, err := domain.ParseConfigName("name", r.PathValue("name"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Svc.DeleteConfig(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"), name, expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- external systems ----

func (a *PoolsAPI) listSystems(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.ExternalSystemAccess, string, error) {
		return a.Svc.ListSystems(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"), p)
	}, toExtSystemAccessBody)
}

func (a *PoolsAPI) putSystem(w http.ResponseWriter, r *http.Request) {
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
	out, err := a.Svc.SetSystemState(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"), r.PathValue("systemId"), enabled, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, out.Version)
	WriteJSON(w, http.StatusOK, toExtSystemAccessBody(out))
}

// ---- role links ----

func (a *PoolsAPI) listLinks(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.PoolRoleLink, string, error) {
		return a.Svc.ListLinks(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"), p)
	}, toPoolRoleLinkBody)
}

func (a *PoolsAPI) createLink(w http.ResponseWriter, r *http.Request) {
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
		RoleName          string `json:"roleName"`
		AccountID         string `json:"accountId"`
		ProfileName       string `json:"profileName"`
		MaxSessionSeconds int    `json:"maxSessionSeconds"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	teamID, poolID := r.PathValue("teamId"), r.PathValue("poolId")
	idem, err := w2Idem(r, "CreatePoolRoleLink", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toPoolRoleLinkBody(v.(domain.PoolRoleLink)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.CreateLink(r.Context(), c, teamID, poolID, domain.NewPoolRoleLinkParams{RoleName: req.RoleName,
		AccountID: req.AccountID, ProfileName: req.ProfileName, MaxSessionSeconds: req.MaxSessionSeconds}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Link.Version)
	w.Header().Set("Location", "/v1/teams/"+teamID+"/pools/"+poolID+"/role-links/"+res.Link.ID)
	WriteJSON(w, http.StatusCreated, toPoolRoleLinkBody(res.Link))
}

func (a *PoolsAPI) verifyLink(w http.ResponseWriter, r *http.Request) {
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
	teamID := r.PathValue("teamId")
	idem, err := w2Idem(r, "VerifyPoolRoleLink", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusAccepted, toPoolRoleLinkBody(v.(domain.PoolRoleLink)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.VerifyLink(r.Context(), c, teamID, r.PathValue("poolId"), r.PathValue("linkId"), idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Link.Version)
	WriteJSON(w, http.StatusAccepted, toPoolRoleLinkBody(res.Link))
}

func (a *PoolsAPI) deleteLink(w http.ResponseWriter, r *http.Request) {
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
	if err := a.Svc.DeleteLink(r.Context(), c, r.PathValue("teamId"), r.PathValue("poolId"), r.PathValue("linkId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

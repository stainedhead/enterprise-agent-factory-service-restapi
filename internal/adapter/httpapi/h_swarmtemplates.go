package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/swarmtemplates"
)

// SwarmTemplatesAPI serves the swarm template operations of API contract §4.10a (M6-B1).
type SwarmTemplatesAPI struct {
	Svc   *swarmtemplates.Service
	Pages PageTokens
}

// BindSwarmTemplates binds the swarm template handlers as the swarmtemplates area.
func BindSwarmTemplates(a *SwarmTemplatesAPI) { w2SetArea("swarmtemplates", a.Register) }

// Register binds the swarm template operations.
func (a *SwarmTemplatesAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListSwarmTemplates": a.list, "CreateSwarmTemplate": a.create, "GetSwarmTemplate": a.get,
		"UpdateSwarmTemplate": a.update, "DeleteSwarmTemplate": a.del,
		"EnableSwarmTemplate": a.enable, "DisableSwarmTemplate": a.disable,
	})
}

type swarmTemplateBody struct {
	ID           string         `json:"id"`
	Version      int64          `json:"version"`
	CreatedAt    string         `json:"createdAt"`
	CreatedBy    string         `json:"createdBy"`
	UpdatedAt    string         `json:"updatedAt"`
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Kind         string         `json:"kind"`
	Spec         map[string]any `json:"spec"`
	BudgetUSD    string         `json:"budgetUsd"`
	TTLHours     int            `json:"ttlHours"`
	TeardownRule string         `json:"teardownRule"`
	Enabled      bool           `json:"enabled"`
}

func toSwarmTemplateBody(t domain.SwarmTemplate) swarmTemplateBody {
	return swarmTemplateBody{ID: t.ID, Version: int64(t.Version), CreatedAt: w2Time(t.CreatedAt), CreatedBy: t.CreatedBy,
		UpdatedAt: w2Time(t.UpdatedAt), Name: string(t.Name), Description: t.Description, Kind: string(t.Kind), Spec: t.Spec,
		BudgetUSD: t.Budget.String(), TTLHours: t.TTLHours, TeardownRule: string(t.TeardownRule), Enabled: t.Enabled}
}

func (a *SwarmTemplatesAPI) list(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.SwarmTemplate, string, error) {
		return a.Svc.List(r.Context(), c, p)
	}, toSwarmTemplateBody)
}

func (a *SwarmTemplatesAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := a.Svc.Get(r.Context(), c, r.PathValue("templateId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, t.Version)
	WriteJSON(w, http.StatusOK, toSwarmTemplateBody(t))
}

func (a *SwarmTemplatesAPI) create(w http.ResponseWriter, r *http.Request) {
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
		Name         string         `json:"name"`
		Description  string         `json:"description"`
		Kind         string         `json:"kind"`
		Spec         map[string]any `json:"spec"`
		BudgetUSD    string         `json:"budgetUsd"`
		TTLHours     int            `json:"ttlHours"`
		TeardownRule string         `json:"teardownRule"`
		Enabled      *bool          `json:"enabled"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	idem, err := w2Idem(r, "CreateSwarmTemplate", "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toSwarmTemplateBody(v.(domain.SwarmTemplate)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Create(r.Context(), c, domain.NewSwarmTemplateParams{Name: req.Name, Description: req.Description, Kind: req.Kind,
		Spec: req.Spec, BudgetUSD: req.BudgetUSD, TTLHours: req.TTLHours, TeardownRule: req.TeardownRule, Enabled: req.Enabled}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Template.Version)
	w.Header().Set("Location", "/v1/admin/swarm-templates/"+res.Template.ID)
	WriteJSON(w, http.StatusCreated, toSwarmTemplateBody(res.Template))
}

func (a *SwarmTemplatesAPI) update(w http.ResponseWriter, r *http.Request) {
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
		Description  *string        `json:"description"`
		Spec         map[string]any `json:"spec"`
		BudgetUSD    *string        `json:"budgetUsd"`
		TTLHours     *int           `json:"ttlHours"`
		TeardownRule *string        `json:"teardownRule"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := a.Svc.Update(r.Context(), c, r.PathValue("templateId"), expect, domain.SwarmTemplatePatch{
		Description: req.Description, Spec: req.Spec, BudgetUSD: req.BudgetUSD, TTLHours: req.TTLHours, TeardownRule: req.TeardownRule})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, t.Version)
	WriteJSON(w, http.StatusOK, toSwarmTemplateBody(t))
}

func (a *SwarmTemplatesAPI) del(w http.ResponseWriter, r *http.Request) {
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
	if err := a.Svc.Delete(r.Context(), c, r.PathValue("templateId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *SwarmTemplatesAPI) enable(w http.ResponseWriter, r *http.Request) {
	a.verb(w, r, "EnableSwarmTemplate", a.Svc.Enable)
}

func (a *SwarmTemplatesAPI) disable(w http.ResponseWriter, r *http.Request) {
	a.verb(w, r, "DisableSwarmTemplate", a.Svc.Disable)
}

func (a *SwarmTemplatesAPI) verb(w http.ResponseWriter, r *http.Request, op string,
	call func(ctx context.Context, c acting.Caller, id string, idem *acting.Idem) (swarmtemplates.Result, error)) {
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
	idem, err := w2Idem(r, op, "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusOK, toSwarmTemplateBody(v.(domain.SwarmTemplate)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := call(r.Context(), c, r.PathValue("templateId"), idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Template.Version)
	WriteJSON(w, http.StatusOK, toSwarmTemplateBody(res.Template))
}

package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agenttemplates"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/catalogsystems"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/models"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/storageclasses"
)

// CatalogAPI serves the service catalogs of M2-A: models and prices, storage classes, agent
// templates and catalog systems. Each operation's route-level permission is enforced by the
// authorization middleware; the use cases check it again.
type CatalogAPI struct {
	Models         *models.Service
	StorageClasses *storageclasses.Service
	Templates      *agenttemplates.Service
	Systems        *catalogsystems.Service
	Pages          PageTokens
}

// BindCatalog binds the catalog handlers as the adminmodels area.
func BindCatalog(a *CatalogAPI) Registrar { return a.Register }

// Register binds the catalog operations.
func (a *CatalogAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListModels": a.listModels, "ListAdminModels": a.listAdminModels, "PutServiceModelAccess": a.putServiceModelAccess, "CreateModel": a.createModel, "UpdateModel": a.updateModel,
		"ListModelPrices": a.listPrices, "CreateModelPrice": a.createPrice,
		"ListStorageClasses": a.listStorageClasses, "ListStorageClassesAdmin": a.listStorageClassesAdmin,
		"PutStorageClass": a.putStorageClass, "DeleteStorageClass": a.deleteStorageClass,
		"ListAgentTemplates": a.listTemplates, "ListAdminAgentTemplates": a.listAdminTemplates,
		"PutAgentTemplate": a.putTemplate, "DeleteAgentTemplate": a.deleteTemplate,
		"ListCatalogSystems": a.listSystems, "CreateCatalogSystem": a.createSystem, "GetCatalogSystem": a.getSystem,
		"UpdateCatalogSystem": a.updateSystem, "DeleteCatalogSystem": a.deleteSystem,
	})
}

// listOf runs a list use case and writes the page. fetch returns the rows and the next cursor.
func listOf[T, B any](a *CatalogAPI, w http.ResponseWriter, r *http.Request, fetch func(c acting.Caller, p acting.Page) ([]T, string, error), body func(T) B) {
	listWith(a.Pages, w, r, fetch, body)
}

// listWith is listOf for any handler set that holds PageTokens.
func listWith[T, B any](pages PageTokens, w http.ResponseWriter, r *http.Request, fetch func(c acting.Caller, p acting.Page) ([]T, string, error), body func(T) B) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	page, err := w2Page(pages, r, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rows, next, err := fetch(c, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[B]{Items: make([]B, 0, len(rows)), NextToken: w2Next(pages, r, c, next)}
	for _, row := range rows {
		out.Items = append(out.Items, body(row))
	}
	WriteJSON(w, http.StatusOK, out)
}

// ---- models and prices ----

func (a *CatalogAPI) listModels(w http.ResponseWriter, r *http.Request) {
	listOf(a, w, r, func(c acting.Caller, p acting.Page) ([]domain.Model, string, error) {
		return a.Models.ListEnabled(r.Context(), c, p)
	}, toModelBody)
}

func (a *CatalogAPI) listAdminModels(w http.ResponseWriter, r *http.Request) {
	listOf(a, w, r, func(c acting.Caller, p acting.Page) ([]domain.Model, string, error) {
		return a.Models.ListAll(r.Context(), c, p)
	}, toAdminModelBody)
}

// serviceModelAccessBody is ServiceModelAccess (API contract §5).
type serviceModelAccessBody struct {
	Access         string `json:"access"`
	EnabledModels  int    `json:"enabledModels"`
	CallableModels int    `json:"callableModels"`
	Version        int64  `json:"version"`
}

func (a *CatalogAPI) putServiceModelAccess(w http.ResponseWriter, r *http.Request) {
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
		Access *string `json:"access"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Access == nil {
		WriteError(w, r, bad("access", "is required"))
		return
	}
	out, err := a.Models.PutServiceAccess(r.Context(), c, *req.Access, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, out.Version)
	WriteJSON(w, http.StatusOK, serviceModelAccessBody{Access: string(out.Access), EnabledModels: out.EnabledModels,
		CallableModels: out.CallableModels, Version: int64(out.Version)})
}

func (a *CatalogAPI) createModel(w http.ResponseWriter, r *http.Request) {
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
		ID              string `json:"id"`
		Provider        string `json:"provider"`
		DisplayName     string `json:"displayName"`
		Enabled         *bool  `json:"enabled"`
		ContextWindow   int    `json:"contextWindow"`
		MaxOutputTokens int    `json:"maxOutputTokens"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	idem, err := w2Idem(r, "CreateModel", "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toModelBody(v.(domain.Model)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Models.Create(r.Context(), c, domain.NewModelParams{ID: req.ID, Provider: req.Provider, DisplayName: req.DisplayName,
		Enabled: req.Enabled, ContextWindow: req.ContextWindow, MaxOutputTokens: req.MaxOutputTokens}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Value.Version)
	w.Header().Set("Location", "/v1/admin/models/"+res.Value.ID)
	WriteJSON(w, http.StatusCreated, toModelBody(res.Value))
}

func (a *CatalogAPI) updateModel(w http.ResponseWriter, r *http.Request) {
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
		DisplayName     *string `json:"displayName"`
		Enabled         *bool   `json:"enabled"`
		ContextWindow   *int    `json:"contextWindow"`
		MaxOutputTokens *int    `json:"maxOutputTokens"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	m, err := a.Models.Update(r.Context(), c, r.PathValue("modelId"), domain.ModelUpdate{DisplayName: req.DisplayName, Enabled: req.Enabled,
		ContextWindow: req.ContextWindow, MaxOutputTokens: req.MaxOutputTokens}, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, m.Version)
	WriteJSON(w, http.StatusOK, toModelBody(m))
}

func (a *CatalogAPI) listPrices(w http.ResponseWriter, r *http.Request) {
	listOf(a, w, r, func(c acting.Caller, p acting.Page) ([]domain.ModelPrice, string, error) {
		return a.Models.ListPrices(r.Context(), c, r.PathValue("modelId"), p)
	}, toModelPriceBody)
}

func (a *CatalogAPI) createPrice(w http.ResponseWriter, r *http.Request) {
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
		EffectiveFrom        string        `json:"effectiveFrom"`
		InputUsdPerMtok      *domain.Money `json:"inputUsdPerMtok"`
		OutputUsdPerMtok     *domain.Money `json:"outputUsdPerMtok"`
		CacheReadUsdPerMtok  *domain.Money `json:"cacheReadUsdPerMtok"`
		CacheWriteUsdPerMtok *domain.Money `json:"cacheWriteUsdPerMtok"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	at, err := w2ParseTime("effectiveFrom", req.EffectiveFrom)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var fe []domain.FieldError
	if req.InputUsdPerMtok == nil {
		fe = append(fe, domain.FieldError{Field: "inputUsdPerMtok", Reason: "is required"})
	}
	if req.OutputUsdPerMtok == nil {
		fe = append(fe, domain.FieldError{Field: "outputUsdPerMtok", Reason: "is required"})
	}
	if len(fe) > 0 {
		WriteError(w, r, domain.Validation(fe...))
		return
	}
	modelID := r.PathValue("modelId")
	idem, err := w2Idem(r, "CreateModelPrice", "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toModelPriceBody(v.(domain.ModelPrice)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Models.CreatePrice(r.Context(), c, modelID, domain.NewModelPriceParams{EffectiveFrom: at, Input: *req.InputUsdPerMtok,
		Output: *req.OutputUsdPerMtok, CacheRead: req.CacheReadUsdPerMtok, CacheWrite: req.CacheWriteUsdPerMtok}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	WriteJSON(w, http.StatusCreated, toModelPriceBody(res.Value))
}

// ---- storage classes ----

func (a *CatalogAPI) listStorageClasses(w http.ResponseWriter, r *http.Request) {
	listOf(a, w, r, func(c acting.Caller, p acting.Page) ([]domain.StorageClass, string, error) {
		return a.StorageClasses.ListEnabled(r.Context(), c, p)
	}, toStorageClassBody)
}

func (a *CatalogAPI) listStorageClassesAdmin(w http.ResponseWriter, r *http.Request) {
	listOf(a, w, r, func(c acting.Caller, p acting.Page) ([]domain.StorageClass, string, error) {
		return a.StorageClasses.ListAll(r.Context(), c, p)
	}, toStorageClassBody)
}

func (a *CatalogAPI) putStorageClass(w http.ResponseWriter, r *http.Request) {
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
		Description string `json:"description"`
		VolumeType  string `json:"volumeType"`
		Enabled     *bool  `json:"enabled"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	enabled := req.Enabled == nil || *req.Enabled
	sc, err := a.StorageClasses.Put(r.Context(), c, domain.StorageClass{Name: r.PathValue("name"), Description: req.Description,
		VolumeType: domain.VolumeType(req.VolumeType), Enabled: enabled}, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, sc.Version)
	WriteJSON(w, http.StatusOK, toStorageClassBody(sc))
}

func (a *CatalogAPI) deleteStorageClass(w http.ResponseWriter, r *http.Request) {
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
	if err := a.StorageClasses.Delete(r.Context(), c, r.PathValue("name"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- agent templates ----

func (a *CatalogAPI) listTemplates(w http.ResponseWriter, r *http.Request) {
	listOf(a, w, r, func(c acting.Caller, p acting.Page) ([]domain.AgentTemplate, string, error) {
		return a.Templates.ListEnabled(r.Context(), c, r.URL.Query().Get("capability"), p)
	}, toAgentTemplateBody)
}

func (a *CatalogAPI) listAdminTemplates(w http.ResponseWriter, r *http.Request) {
	listOf(a, w, r, func(c acting.Caller, p acting.Page) ([]domain.AgentTemplate, string, error) {
		return a.Templates.ListAll(r.Context(), c, r.URL.Query().Get("capability"), p)
	}, toAgentTemplateBody)
}

func (a *CatalogAPI) putTemplate(w http.ResponseWriter, r *http.Request) {
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
		Description      string           `json:"description"`
		Harness          string           `json:"harness"`
		DefaultResources domain.Resources `json:"defaultResources"`
		AllowedDigests   []string         `json:"allowedDigests"`
		CurrentDigest    string           `json:"currentDigest"`
		Enabled          *bool            `json:"enabled"`
		Capabilities     []string         `json:"capabilities"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := a.Templates.Put(r.Context(), c, domain.PutAgentTemplateParams{Name: r.PathValue("name"), Description: req.Description,
		Harness: req.Harness, DefaultResources: req.DefaultResources, AllowedDigests: req.AllowedDigests, CurrentDigest: req.CurrentDigest,
		Enabled: req.Enabled, Capabilities: req.Capabilities}, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, t.Version)
	WriteJSON(w, http.StatusOK, toAgentTemplateBody(t))
}

func (a *CatalogAPI) deleteTemplate(w http.ResponseWriter, r *http.Request) {
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
	if err := a.Templates.Delete(r.Context(), c, r.PathValue("name"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- catalog systems ----

func (a *CatalogAPI) listSystems(w http.ResponseWriter, r *http.Request) {
	listOf(a, w, r, func(c acting.Caller, p acting.Page) ([]domain.ExternalSystem, string, error) {
		return a.Systems.List(r.Context(), c, p)
	}, toCatalogSystemBody)
}

func (a *CatalogAPI) getSystem(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.Systems.Get(r.Context(), c, r.PathValue("systemId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, s.Version)
	WriteJSON(w, http.StatusOK, toCatalogSystemBody(s))
}

func (a *CatalogAPI) createSystem(w http.ResponseWriter, r *http.Request) {
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
		Enabled     *bool    `json:"enabled"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	idem, err := w2Idem(r, "CreateCatalogSystem", "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toCatalogSystemBody(v.(domain.ExternalSystem)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Systems.Create(r.Context(), c, domain.NewCatalogSystemParams{Slug: req.Slug, Name: req.Name, Category: domain.ExtCategory(req.Category),
		Description: req.Description, DocsURL: req.DocsURL, Endpoints: req.Endpoints, Enabled: req.Enabled}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.System.Version)
	w.Header().Set("Location", "/v1/admin/external-systems/"+res.System.ID)
	WriteJSON(w, http.StatusCreated, toCatalogSystemBody(res.System))
}

func (a *CatalogAPI) updateSystem(w http.ResponseWriter, r *http.Request) {
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
		Enabled     *bool     `json:"enabled"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	s, err := a.Systems.Update(r.Context(), c, r.PathValue("systemId"), domain.SystemUpdate{Name: req.Name, Description: req.Description,
		DocsURL: req.DocsURL, Endpoints: req.Endpoints, Enabled: req.Enabled}, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, s.Version)
	WriteJSON(w, http.StatusOK, toCatalogSystemBody(s))
}

func (a *CatalogAPI) deleteSystem(w http.ResponseWriter, r *http.Request) {
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
	if err := a.Systems.Delete(r.Context(), c, r.PathValue("systemId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

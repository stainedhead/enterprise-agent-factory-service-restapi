package httpapi

import (
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/config"
)

// ConfigAPI serves the configuration value operations of M2-D: a team's namespace values and each
// agent's values. Route permissions are enforced by the authorization middleware; the use cases
// check them again.
type ConfigAPI struct {
	Config *config.Service
	Pages  PageTokens
}

// BindConfig binds the configuration handlers as the config area.
func BindConfig(a *ConfigAPI) { w2SetArea("config", a.Register) }

// Register binds the configuration operations.
func (a *ConfigAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListNamespaceConfigValues": a.listNamespace, "PutNamespaceConfigValue": a.putNamespace, "DeleteNamespaceConfigValue": a.deleteNamespace,
		"ListAgentConfigValues": a.listAgent, "PutAgentConfigValue": a.putAgent, "DeleteAgentConfigValue": a.deleteAgent,
	})
}

func (a *ConfigAPI) listNamespace(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.NamespaceConfigValue, string, error) {
		return a.Config.ListNamespace(r.Context(), c, r.PathValue("teamId"), p)
	}, toNamespaceConfigBody)
}

func (a *ConfigAPI) listAgent(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.AgentConfigValue, string, error) {
		return a.Config.ListAgent(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), p)
	}, toAgentConfigBody)
}

func (a *ConfigAPI) putNamespace(w http.ResponseWriter, r *http.Request) {
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
		Value *string `json:"value"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Value == nil {
		WriteError(w, r, domain.Validation(domain.FieldError{Field: "value", Reason: "is required"}))
		return
	}
	v, err := a.Config.PutNamespace(r.Context(), c, r.PathValue("teamId"), name, *req.Value, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, v.Version)
	WriteJSON(w, http.StatusOK, toNamespaceConfigBody(v))
}

func (a *ConfigAPI) deleteNamespace(w http.ResponseWriter, r *http.Request) {
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
	if err := a.Config.DeleteNamespace(r.Context(), c, r.PathValue("teamId"), name, expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *ConfigAPI) putAgent(w http.ResponseWriter, r *http.Request) {
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
	v, err := a.Config.PutAgent(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), name,
		config.AgentValueInput{Source: domain.ConfigSource(req.Source), Value: req.Value}, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, v.Version)
	WriteJSON(w, http.StatusOK, toAgentConfigBody(v))
}

func (a *ConfigAPI) deleteAgent(w http.ResponseWriter, r *http.Request) {
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
	if err := a.Config.DeleteAgent(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), name, expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

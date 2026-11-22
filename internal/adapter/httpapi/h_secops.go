package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secops"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
)

const secOpsAreaName = "secops"

func init() { Areas = append(Areas, Area{secOpsAreaName, none}) }

// SecOpsAPI serves security operators, service-level API keys and the team directory (M6-A):
// ListSecurityOperators, AddSecurityOperator, RemoveSecurityOperator, ListServiceApiKeys,
// CreateServiceApiKey, DeleteServiceApiKey and ListTeamDirectory.
type SecOpsAPI struct {
	Operators *secops.Service
	Keys      *apikeys.ServiceKeys
	Directory *teams.Directory
	Pages     PageTokens
}

// BindSecOps binds the M6-A handlers as the secops area.
func BindSecOps(a *SecOpsAPI) { w2SetArea(secOpsAreaName, a.Register) }

// Register binds the operations.
func (a *SecOpsAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListSecurityOperators":  a.listOperators,
		"AddSecurityOperator":    a.addOperator,
		"RemoveSecurityOperator": a.removeOperator,
		"ListServiceApiKeys":     a.listKeys,
		"CreateServiceApiKey":    a.createKey,
		"DeleteServiceApiKey":    a.deleteKey,
		"ListTeamDirectory":      a.directory,
	})
}

// securityOperatorBody is the SecurityOperator schema: a service-level binding has no role member and
// no team.
type securityOperatorBody struct {
	ID                 string `json:"id"`
	SubjectType        string `json:"subjectType"`
	UserID             string `json:"userId,omitempty"`
	Email              string `json:"email,omitempty"`
	GroupName          string `json:"groupName,omitempty"`
	IdentityProviderID string `json:"identityProviderId,omitempty"`
	Version            int64  `json:"version"`
	CreatedAt          string `json:"createdAt"`
	CreatedBy          string `json:"createdBy"`
	UpdatedAt          string `json:"updatedAt"`
}

func toSecurityOperatorBody(m secops.Operator) securityOperatorBody {
	return securityOperatorBody{ID: m.ID, SubjectType: string(m.SubjectType), UserID: m.UserID, Email: m.Email,
		GroupName: m.GroupName, IdentityProviderID: m.IdentityProviderID, Version: int64(m.Version),
		CreatedAt: w2Time(m.CreatedAt), CreatedBy: m.CreatedBy, UpdatedAt: w2Time(m.UpdatedAt)}
}

func (a *SecOpsAPI) listOperators(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]secops.Operator, string, error) {
		return a.Operators.List(r.Context(), c, p)
	}, toSecurityOperatorBody)
}

func (a *SecOpsAPI) addOperator(w http.ResponseWriter, r *http.Request) {
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
		SubjectType        string `json:"subjectType"`
		UserID             string `json:"userId"`
		Email              string `json:"email"`
		GroupName          string `json:"groupName"`
		IdentityProviderID string `json:"identityProviderId"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	idem, err := w2Idem(r, "AddSecurityOperator", "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toSecurityOperatorBody(v.(secops.Operator)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Operators.Add(r.Context(), c, domain.NewMemberParams{SubjectType: domain.SubjectType(req.SubjectType),
		UserID: req.UserID, Email: req.Email, GroupName: req.GroupName, IdentityProviderID: req.IdentityProviderID}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Operator.Version)
	w.Header().Set("Location", "/v1/admin/security-operators/"+res.Operator.ID)
	WriteJSON(w, http.StatusCreated, toSecurityOperatorBody(res.Operator))
}

func (a *SecOpsAPI) removeOperator(w http.ResponseWriter, r *http.Request) {
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
	if err := a.Operators.Remove(r.Context(), c, r.PathValue("bindingId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *SecOpsAPI) listKeys(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.APIKey, string, error) {
		return a.Keys.List(r.Context(), c, p)
	}, func(k domain.APIKey) apiKeyBody { return toAPIKeyBody(k, "") })
}

func (a *SecOpsAPI) createKey(w http.ResponseWriter, r *http.Request) {
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
		Name      string `json:"name"`
		Role      string `json:"role"`
		ExpiresAt string `json:"expiresAt"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	exp, err := w2ParseTime("expiresAt", req.ExpiresAt)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	// The stored copy of the response never carries the secret (D52).
	idem, err := w2Idem(r, "CreateServiceApiKey", "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toAPIKeyBody(v.(apikeys.Created).Key, ""))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Keys.Create(r.Context(), c, apikeys.NewServiceKeyParams{Name: req.Name, Role: req.Role, ExpiresAt: exp}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w.Header().Set("Location", "/v1/admin/api-keys/"+res.Key.ID)
	WriteJSON(w, http.StatusCreated, toAPIKeyBody(res.Key, res.Secret))
}

func (a *SecOpsAPI) deleteKey(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Keys.Delete(r.Context(), c, r.PathValue("keyId")); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *SecOpsAPI) directory(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]port.TeamDirectoryEntry, string, error) {
		return a.Directory.List(r.Context(), c, p)
	}, func(e port.TeamDirectoryEntry) teamRefBody {
		return teamRefBody{ID: e.ID, Slug: e.Slug, DisplayName: e.DisplayName}
	})
}

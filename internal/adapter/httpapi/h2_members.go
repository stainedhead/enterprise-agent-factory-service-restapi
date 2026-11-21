package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/members"
)

// MembersAPI serves ListMembers, AddMember, UpdateMember and RemoveMember (M1-C1).
type MembersAPI struct {
	Svc   *members.Service
	Pages PageTokens
}

// APIKeysAPI serves ListApiKeys, CreateApiKey and DeleteApiKey (M1-C2).
type APIKeysAPI struct {
	Svc   *apikeys.Service
	Pages PageTokens
}

// BindMembers binds the member handlers as the members area.
func BindMembers(a *MembersAPI) { w2SetArea("members", a.Register) }

// BindAPIKeys binds the API key handlers as the apikeys area.
func BindAPIKeys(a *APIKeysAPI) { w2SetArea("apikeys", a.Register) }

type memberBody struct {
	ID                 string `json:"id"`
	Role               string `json:"role"`
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

func toMemberBody(m domain.Member) memberBody {
	return memberBody{ID: m.ID, Role: string(m.Role), SubjectType: string(m.SubjectType), UserID: m.UserID, Email: m.Email,
		GroupName: m.GroupName, IdentityProviderID: m.IdentityProviderID, Version: int64(m.Version),
		CreatedAt: w2Time(m.CreatedAt), CreatedBy: m.CreatedBy, UpdatedAt: w2Time(m.UpdatedAt)}
}

// Register binds the member operations.
func (a *MembersAPI) Register(g *Registry) error {
	for id, h := range map[string]http.HandlerFunc{
		"ListMembers": a.list, "AddMember": a.add, "UpdateMember": a.update, "RemoveMember": a.remove,
	} {
		if err := g.Handle(id, h); err != nil {
			return err
		}
	}
	return nil
}

func (a *MembersAPI) list(w http.ResponseWriter, r *http.Request) {
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
	rows, next, err := a.Svc.List(r.Context(), c, r.PathValue("teamId"), page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[memberBody]{Items: make([]memberBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
	for _, m := range rows {
		out.Items = append(out.Items, toMemberBody(m))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *MembersAPI) add(w http.ResponseWriter, r *http.Request) {
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
		Role               string `json:"role"`
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
	idem, err := w2Idem(r, "AddMember", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toMemberBody(v.(domain.Member)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Add(r.Context(), c, teamID, domain.NewMemberParams{Role: domain.TeamRole(req.Role),
		SubjectType: domain.SubjectType(req.SubjectType), UserID: req.UserID, Email: req.Email, GroupName: req.GroupName,
		IdentityProviderID: req.IdentityProviderID}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Member.Version)
	w.Header().Set("Location", "/v1/teams/"+teamID+"/members/"+res.Member.ID)
	WriteJSON(w, http.StatusCreated, toMemberBody(res.Member))
}

func (a *MembersAPI) update(w http.ResponseWriter, r *http.Request) {
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
		Role *string `json:"role"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Role == nil {
		WriteError(w, r, bad("role", "is required"))
		return
	}
	m, err := a.Svc.Update(r.Context(), c, r.PathValue("teamId"), r.PathValue("bindingId"), domain.TeamRole(*req.Role), expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, m.Version)
	WriteJSON(w, http.StatusOK, toMemberBody(m))
}

func (a *MembersAPI) remove(w http.ResponseWriter, r *http.Request) {
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
	if err := a.Svc.Remove(r.Context(), c, r.PathValue("teamId"), r.PathValue("bindingId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type apiKeyBody struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Role       string  `json:"role"`
	KeyPrefix  string  `json:"keyPrefix"`
	ExpiresAt  string  `json:"expiresAt"`
	LastUsedAt *string `json:"lastUsedAt"`
	CreatedAt  string  `json:"createdAt"`
	CreatedBy  string  `json:"createdBy"`
	// SecretKey is set only in the response that creates the key.
	SecretKey string `json:"secretKey,omitempty"`
}

func toAPIKeyBody(k domain.APIKey, secret string) apiKeyBody {
	return apiKeyBody{ID: k.ID, Name: k.Name, Role: string(k.Role), KeyPrefix: k.KeyPrefix, ExpiresAt: w2Time(k.ExpiresAt),
		LastUsedAt: w2TimePtr(k.LastUsedAt), CreatedAt: w2Time(k.CreatedAt), CreatedBy: k.CreatedBy, SecretKey: secret}
}

// Register binds the API key operations.
func (a *APIKeysAPI) Register(g *Registry) error {
	for id, h := range map[string]http.HandlerFunc{"ListApiKeys": a.list, "CreateApiKey": a.create, "DeleteApiKey": a.del} {
		if err := g.Handle(id, h); err != nil {
			return err
		}
	}
	return nil
}

func (a *APIKeysAPI) list(w http.ResponseWriter, r *http.Request) {
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
	rows, next, err := a.Svc.List(r.Context(), c, r.PathValue("teamId"), page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[apiKeyBody]{Items: make([]apiKeyBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
	for _, k := range rows {
		out.Items = append(out.Items, toAPIKeyBody(k, ""))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *APIKeysAPI) create(w http.ResponseWriter, r *http.Request) {
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
	// The stored copy of the response carries the secret only until the store strips it.
	idem, err := w2Idem(r, "CreateApiKey", teamID, raw, func(v any) (int, json.RawMessage, error) {
		cr := v.(apikeys.Created)
		return w2Render(http.StatusCreated, toAPIKeyBody(cr.Key, ""))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Create(r.Context(), c, teamID, domain.NewAPIKeyParams{Name: req.Name, Role: domain.TeamRole(req.Role), ExpiresAt: exp}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w.Header().Set("Location", "/v1/teams/"+teamID+"/api-keys/"+res.Key.ID)
	WriteJSON(w, http.StatusCreated, toAPIKeyBody(res.Key, res.Secret))
}

func (a *APIKeysAPI) del(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if err := a.Svc.Delete(r.Context(), c, r.PathValue("teamId"), r.PathValue("keyId")); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/accounts"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/identity"
)

// IdentityAPI serves the agent role link and identity operations (M4-A3) and the two account listings
// (M4-A2).
type IdentityAPI struct {
	Identity *identity.Service
	Accounts *accounts.Service
	Pages    PageTokens
}

// BindIdentity binds the handlers as the identity area.
func BindIdentity(a *IdentityAPI) { w2SetArea("identity", a.Register) }

// Register binds the operations.
func (a *IdentityAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListAgentRoleLinks": a.listLinks, "CreateAgentRoleLink": a.createLink, "GetAgentRoleLink": a.getLink,
		"DeleteAgentRoleLink": a.deleteLink, "VerifyAgentRoleLink": a.verifyLink,
		"ListAgentIdentities": a.listIdentities, "CreateAgentIdentity": a.createIdentity, "DeleteAgentIdentity": a.deleteIdentity,
		"ListAllowedAccounts": a.listAllowed, "ListOrgAccounts": a.listOrg,
	})
}

type roleLinkBody struct {
	ID                string         `json:"id"`
	AccountID         string         `json:"accountId"`
	RoleArn           string         `json:"roleArn"`
	ProfileName       string         `json:"profileName"`
	ExternalID        string         `json:"externalId"`
	MaxSessionSeconds int            `json:"maxSessionSeconds"`
	Status            string         `json:"status"`
	LastVerifiedAt    *string        `json:"lastVerifiedAt"`
	LastError         *string        `json:"lastError"`
	TrustPolicy       map[string]any `json:"trustPolicy,omitempty"`
	Version           int64          `json:"version"`
	CreatedAt         string         `json:"createdAt"`
	CreatedBy         string         `json:"createdBy"`
	UpdatedAt         string         `json:"updatedAt"`
}

func toRoleLinkBody(l domain.RoleLink, trust map[string]any) roleLinkBody {
	var lastErr *string
	if l.LastError != "" {
		e := l.LastError
		lastErr = &e
	}
	return roleLinkBody{ID: l.ID, AccountID: l.AccountID, RoleArn: l.RoleARN, ProfileName: l.ProfileName, ExternalID: l.ExternalID,
		MaxSessionSeconds: l.MaxSessionSeconds, Status: string(l.Status), LastVerifiedAt: w2TimePtr(l.LastVerifiedAt), LastError: lastErr,
		TrustPolicy: trust, Version: int64(l.Version), CreatedAt: w2Time(l.CreatedAt), CreatedBy: l.CreatedBy, UpdatedAt: w2Time(l.UpdatedAt)}
}

type agentIdentityBody struct {
	ID          string  `json:"id"`
	Kind        string  `json:"kind"`
	ExternalRef string  `json:"externalRef"`
	SecretID    *string `json:"secretId"`
	Status      string  `json:"status"`
	Version     int64   `json:"version"`
	CreatedAt   string  `json:"createdAt"`
	CreatedBy   string  `json:"createdBy"`
	UpdatedAt   string  `json:"updatedAt"`
}

func toAgentIdentityBody(i domain.AgentIdentity) agentIdentityBody {
	var sec *string
	if i.SecretID != "" {
		s := i.SecretID
		sec = &s
	}
	return agentIdentityBody{ID: i.ID, Kind: string(i.Kind), ExternalRef: i.ExternalRef, SecretID: sec, Status: string(i.Status),
		Version: int64(i.Version), CreatedAt: w2Time(i.CreatedAt), CreatedBy: i.CreatedBy, UpdatedAt: w2Time(i.UpdatedAt)}
}

type accountBody struct {
	AccountID string  `json:"accountId"`
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	SyncedAt  *string `json:"syncedAt,omitempty"`
}

func toAllowedAccountBody(a domain.OrgAccount) accountBody {
	return accountBody{AccountID: a.ID, Name: a.Name, Status: string(a.Status)}
}

func toOrgAccountBody(a domain.OrgAccount) accountBody {
	s := w2Time(a.SyncedAt)
	return accountBody{AccountID: a.ID, Name: a.Name, Status: string(a.Status), SyncedAt: &s}
}

func (a *IdentityAPI) listLinks(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.RoleLink, string, error) {
		return a.Identity.ListLinks(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), p)
	}, func(l domain.RoleLink) roleLinkBody { return toRoleLinkBody(l, nil) })
}

func (a *IdentityAPI) listIdentities(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.AgentIdentity, string, error) {
		return a.Identity.ListIdentities(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), p)
	}, toAgentIdentityBody)
}

func (a *IdentityAPI) listAllowed(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.OrgAccount, string, error) {
		return a.Accounts.ListAllowed(r.Context(), c, r.PathValue("teamId"), p)
	}, toAllowedAccountBody)
}

func (a *IdentityAPI) listOrg(w http.ResponseWriter, r *http.Request) {
	listWith(a.Pages, w, r, func(c acting.Caller, p acting.Page) ([]domain.OrgAccount, string, error) {
		return a.Accounts.ListOrg(r.Context(), c, p)
	}, toOrgAccountBody)
}

func (a *IdentityAPI) getLink(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.Identity.GetLink(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), r.PathValue("linkId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, v.Link.Version)
	WriteJSON(w, http.StatusOK, toRoleLinkBody(v.Link, v.TrustPolicy))
}

func (a *IdentityAPI) createLink(w http.ResponseWriter, r *http.Request) {
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
		RoleArn           string `json:"roleArn"`
		ProfileName       string `json:"profileName"`
		MaxSessionSeconds int    `json:"maxSessionSeconds"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	teamID, agentID := r.PathValue("teamId"), r.PathValue("agentId")
	idem, err := w2Idem(r, "CreateAgentRoleLink", teamID, raw, func(v any) (int, json.RawMessage, error) {
		x := v.(identity.View)
		return w2Render(http.StatusCreated, toRoleLinkBody(x.Link, x.TrustPolicy))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Identity.CreateLink(r.Context(), c, teamID, agentID, domain.NewRoleLinkParams{RoleARN: req.RoleArn,
		ProfileName: req.ProfileName, MaxSessionSeconds: req.MaxSessionSeconds}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Link.Version)
	w.Header().Set("Location", "/v1/teams/"+teamID+"/agents/"+agentID+"/role-links/"+res.Link.ID)
	WriteJSON(w, http.StatusCreated, toRoleLinkBody(res.Link, res.TrustPolicy))
}

func (a *IdentityAPI) verifyLink(w http.ResponseWriter, r *http.Request) {
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
	idem, err := w2Idem(r, "VerifyAgentRoleLink", teamID, raw, func(v any) (int, json.RawMessage, error) {
		x := v.(identity.View)
		return w2Render(http.StatusAccepted, toRoleLinkBody(x.Link, x.TrustPolicy))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Identity.VerifyLink(r.Context(), c, teamID, r.PathValue("agentId"), r.PathValue("linkId"), idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Link.Version)
	WriteJSON(w, http.StatusAccepted, toRoleLinkBody(res.Link, res.TrustPolicy))
}

func (a *IdentityAPI) deleteLink(w http.ResponseWriter, r *http.Request) {
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
	if err := a.Identity.DeleteLink(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), r.PathValue("linkId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *IdentityAPI) createIdentity(w http.ResponseWriter, r *http.Request) {
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
		Kind        string `json:"kind"`
		ExternalRef string `json:"externalRef"`
		SecretID    string `json:"secretId"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	teamID, agentID := r.PathValue("teamId"), r.PathValue("agentId")
	idem, err := w2Idem(r, "CreateAgentIdentity", teamID, raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toAgentIdentityBody(v.(domain.AgentIdentity)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Identity.CreateIdentity(r.Context(), c, teamID, agentID, domain.NewAgentIdentityParams{
		Kind: domain.AgentIdentityKind(req.Kind), ExternalRef: req.ExternalRef, SecretID: req.SecretID}, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Identity.Version)
	w.Header().Set("Location", "/v1/teams/"+teamID+"/agents/"+agentID+"/identities/"+res.Identity.ID)
	WriteJSON(w, http.StatusCreated, toAgentIdentityBody(res.Identity))
}

func (a *IdentityAPI) deleteIdentity(w http.ResponseWriter, r *http.Request) {
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
	if err := a.Identity.DeleteIdentity(r.Context(), c, r.PathValue("teamId"), r.PathValue("agentId"), r.PathValue("identityId"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

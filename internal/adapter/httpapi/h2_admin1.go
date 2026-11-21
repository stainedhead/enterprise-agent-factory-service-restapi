package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/audit"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/idp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/revisions"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/servicetags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/settings"
)

// Admin1API serves service administration I (M1-E): settings, identity providers and service
// tags. AuditAPI and RevisionsAPI serve the audit and revision reads.
type Admin1API struct {
	Settings    *settings.Service
	IdPs        *idp.Service
	ServiceTags *servicetags.Service
	Pages       PageTokens
}

// AuditAPI serves ListAuditEvents and ListAdminAuditEvents (M1-E4).
type AuditAPI struct {
	Svc   *audit.Service
	Pages PageTokens
}

// RevisionsAPI serves ListRevisions and GetRevision (M1-E5).
type RevisionsAPI struct {
	Svc   *revisions.Service
	Pages PageTokens
}

// BindAdmin1 binds settings, identity provider and service tag handlers.
func BindAdmin1(a *Admin1API) {
	w2SetArea("adminsettings", a.registerSettings)
	w2SetArea("identity", a.registerIdPs)
	w2SetArea("admin", a.registerTags)
}

// BindAudit binds the audit handlers as the audit area.
func BindAudit(a *AuditAPI) { w2SetArea("audit", a.Register) }

// BindRevisions binds the revision handlers as the revisions area.
func BindRevisions(a *RevisionsAPI) { w2SetArea("revisions", a.Register) }

func w2Bind(g *Registry, m map[string]http.HandlerFunc) error {
	for id, h := range m {
		if err := g.Handle(id, h); err != nil {
			return err
		}
	}
	return nil
}

// ---- settings ----

type settingBody struct {
	Key       string          `json:"key"`
	Value     json.RawMessage `json:"value"`
	Default   json.RawMessage `json:"default"`
	Type      string          `json:"type"`
	Allowed   string          `json:"allowed"`
	Min       *int64          `json:"min,omitempty"`
	Max       *int64          `json:"max,omitempty"`
	Nullable  bool            `json:"nullable,omitempty"`
	Pattern   string          `json:"pattern,omitempty"`
	Version   int64           `json:"version"`
	UpdatedAt *string         `json:"updatedAt"`
}

func toSettingBody(s settings.Setting) settingBody {
	return settingBody{Key: s.Spec.Key, Value: s.Value, Default: json.RawMessage(s.Spec.Default), Type: s.Spec.Type,
		Allowed: s.Spec.Allowed, Min: s.Spec.Min, Max: s.Spec.Max, Nullable: s.Spec.Nullable, Pattern: s.Spec.Pattern,
		Version: int64(s.Version), UpdatedAt: w2TimePtr(s.UpdatedAt)}
}

func (a *Admin1API) registerSettings(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{"GetSettings": a.getSettings, "PutSetting": a.putSetting})
}

func (a *Admin1API) getSettings(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rows, err := a.Settings.Get(r.Context(), c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[settingBody]{Items: make([]settingBody, 0, len(rows))}
	for _, s := range rows {
		out.Items = append(out.Items, toSettingBody(s))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *Admin1API) putSetting(w http.ResponseWriter, r *http.Request) {
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
		Value *json.RawMessage `json:"value"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Value == nil {
		WriteError(w, r, bad("value", "is required"))
		return
	}
	s, err := a.Settings.Put(r.Context(), c, r.PathValue("key"), *req.Value, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, s.Version)
	WriteJSON(w, http.StatusOK, toSettingBody(s))
}

// ---- identity providers ----

type teamRefBody struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	DisplayName string `json:"displayName"`
}

type idpBody struct {
	ID              string        `json:"id"`
	Kind            string        `json:"kind"`
	Name            string        `json:"name"`
	IssuerURL       string        `json:"issuerUrl"`
	ConsoleClientID string        `json:"consoleClientId"`
	CLIClientID     string        `json:"cliClientId"`
	ClientSecretARN *string       `json:"clientSecretArn"`
	GroupsClaim     string        `json:"groupsClaim"`
	IsDefault       bool          `json:"isDefault"`
	Enabled         bool          `json:"enabled"`
	Version         int64         `json:"version"`
	CreatedAt       string        `json:"createdAt"`
	CreatedBy       string        `json:"createdBy"`
	UpdatedAt       string        `json:"updatedAt"`
	Teams           []teamRefBody `json:"teams,omitempty"`
}

func toIdPBody(p domain.IdentityProvider) idpBody {
	b := idpBody{ID: p.ID, Kind: string(p.Kind), Name: p.Name, IssuerURL: p.IssuerURL, ConsoleClientID: p.ConsoleClientID,
		CLIClientID: p.CLIClientID, GroupsClaim: p.GroupsClaim, IsDefault: p.IsDefault, Enabled: p.Enabled,
		Version: int64(p.Version), CreatedAt: w2Time(p.CreatedAt), CreatedBy: p.CreatedBy, UpdatedAt: w2Time(p.UpdatedAt)}
	if p.ClientSecretARN != "" {
		b.ClientSecretARN = &p.ClientSecretARN
	}
	return b
}

func (a *Admin1API) registerIdPs(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListIdentityProviders": a.listIdPs, "CreateIdentityProvider": a.createIdP, "GetIdentityProvider": a.getIdP,
		"UpdateIdentityProvider": a.updateIdP, "DeleteIdentityProvider": a.deleteIdP,
	})
}

func (a *Admin1API) listIdPs(w http.ResponseWriter, r *http.Request) {
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
	rows, next, err := a.IdPs.List(r.Context(), c, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[idpBody]{Items: make([]idpBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
	for _, p := range rows {
		out.Items = append(out.Items, toIdPBody(p))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *Admin1API) getIdP(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	p, teams, err := a.IdPs.Get(r.Context(), c, r.PathValue("id"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	b := toIdPBody(p)
	b.Teams = make([]teamRefBody, 0, len(teams))
	for _, t := range teams {
		b.Teams = append(b.Teams, teamRefBody{ID: t.ID, Slug: t.Slug, DisplayName: t.DisplayName})
	}
	w2Etag(w, p.Version)
	WriteJSON(w, http.StatusOK, b)
}

func (a *Admin1API) createIdP(w http.ResponseWriter, r *http.Request) {
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
		Kind            string  `json:"kind"`
		Name            string  `json:"name"`
		IssuerURL       string  `json:"issuerUrl"`
		ConsoleClientID string  `json:"consoleClientId"`
		CLIClientID     string  `json:"cliClientId"`
		ClientSecretARN string  `json:"clientSecretArn"`
		GroupsClaim     *string `json:"groupsClaim"`
		IsDefault       bool    `json:"isDefault"`
		Enabled         *bool   `json:"enabled"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	in := domain.IdentityProvider{Kind: domain.IdPKind(req.Kind), Name: req.Name, IssuerURL: req.IssuerURL,
		ConsoleClientID: req.ConsoleClientID, CLIClientID: req.CLIClientID, ClientSecretARN: req.ClientSecretARN,
		GroupsClaim: "groups", IsDefault: req.IsDefault, Enabled: true}
	if req.GroupsClaim != nil {
		in.GroupsClaim = *req.GroupsClaim
	}
	if req.Enabled != nil {
		in.Enabled = *req.Enabled
	}
	idem, err := w2Idem(r, "CreateIdentityProvider", "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, toIdPBody(v.(domain.IdentityProvider)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	p, replay, err := a.IdPs.Create(r.Context(), c, in, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if replay != nil {
		w2Replay(w, replay)
		return
	}
	w2Etag(w, p.Version)
	w.Header().Set("Location", "/v1/admin/identity-providers/"+p.ID)
	WriteJSON(w, http.StatusCreated, toIdPBody(p))
}

func (a *Admin1API) updateIdP(w http.ResponseWriter, r *http.Request) {
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
		Name            *string            `json:"name"`
		ConsoleClientID *string            `json:"consoleClientId"`
		CLIClientID     *string            `json:"cliClientId"`
		ClientSecretARN w2Nullable[string] `json:"clientSecretArn"`
		GroupsClaim     *string            `json:"groupsClaim"`
		IsDefault       *bool              `json:"isDefault"`
		Enabled         *bool              `json:"enabled"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	patch := idp.Patch{Name: req.Name, ConsoleClientID: req.ConsoleClientID, CLIClientID: req.CLIClientID,
		GroupsClaim: req.GroupsClaim, IsDefault: req.IsDefault, Enabled: req.Enabled}
	if req.ClientSecretARN.Set {
		o := domain.OptionalString{Set: true}
		if !req.ClientSecretARN.Null {
			v := req.ClientSecretARN.Value
			o.Value = &v
		}
		patch.ClientSecretARN = &o
	}
	p, err := a.IdPs.Update(r.Context(), c, r.PathValue("id"), expect, patch)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, p.Version)
	WriteJSON(w, http.StatusOK, toIdPBody(p))
}

func (a *Admin1API) deleteIdP(w http.ResponseWriter, r *http.Request) {
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
	if err := a.IdPs.Delete(r.Context(), c, r.PathValue("id"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- service tags ----

type serviceTagBody struct {
	Key           string   `json:"key"`
	Mode          string   `json:"mode"`
	Value         *string  `json:"value"`
	DefaultValue  *string  `json:"defaultValue"`
	AllowedValues []string `json:"allowedValues"`
	Description   string   `json:"description"`
	Version       int64    `json:"version"`
	UpdatedAt     string   `json:"updatedAt"`
}

func toServiceTagBody(t domain.ServiceTag) serviceTagBody {
	return serviceTagBody{Key: t.Key, Mode: string(t.Mode), Value: t.Value, DefaultValue: t.DefaultValue,
		AllowedValues: t.AllowedValues, Description: t.Description, Version: int64(t.Version), UpdatedAt: w2Time(t.UpdatedAt)}
}

func (a *Admin1API) registerTags(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{
		"ListServiceTags": a.listTags, "PutServiceTag": a.putTag, "DeleteServiceTag": a.deleteTag,
	})
}

func (a *Admin1API) listTags(w http.ResponseWriter, r *http.Request) {
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
	rows, next, err := a.ServiceTags.List(r.Context(), c, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[serviceTagBody]{Items: make([]serviceTagBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
	for _, t := range rows {
		out.Items = append(out.Items, toServiceTagBody(t))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *Admin1API) putTag(w http.ResponseWriter, r *http.Request) {
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
		Mode          string   `json:"mode"`
		Value         *string  `json:"value"`
		DefaultValue  *string  `json:"defaultValue"`
		AllowedValues []string `json:"allowedValues"`
		Description   string   `json:"description"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := a.ServiceTags.Put(r.Context(), c, domain.ServiceTag{Key: r.PathValue("key"), Mode: domain.ServiceTagMode(req.Mode),
		Value: req.Value, DefaultValue: req.DefaultValue, AllowedValues: req.AllowedValues, Description: req.Description}, expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, t.Version)
	WriteJSON(w, http.StatusOK, toServiceTagBody(t))
}

func (a *Admin1API) deleteTag(w http.ResponseWriter, r *http.Request) {
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
	if err := a.ServiceTags.Delete(r.Context(), c, r.PathValue("key"), expect); err != nil {
		WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- audit ----

type auditBody struct {
	ID           int64           `json:"id"`
	OccurredAt   string          `json:"occurredAt"`
	ActorType    string          `json:"actorType"`
	ActorID      string          `json:"actorId"`
	Action       string          `json:"action"`
	Outcome      string          `json:"outcome"`
	ResourceKind *string         `json:"resourceKind"`
	ResourceID   *string         `json:"resourceId"`
	TeamID       *string         `json:"teamId"`
	RequestID    *string         `json:"requestId"`
	SourceIP     *string         `json:"sourceIp"`
	UserAgent    *string         `json:"userAgent"`
	WorkerPod    *string         `json:"workerPod"`
	Before       json.RawMessage `json:"before"`
	After        json.RawMessage `json:"after"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func rawOrNull(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("null")
	}
	return b
}

func toAuditBody(e audit.Event) auditBody {
	return auditBody{ID: e.ID, OccurredAt: w2Time(e.OccurredAt), ActorType: e.ActorType, ActorID: e.ActorID, Action: e.Action,
		Outcome: e.Outcome, ResourceKind: strPtr(e.ResourceKind), ResourceID: strPtr(e.ResourceID), TeamID: strPtr(e.TeamID),
		RequestID: strPtr(e.RequestID), SourceIP: strPtr(e.SourceIP), UserAgent: strPtr(e.UserAgent), WorkerPod: strPtr(e.WorkerPod),
		Before: rawOrNull(e.Before), After: rawOrNull(e.After)}
}

// Register binds the audit operations.
func (a *AuditAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{"ListAuditEvents": a.list, "ListAdminAuditEvents": a.list})
}

var auditOutcomes = map[string]bool{"allowed": true, "denied": true, "failed": true}

func (a *AuditAPI) list(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	q := r.URL.Query()
	f := audit.Filter{ActorID: q.Get("actorId"), Action: q.Get("action"), Outcome: q.Get("outcome"),
		ResourceKind: q.Get("resourceKind"), ResourceID: q.Get("resourceId"), TeamID: q.Get("teamId")}
	if f.Outcome != "" && !auditOutcomes[f.Outcome] {
		WriteError(w, r, bad("outcome", "must be allowed, denied or failed"))
		return
	}
	if v := q.Get("from"); v != "" {
		t, err := w2ParseTime("from", v)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, err := w2ParseTime("to", v)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		f.To = &t
	}
	page, err := w2Page(a.Pages, r, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rt, _ := RouteFrom(r)
	var (
		rows []audit.Event
		next string
	)
	if rt.OperationID == "ListAuditEvents" {
		rows, next, err = a.Svc.ListTeam(r.Context(), c, r.PathValue("teamId"), f, page)
	} else {
		rows, next, err = a.Svc.ListService(r.Context(), c, f, page)
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	token := w2Next(a.Pages, r, c, next)
	if WantsCSV(r) {
		cells := make([][]string, 0, len(rows))
		for _, e := range rows {
			cells = append(cells, []string{strconv.FormatInt(e.ID, 10), w2Time(e.OccurredAt), e.ActorType, e.ActorID, e.Action,
				e.Outcome, e.ResourceKind, e.ResourceID, e.TeamID, e.RequestID, e.SourceIP, e.UserAgent})
		}
		if err := WriteCSV(w, []string{"id", "occurredAt", "actorType", "actorId", "action", "outcome", "resourceKind",
			"resourceId", "teamId", "requestId", "sourceIp", "userAgent"}, cells, token); err != nil {
			WriteError(w, r, err)
		}
		return
	}
	out := w2ListBody[auditBody]{Items: make([]auditBody, 0, len(rows)), NextToken: token}
	for _, e := range rows {
		out.Items = append(out.Items, toAuditBody(e))
	}
	WriteJSON(w, http.StatusOK, out)
}

// ---- revisions ----

type revisionBody struct {
	Revision     int64           `json:"revision"`
	ResourceKind string          `json:"resourceKind"`
	ResourceID   string          `json:"resourceId"`
	Operation    string          `json:"operation"`
	Version      *int64          `json:"version"`
	ChangedBy    string          `json:"changedBy"`
	ChangedAt    string          `json:"changedAt"`
	Snapshot     json.RawMessage `json:"snapshot,omitempty"`
}

func toRevisionBody(v revisions.Revision) revisionBody {
	return revisionBody{Revision: v.Revision, ResourceKind: v.ResourceKind, ResourceID: v.ResourceID, Operation: v.Operation,
		Version: v.Version, ChangedBy: v.ChangedBy, ChangedAt: w2Time(v.ChangedAt), Snapshot: v.Snapshot}
}

// Register binds the revision operations.
func (a *RevisionsAPI) Register(g *Registry) error {
	return w2Bind(g, map[string]http.HandlerFunc{"ListRevisions": a.list, "GetRevision": a.get})
}

func (a *RevisionsAPI) list(w http.ResponseWriter, r *http.Request) {
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
	rows, next, err := a.Svc.List(r.Context(), c, r.PathValue("teamId"),
		revisions.Filter{ResourceKind: q.Get("resourceKind"), ResourceID: q.Get("resourceId")}, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[revisionBody]{Items: make([]revisionBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
	for _, v := range rows {
		out.Items = append(out.Items, toRevisionBody(v))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *RevisionsAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	n, err := w2Int64("rev", r.PathValue("rev"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	v, err := a.Svc.Get(r.Context(), c, r.PathValue("teamId"), n)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, toRevisionBody(v))
}

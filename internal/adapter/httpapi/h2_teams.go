package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
)

// TeamsAPI serves the team operations of API contract §4.2 (M1-B).
type TeamsAPI struct {
	Svc   *teams.Service
	Pages PageTokens
	Now   func() time.Time
}

// BindTeams binds the team handlers as the teams area.
func BindTeams(a *TeamsAPI) { w2SetArea("teams", a.Register) }

type teamBody struct {
	ID                       string        `json:"id"`
	Slug                     string        `json:"slug"`
	DisplayName              string        `json:"displayName"`
	Description              string        `json:"description,omitempty"`
	Kind                     string        `json:"kind"`
	State                    string        `json:"state"`
	Namespace                string        `json:"namespace"`
	CostCenter               string        `json:"costCenter,omitempty"`
	OwnerEmail               string        `json:"ownerEmail,omitempty"`
	MaxBudgetUsd             string        `json:"maxBudgetUsd"`
	CeilingOverrideUsd       *string       `json:"ceilingOverrideUsd"`
	CeilingOverrideExpiresAt *string       `json:"ceilingOverrideExpiresAt"`
	EffectiveCeilingUsd      string        `json:"effectiveCeilingUsd"`
	Quotas                   domain.Quotas `json:"quotas"`
	IsolationTier            string        `json:"isolationTier"`
	ViewerKubectl            bool          `json:"viewerKubectl"`
	ExpiresAt                *string       `json:"expiresAt,omitempty"`
	TeardownRule             string        `json:"teardownRule,omitempty"`
	TornDownAt               *string       `json:"tornDownAt,omitempty"`
	TeardownReason           string        `json:"teardownReason,omitempty"`
	IdentityProviderID       string        `json:"identityProviderId,omitempty"`
	AllowedAwsAccounts       []string      `json:"allowedAwsAccounts"`
	MayManageAwsAccounts     bool          `json:"mayManageAwsAccounts"`
	MayAddExternalSystems    bool          `json:"mayAddExternalSystems"`
	Version                  int64         `json:"version"`
	Generation               int64         `json:"generation"`
	CreatedAt                string        `json:"createdAt"`
	CreatedBy                string        `json:"createdBy"`
	UpdatedAt                string        `json:"updatedAt"`
	Status                   domain.Status `json:"status"`
}

func (a *TeamsAPI) teamBody(t domain.Team) teamBody {
	b := teamBody{
		ID: t.ID, Slug: string(t.Slug), DisplayName: t.DisplayName, Description: t.Description, Kind: string(t.Kind),
		State: string(t.State), Namespace: t.Namespace, CostCenter: t.CostCenter, OwnerEmail: t.OwnerEmail,
		MaxBudgetUsd: t.MaxBudget.String(), CeilingOverrideExpiresAt: w2TimePtr(t.CeilingOverrideExpiresAt),
		EffectiveCeilingUsd: t.EffectiveCeiling(a.Now()).String(), Quotas: t.Quotas, IsolationTier: string(t.IsolationTier),
		ViewerKubectl: t.ViewerKubectl, ExpiresAt: w2TimePtr(t.ExpiresAt), TeardownRule: string(t.TeardownRule),
		TornDownAt: w2TimePtr(t.TornDownAt), TeardownReason: string(t.TeardownReason), IdentityProviderID: t.IdentityProviderID,
		AllowedAwsAccounts: t.AllowedAwsAccounts, MayManageAwsAccounts: t.MayManageAwsAccounts,
		MayAddExternalSystems: t.MayAddExternalSystems, Version: int64(t.Version), Generation: int64(t.Generation),
		CreatedAt: w2Time(t.CreatedAt), CreatedBy: t.CreatedBy, UpdatedAt: w2Time(t.UpdatedAt), Status: t.Status,
	}
	if b.AllowedAwsAccounts == nil {
		b.AllowedAwsAccounts = []string{}
	}
	if t.CeilingOverride != nil {
		s := t.CeilingOverride.String()
		b.CeilingOverrideUsd = &s
	}
	if b.Status.Conditions == nil {
		b.Status.Conditions = []domain.Condition{}
	}
	return b
}

// Register binds the team operations.
func (a *TeamsAPI) Register(g *Registry) error {
	for id, h := range map[string]http.HandlerFunc{
		"ListTeams": a.list, "CreateTeam": a.create, "GetTeam": a.get, "UpdateTeam": a.update, "DeleteTeam": a.del,
		"SuspendTeam": a.suspend, "ResumeTeam": a.resume, "GetTeamStatus": a.status,
		"ListTeamModels": a.models, "PutTeamModels": a.putModels, "GetTeamTags": a.tags, "PutTeamTags": a.putTags,
	} {
		if err := g.Handle(id, h); err != nil {
			return err
		}
	}
	return nil
}

func (a *TeamsAPI) list(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	f := teams.ListFilter{State: domain.TeamState(r.URL.Query().Get("state")), Kind: domain.TeamKind(r.URL.Query().Get("kind"))}
	if f.State != "" && !f.State.Valid() {
		WriteError(w, r, bad("state", "must be active, suspended, deleting or deleted"))
		return
	}
	if f.Kind != "" && !f.Kind.Valid() {
		WriteError(w, r, bad("kind", "must be persistent or ephemeral"))
		return
	}
	page, err := w2Page(a.Pages, r, c)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	rows, next, err := a.Svc.List(r.Context(), c, f, page)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	out := w2ListBody[teamBody]{Items: make([]teamBody, 0, len(rows)), NextToken: w2Next(a.Pages, r, c, next)}
	for _, t := range rows {
		out.Items = append(out.Items, a.teamBody(t))
	}
	WriteJSON(w, http.StatusOK, out)
}

func (a *TeamsAPI) get(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := a.Svc.Get(r.Context(), c, r.PathValue("teamId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, t.Version)
	WriteJSON(w, http.StatusOK, a.teamBody(t))
}

func (a *TeamsAPI) status(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := a.Svc.Get(r.Context(), c, r.PathValue("teamId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	st := a.teamBody(t).Status
	WriteJSON(w, http.StatusOK, struct {
		ID         string        `json:"id"`
		Kind       string        `json:"kind"`
		Generation int64         `json:"generation"`
		Status     domain.Status `json:"status"`
	}{t.ID, teams.StatusKind, int64(t.Generation), st})
}

type createTeamRequest struct {
	Slug                  string             `json:"slug"`
	DisplayName           string             `json:"displayName"`
	Description           string             `json:"description"`
	Kind                  string             `json:"kind"`
	CostCenter            string             `json:"costCenter"`
	OwnerEmail            string             `json:"ownerEmail"`
	MaxBudgetUsd          *domain.Money      `json:"maxBudgetUsd"`
	Quotas                domain.Quotas      `json:"quotas"`
	IsolationTier         string             `json:"isolationTier"`
	ViewerKubectl         bool               `json:"viewerKubectl"`
	ExpiresAt             *string            `json:"expiresAt"`
	TeardownRule          string             `json:"teardownRule"`
	IdentityProviderID    string             `json:"identityProviderId"`
	AllowedAwsAccounts    []string           `json:"allowedAwsAccounts"`
	MayManageAwsAccounts  bool               `json:"mayManageAwsAccounts"`
	MayAddExternalSystems bool               `json:"mayAddExternalSystems"`
	FirstAdmin            *firstAdminRequest `json:"firstAdmin"`
	Models                []string           `json:"models"`
	Tags                  []tagValueBody     `json:"tags"`
	Budget                *teamBudgetRequest `json:"budget"`
}

type firstAdminRequest struct {
	SubjectType string `json:"subjectType"`
	UserID      string `json:"userId"`
	Email       string `json:"email"`
	GroupName   string `json:"groupName"`
}

type teamBudgetRequest struct {
	AmountUsd  *domain.Money `json:"amountUsd"`
	Period     string        `json:"period"`
	Thresholds []int         `json:"thresholds"`
}

type tagValueBody struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (a *TeamsAPI) create(w http.ResponseWriter, r *http.Request) {
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
	var req createTeamRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	var missing []domain.FieldError
	if req.MaxBudgetUsd == nil {
		missing = append(missing, domain.FieldError{Field: "maxBudgetUsd", Reason: "is required"})
	}
	if req.FirstAdmin == nil {
		missing = append(missing, domain.FieldError{Field: "firstAdmin", Reason: "is required"})
	}
	if len(missing) > 0 {
		WriteError(w, r, domain.Validation(missing...))
		return
	}
	in := teams.CreateTeamInput{
		Params: domain.NewTeamParams{
			Slug: req.Slug, DisplayName: req.DisplayName, Description: req.Description, Kind: domain.TeamKind(req.Kind),
			CostCenter: req.CostCenter, OwnerEmail: req.OwnerEmail, MaxBudget: *req.MaxBudgetUsd, Quotas: req.Quotas,
			IsolationTier: domain.IsolationTier(req.IsolationTier), ViewerKubectl: req.ViewerKubectl,
			TeardownRule: domain.TeardownRule(req.TeardownRule), IdentityProviderID: req.IdentityProviderID,
			AllowedAwsAccounts: req.AllowedAwsAccounts, MayManageAwsAccounts: req.MayManageAwsAccounts,
			MayAddExternalSystems: req.MayAddExternalSystems,
		},
		FirstAdmin: domain.NewMemberParams{SubjectType: domain.SubjectType(req.FirstAdmin.SubjectType), UserID: req.FirstAdmin.UserID,
			Email: req.FirstAdmin.Email, GroupName: req.FirstAdmin.GroupName},
		Models: req.Models,
	}
	if req.ExpiresAt != nil {
		t, err := w2ParseTime("expiresAt", *req.ExpiresAt)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		in.Params.ExpiresAt = &t
	}
	for _, t := range req.Tags {
		in.Tags = append(in.Tags, domain.TagValue{Key: t.Key, Value: t.Value})
	}
	if req.Budget != nil {
		if req.Budget.AmountUsd == nil {
			WriteError(w, r, bad("budget.amountUsd", "is required"))
			return
		}
		if req.Budget.Period != "" && req.Budget.Period != "lifetime" {
			WriteError(w, r, bad("budget.period", "must be lifetime"))
			return
		}
		in.Budget = &teams.BudgetInput{Amount: *req.Budget.AmountUsd, Thresholds: req.Budget.Thresholds}
	}
	idem, err := w2Idem(r, "CreateTeam", "", raw, func(v any) (int, json.RawMessage, error) {
		return w2Render(http.StatusCreated, a.teamBody(v.(domain.Team)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	res, err := a.Svc.Create(r.Context(), c, in, idem)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Team.Version)
	w.Header().Set("Location", "/v1/teams/"+res.Team.ID)
	WriteJSON(w, http.StatusCreated, a.teamBody(res.Team))
}

type updateTeamRequest struct {
	DisplayName              *string                  `json:"displayName"`
	Description              *string                  `json:"description"`
	OwnerEmail               *string                  `json:"ownerEmail"`
	CostCenter               *string                  `json:"costCenter"`
	IdentityProviderID       w2Nullable[string]       `json:"identityProviderId"`
	Kind                     *string                  `json:"kind"`
	MaxBudgetUsd             *domain.Money            `json:"maxBudgetUsd"`
	CeilingOverrideUsd       w2Nullable[domain.Money] `json:"ceilingOverrideUsd"`
	CeilingOverrideExpiresAt w2Nullable[string]       `json:"ceilingOverrideExpiresAt"`
	CeilingOverrideReason    string                   `json:"ceilingOverrideReason"`
	Quotas                   *domain.Quotas           `json:"quotas"`
	IsolationTier            *string                  `json:"isolationTier"`
	ViewerKubectl            *bool                    `json:"viewerKubectl"`
	MayManageAwsAccounts     *bool                    `json:"mayManageAwsAccounts"`
	MayAddExternalSystems    *bool                    `json:"mayAddExternalSystems"`
	AllowedAwsAccounts       *[]string                `json:"allowedAwsAccounts"`
}

func (q updateTeamRequest) patch() (domain.TeamPatch, error) {
	p := domain.TeamPatch{
		DisplayName: q.DisplayName, Description: q.Description, OwnerEmail: q.OwnerEmail, CostCenter: q.CostCenter,
		MaxBudget: q.MaxBudgetUsd, CeilingOverrideReason: q.CeilingOverrideReason, Quotas: q.Quotas,
		ViewerKubectl: q.ViewerKubectl, MayManageAwsAccounts: q.MayManageAwsAccounts,
		MayAddExternalSystems: q.MayAddExternalSystems, AllowedAwsAccounts: q.AllowedAwsAccounts,
	}
	if q.Kind != nil {
		k := domain.TeamKind(*q.Kind)
		p.Kind = &k
	}
	if q.IsolationTier != nil {
		t := domain.IsolationTier(*q.IsolationTier)
		p.IsolationTier = &t
	}
	if q.IdentityProviderID.Set {
		p.IdentityProviderID.Set = true
		if !q.IdentityProviderID.Null {
			v := q.IdentityProviderID.Value
			p.IdentityProviderID.Value = &v
		}
	}
	if q.CeilingOverrideUsd.Set {
		p.CeilingOverride.Set = true
		if !q.CeilingOverrideUsd.Null {
			v := q.CeilingOverrideUsd.Value
			p.CeilingOverride.Value = &v
		}
	}
	if q.CeilingOverrideExpiresAt.Set {
		p.CeilingOverrideExpiresAt.Set = true
		if !q.CeilingOverrideExpiresAt.Null {
			t, err := w2ParseTime("ceilingOverrideExpiresAt", q.CeilingOverrideExpiresAt.Value)
			if err != nil {
				return p, err
			}
			p.CeilingOverrideExpiresAt.Value = &t
		}
	}
	return p, nil
}

func (a *TeamsAPI) update(w http.ResponseWriter, r *http.Request) {
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
	var req updateTeamRequest
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	patch, err := req.patch()
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := a.Svc.Update(r.Context(), c, r.PathValue("teamId"), expect, patch)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, t.Version)
	WriteJSON(w, http.StatusOK, a.teamBody(t))
}

func (a *TeamsAPI) del(w http.ResponseWriter, r *http.Request) {
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
	t, err := a.Svc.Delete(r.Context(), c, r.PathValue("teamId"), expect)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, t.Version)
	WriteJSON(w, http.StatusAccepted, a.teamBody(t))
}

// transition serves SuspendTeam and ResumeTeam.
func (a *TeamsAPI) transition(w http.ResponseWriter, r *http.Request, op string) {
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
		return w2Render(http.StatusOK, a.teamBody(v.(domain.Team)))
	})
	if err != nil {
		WriteError(w, r, err)
		return
	}
	var res teams.Result
	if op == "SuspendTeam" {
		res, err = a.Svc.Suspend(r.Context(), c, teamID, idem)
	} else {
		res, err = a.Svc.Resume(r.Context(), c, teamID, idem)
	}
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if res.Replay != nil {
		w2Replay(w, res.Replay)
		return
	}
	w2Etag(w, res.Team.Version)
	WriteJSON(w, http.StatusOK, a.teamBody(res.Team))
}

func (a *TeamsAPI) suspend(w http.ResponseWriter, r *http.Request) { a.transition(w, r, "SuspendTeam") }
func (a *TeamsAPI) resume(w http.ResponseWriter, r *http.Request)  { a.transition(w, r, "ResumeTeam") }

type modelBody struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	DisplayName     string `json:"displayName"`
	Enabled         bool   `json:"enabled"`
	ContextWindow   int    `json:"contextWindow"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
	Version         int64  `json:"version"`
	UpdatedAt       string `json:"updatedAt"`
}

func modelBodies(ms []teams.ModelInfo) []modelBody {
	out := make([]modelBody, 0, len(ms))
	for _, m := range ms {
		out = append(out, modelBody{ID: m.ID, Provider: m.Provider, DisplayName: m.DisplayName, Enabled: m.Enabled,
			ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens, Version: m.Version, UpdatedAt: w2Time(m.UpdatedAt)})
	}
	return out
}

func (a *TeamsAPI) models(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	ms, v, err := a.Svc.Models(r.Context(), c, r.PathValue("teamId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, v)
	WriteJSON(w, http.StatusOK, w2ListBody[modelBody]{Items: modelBodies(ms)})
}

func (a *TeamsAPI) putModels(w http.ResponseWriter, r *http.Request) {
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
		ModelIDs *[]string `json:"modelIds"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.ModelIDs == nil {
		WriteError(w, r, bad("modelIds", "is required"))
		return
	}
	ms, v, err := a.Svc.PutModels(r.Context(), c, r.PathValue("teamId"), expect, *req.ModelIDs)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, v)
	WriteJSON(w, http.StatusOK, w2ListBody[modelBody]{Items: modelBodies(ms)})
}

type teamTagBody struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Source string `json:"source"`
}

type teamTagsBody struct {
	Tags                       []teamTagBody `json:"tags"`
	RequestMetadataDroppedKeys []string      `json:"requestMetadataDroppedKeys"`
	Version                    int64         `json:"version"`
}

func tagsBody(t teams.TagsResult) teamTagsBody {
	out := teamTagsBody{Tags: make([]teamTagBody, 0, len(t.Tags.Tags)), RequestMetadataDroppedKeys: t.Dropped, Version: int64(t.Version)}
	if out.RequestMetadataDroppedKeys == nil {
		out.RequestMetadataDroppedKeys = []string{}
	}
	for _, g := range t.Tags.Tags {
		out.Tags = append(out.Tags, teamTagBody{Key: g.Key, Value: g.Value, Source: string(g.Source)})
	}
	return out
}

func (a *TeamsAPI) tags(w http.ResponseWriter, r *http.Request) {
	c, err := w2Caller(r)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	t, err := a.Svc.Tags(r.Context(), c, r.PathValue("teamId"))
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, t.Version)
	WriteJSON(w, http.StatusOK, tagsBody(t))
}

func (a *TeamsAPI) putTags(w http.ResponseWriter, r *http.Request) {
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
		Tags *[]tagValueBody `json:"tags"`
	}
	if err := Decode(r, &req); err != nil {
		WriteError(w, r, err)
		return
	}
	if req.Tags == nil {
		WriteError(w, r, bad("tags", "is required"))
		return
	}
	values := make([]domain.TagValue, 0, len(*req.Tags))
	for _, t := range *req.Tags {
		values = append(values, domain.TagValue{Key: t.Key, Value: t.Value})
	}
	t, err := a.Svc.PutTags(r.Context(), c, r.PathValue("teamId"), expect, values)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	w2Etag(w, t.Version)
	WriteJSON(w, http.StatusOK, tagsBody(t))
}

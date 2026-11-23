package teams

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const (
	kindTeam = "teams"
	// StatusKind is the resource_kind the reconciler writes team status under.
	StatusKind = "team"
)

// BootProvisioner writes the orchestration agent's boot secrets to Secrets Manager and records them on
// the agent, in the request's transaction; Compensate deletes what a failed request wrote. The agents
// use case implements it.
type BootProvisioner interface {
	CreateBootSecrets(ctx context.Context, tx port.Tx, c acting.Caller, teamID, agentID string, tool domain.CollabTool, idem *acting.Idem, created *[]string) error
	Compensate(ctx context.Context, arns []string)
}

// Service is the team use case set.
type Service struct {
	acting.Deps
	Repo     Repository
	Settings port.SettingsReader
	// Boot provisions the orchestration agent's boot secrets (ORC-2). Required to create a team whose
	// tool has boot secrets (Paperclip).
	Boot BootProvisioner
}

// CreateTeamInput is a CreateTeam request.
type CreateTeamInput struct {
	Params     domain.NewTeamParams
	FirstAdmin domain.NewMemberParams
	// Models is the explicit model list; with no ModelAccess it means selected (CD-30).
	Models        []string
	Tags          []domain.TagValue
	Budget        *BudgetInput
	Orchestration *OrchestrationInput
}

// Result is a team with the replayed response when an idempotent call hit the store.
type Result struct {
	Team   domain.Team
	Replay *port.StoredResponse
}

func teamAudit(t domain.Team) map[string]any {
	m := map[string]any{
		"slug": string(t.Slug), "displayName": t.DisplayName, "kind": t.Kind, "state": t.State,
		"costCenter": t.CostCenter, "ownerEmail": t.OwnerEmail, "maxBudgetUsd": t.MaxBudget.String(),
		"isolationTier": t.IsolationTier, "viewerKubectl": t.ViewerKubectl, "quotas": t.Quotas,
		"mayManageAwsAccounts": t.MayManageAwsAccounts, "mayAddExternalSystems": t.MayAddExternalSystems,
		"maxCollaborationExposure": string(t.MaxCollaborationExposure), "modelAccess": string(t.ModelAccess),
		"allowedAwsAccounts": t.AllowedAwsAccounts, "identityProviderId": t.IdentityProviderID,
		"generation": t.Generation,
	}
	if t.CeilingOverride != nil {
		m["ceilingOverrideUsd"] = t.CeilingOverride.String()
		m["ceilingOverrideExpiresAt"] = t.CeilingOverrideExpiresAt
	}
	return m
}

func (s *Service) ephemeralMaxDays(ctx context.Context) (int, error) {
	raw, err := s.Settings.Setting(ctx, domain.SettingTeamsEphemeralMaxDays)
	if err != nil {
		return 0, err
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Service) stringSetting(ctx context.Context, key string) (string, error) {
	raw, err := s.Settings.Setting(ctx, key)
	if err != nil {
		return "", err
	}
	var v *string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", domain.Internal(err)
	}
	if v == nil {
		return "", nil
	}
	return *v, nil
}

// toolEntry is a tool's entry of orchestration.tools as CreateTeam reads it.
type toolEntry struct {
	Enabled         bool    `json:"enabled"`
	Default         bool    `json:"default"`
	TemplateID      *string `json:"templateId"`
	ImageRepository *string `json:"imageRepository"`
}

func (s *Service) tools(ctx context.Context) (map[string]toolEntry, error) {
	raw, err := s.Settings.Setting(ctx, domain.SettingOrchestrationTools)
	if err != nil {
		return nil, err
	}
	out := map[string]toolEntry{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, domain.Internal(err)
	}
	return out, nil
}

// resolveTool picks the requested tool or the service default and checks it is enabled and its runtime
// set up (orchestration_runtime_unset is a Conflict, ORC-2).
func (s *Service) resolveTool(ctx context.Context, want domain.CollabTool) (domain.CollabTool, error) {
	tools, err := s.tools(ctx)
	if err != nil {
		return "", err
	}
	tool := want
	if tool == "" {
		for name, e := range tools {
			if e.Default {
				tool = domain.CollabTool(name)
			}
		}
	}
	e, ok := tools[string(tool)]
	switch {
	case !tool.Valid():
		return "", domain.Validation(domain.FieldError{Field: "orchestration.tool", Reason: "must be paperclip or buzz"})
	case !ok || !e.Enabled:
		return "", domain.Validation(domain.FieldError{Field: "orchestration.tool", Reason: domain.ReasonToolNotEnabled})
	case e.TemplateID == nil || e.ImageRepository == nil:
		return "", domain.Conflict(domain.ReasonOrchestrationRuntimeUnset, "the runtime of tool "+string(tool)+" is not set up", nil)
	}
	return tool, nil
}

// orchestrationFields checks the explicit levels against the limit and the tool's profile. Defaults are
// never refused for being above the limit, because the database lowers them (ORC-26).
func orchestrationFields(in *OrchestrationInput, tool domain.CollabTool, limit domain.Exposure) error {
	if in == nil {
		return nil
	}
	var fe []domain.FieldError
	check := func(field string, surface domain.Surface, e domain.Exposure) {
		switch {
		case e == "":
		case !e.Valid():
			fe = append(fe, domain.FieldError{Field: field, Reason: "must be off, private or public"})
		case e != domain.ExposureOff && !tool.Supports(surface):
			fe = append(fe, domain.FieldError{Field: field, Reason: domain.ReasonSurfaceNotSupported})
		case e.Rank() > limit.Rank():
			fe = append(fe, domain.FieldError{Field: field, Reason: domain.ReasonExposureAboveLimit})
		}
	}
	check("orchestration.webUiExposure", domain.SurfaceWebUI, in.WebUIExposure)
	check("orchestration.webApiExposure", domain.SurfaceWebAPI, in.WebAPIExposure)
	if in.RegistrationMode != "" && !tool.SupportsRegistration(in.RegistrationMode) {
		fe = append(fe, domain.FieldError{Field: "orchestration.registrationMode", Reason: domain.ReasonRegistrationNotSupported})
	}
	if in.VolumeSizeGiB != 0 && (in.VolumeSizeGiB < 10 || in.VolumeSizeGiB > 1000) {
		fe = append(fe, domain.FieldError{Field: "orchestration.volume.sizeGiB", Reason: "must be 10 to 1000"})
	}
	if len(fe) > 0 {
		return domain.Validation(fe...)
	}
	return nil
}

// orchestrationModels resolves the orchestration agent's models against the team just created. The
// fields are reported under orchestration.; a missing default is no_model_available (ORC-26).
func orchestrationModels(in *OrchestrationInput, sets domain.ModelSets) (domain.AgentModels, error) {
	var mi *domain.ModelsInput
	legacy := ""
	if in != nil {
		mi, legacy = in.Models, in.ModelID
	}
	m, err := domain.ResolveNewModels(mi, legacy, sets)
	if err != nil {
		fe := domain.FieldErrors(err)
		for i := range fe {
			if fe[i].Field == "models.defaultModelId" && fe[i].Reason == "required" {
				fe[i].Reason = domain.ReasonNoModelAvailable
			}
			fe[i].Field = "orchestration." + fe[i].Field
		}
		return domain.AgentModels{}, domain.Validation(fe...)
	}
	return m, nil
}

// teamModelSelection resolves a new team's model access and list (MOD-2, CD-30): modelAccess defaults to
// models.default_team_access, a list without an access means selected, and a selected team with no list
// starts with models.default_team_models.
func (s *Service) teamModelSelection(ctx context.Context, p *domain.NewTeamParams, ids []string) ([]string, error) {
	access := p.ModelAccess
	if access == "" && len(ids) > 0 {
		access = domain.ModelAccessSelected
	}
	if access == "" {
		v, err := s.stringSetting(ctx, domain.SettingModelsDefaultTeamAccess)
		if err != nil {
			return nil, err
		}
		access = domain.ModelAccess(v)
		if !access.Valid() {
			access = domain.ModelAccessAll
		}
	}
	if !access.Valid() {
		return nil, domain.Validation(domain.FieldError{Field: "modelAccess", Reason: "must be all or selected"})
	}
	p.ModelAccess = access
	switch access {
	case domain.ModelAccessAll:
		if len(ids) > 0 {
			return nil, domain.Validation(domain.FieldError{Field: "models", Reason: domain.ReasonModelIDsNotAllowedForAll})
		}
		return nil, nil
	default:
		if len(ids) == 0 {
			raw, err := s.Settings.Setting(ctx, domain.SettingModelsDefaultTeamModels)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(raw, &ids); err != nil {
				return nil, domain.Internal(err)
			}
		}
		return ids, nil
	}
}

// Create implements CreateTeam (RA-US-3, ORC-2): one transaction writes the team, its models, tags,
// lifetime budget (ephemeral), the first team-admin binding, the orchestration agent with its boot
// secrets, audit records and the events. A Secrets Manager failure is a 503 and nothing is created.
func (s *Service) Create(ctx context.Context, c acting.Caller, in CreateTeamInput, idem *acting.Idem) (Result, error) {
	if d := authz.Authorize(c.Principal, authz.TeamsCreate, authz.Resource{Scope: authz.ScopeService}); !d.Allowed() {
		return Result{}, s.Denied(ctx, c, "CreateTeam", "", authz.TeamsCreate)
	}
	var res Result
	var created []string
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		created = nil
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		days, err := s.ephemeralMaxDays(ctx)
		if err != nil {
			return err
		}
		id, err := s.IDs.NewID(domain.PrefixTeam)
		if err != nil {
			return err
		}
		params := in.Params
		defLimit, err := s.stringSetting(ctx, domain.SettingOrchestrationDefaultLimit)
		if err != nil {
			return err
		}
		params.MaxCollaborationExposure = ptrTo(params.LimitFromParams(domain.Exposure(defLimit)))
		modelIDs, err := s.teamModelSelection(ctx, &params, in.Models)
		if err != nil {
			return err
		}
		t, err := domain.NewTeam(id.String(), params, s.Now(), days)
		if err != nil {
			return err
		}
		if err := s.checkBudget(t, in.Budget); err != nil {
			return err
		}
		admin := in.FirstAdmin
		admin.Role = domain.TeamRoleTeamAdmin
		if err := admin.Validate(); err != nil {
			return prefixFields(err, "firstAdmin.")
		}
		uid, ok, err := s.resolveAdmin(ctx, tx, &admin)
		if err != nil {
			return err
		}
		if !ok {
			return domain.Validation(domain.FieldError{Field: "firstAdmin", Reason: "no such user; they must sign in once first"})
		}
		_ = uid
		if err := s.checkModels(ctx, tx, modelIDs); err != nil {
			return err
		}
		tool, err := s.resolveTool(ctx, orchTool(in.Orchestration))
		if err != nil {
			return err
		}
		if err := orchestrationFields(in.Orchestration, tool, t.MaxCollaborationExposure); err != nil {
			return err
		}
		svcTags, err := s.Repo.ServiceTags(ctx, tx)
		if err != nil {
			return err
		}
		if err := domain.ValidateTeamTags(svcTags, in.Tags); err != nil {
			return err
		}
		if err := s.Repo.Insert(ctx, tx, t); err != nil {
			return err
		}
		if in.Budget != nil {
			bid, err := s.IDs.NewID(domain.PrefixBudget)
			if err != nil {
				return err
			}
			b := *in.Budget
			b.ID = bid.String()
			if err := s.Repo.InsertBudget(ctx, tx, t.ID, b); err != nil {
				return err
			}
		}
		if err := s.Repo.ReplaceModels(ctx, tx, t.ID, modelIDs); err != nil {
			return err
		}
		if err := s.Repo.ReplaceTeamTags(ctx, tx, t.ID, in.Tags); err != nil {
			return err
		}
		rb, err := s.IDs.NewID(domain.PrefixBinding)
		if err != nil {
			return err
		}
		if err := s.Repo.InsertAdminBinding(ctx, tx, rb.String(), t.ID, admin); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateTeam", kindTeam, t.ID, t.ID, nil, teamAudit(t))); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventTeamCreated, t.ID, kindTeam, t.ID,
			map[string]any{"slug": string(t.Slug), "kind": string(t.Kind)}); err != nil {
			return err
		}
		if err := s.createOrchestration(ctx, tx, c, t, tool, in.Orchestration, idem, &created); err != nil {
			return err
		}
		got, err := s.Repo.Get(ctx, tx, t.ID, false)
		if err != nil {
			return err
		}
		res.Team = got.WithDeletingStatus()
		return idem.Complete(ctx, s.Idem, tx, res.Team)
	})
	if err != nil && s.Boot != nil {
		s.Boot.Compensate(ctx, created)
	}
	return res, err
}

func ptrTo[T any](v T) *T { return &v }

func orchTool(in *OrchestrationInput) domain.CollabTool {
	if in == nil {
		return ""
	}
	return in.Tool
}

// createOrchestration creates the team's orchestration agent (afs.create_orchestration_agent, which also
// writes its audit record and its outbox events agent.created and orchestration.created, i.e.
// domain.EventOrchestrationCreated) and then its boot secrets.
func (s *Service) createOrchestration(ctx context.Context, tx port.Tx, c acting.Caller, t domain.Team, tool domain.CollabTool, in *OrchestrationInput, idem *acting.Idem, created *[]string) error {
	sets, err := s.Repo.ModelSets(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	models, err := orchestrationModels(in, sets)
	if err != nil {
		return err
	}
	aid, err := s.IDs.NewID(domain.PrefixAgent)
	if err != nil {
		return err
	}
	oa := OrchestrationAgent{TeamID: t.ID, AgentID: aid.String(), Name: domain.OrchestratorName, Tool: tool, Models: models}
	if in != nil {
		oa.WebUIExposure, oa.WebAPIExposure, oa.RegistrationMode = in.WebUIExposure, in.WebAPIExposure, in.RegistrationMode
		oa.Resources, oa.VolumeSizeGiB, oa.VolumeOnDelete = in.Resources, in.VolumeSizeGiB, in.VolumeOnDelete
	}
	if err := s.Repo.InsertOrchestration(ctx, tx, oa); err != nil {
		return err
	}
	if len(tool.BootSecrets()) > 0 {
		if s.Boot == nil {
			return domain.Unavailable("Secrets Manager", errors.New("teams: no Secrets Manager writer for the boot secrets"))
		}
		return s.Boot.CreateBootSecrets(ctx, tx, c, t.ID, oa.AgentID, tool, idem, created)
	}
	return nil
}

func (s *Service) resolveAdmin(ctx context.Context, tx port.Tx, m *domain.NewMemberParams) (string, bool, error) {
	if m.SubjectType == domain.SubjectTypeGroup {
		return "", true, nil
	}
	id, ok, err := s.Repo.ResolveUser(ctx, tx, m.UserID, m.Email)
	if ok {
		m.UserID, m.Email = id, ""
	}
	return id, ok, err
}

func prefixFields(err error, prefix string) error {
	var de *domain.Error
	if !errors.As(err, &de) {
		return err
	}
	fs := slices.Clone(de.Fields)
	for i := range fs {
		fs[i].Field = prefix + fs[i].Field
	}
	return domain.Validation(fs...)
}

func (s *Service) checkModels(ctx context.Context, tx port.Tx, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	found, err := s.Repo.ModelsByID(ctx, tx, ids)
	if err != nil {
		return err
	}
	var fe []domain.FieldError
	seen := map[string]bool{}
	for _, id := range ids {
		m, ok := found[id]
		switch {
		case seen[id]:
			fe = append(fe, domain.FieldError{Field: "models", Reason: id + " is listed twice"})
		case !ok:
			fe = append(fe, domain.FieldError{Field: "models", Reason: id + " is not in the model catalog"})
		case !m.Enabled:
			fe = append(fe, domain.FieldError{Field: "models", Reason: domain.ReasonModelNotEnabledForService + ": " + id})
		}
		seen[id] = true
	}
	if len(fe) > 0 {
		return domain.Validation(fe...)
	}
	return nil
}

// visible hides a deleted team from everyone but service admins (RA-US-16.3).
func visible(c acting.Caller, t domain.Team) bool {
	return t.State != domain.TeamStateDeleted || c.Principal.ServiceAdmin
}

// List implements ListTeams: the caller's teams (all for a service admin), in id order.
func (s *Service) List(ctx context.Context, c acting.Caller, f ListFilter, p acting.Page) ([]domain.Team, string, error) {
	p = p.Normalized()
	if f.State != domain.TeamStateDeleted || !c.Principal.ServiceAdmin {
		f.HideDeleted = true
	}
	var rows []domain.Team
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, f, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(t domain.Team) string { return t.ID })
	for i := range page {
		page[i] = page[i].WithDeletingStatus()
	}
	return page, next, nil
}

// Get implements GetTeam and GetTeamStatus.
func (s *Service) Get(ctx context.Context, c acting.Caller, teamID string) (domain.Team, error) {
	var t domain.Team
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		t, err = s.Repo.Get(ctx, tx, teamID, false)
		return err
	})
	if err != nil {
		return domain.Team{}, err
	}
	if !visible(c, t) {
		return domain.Team{}, domain.NotFound("team")
	}
	return t.WithDeletingStatus(), nil
}

// Flags returns the team's guardrail flags and state for the authorization and suspended-team
// middleware; ok is false when the team does not exist or is invisible to the caller.
func (s *Service) Flags(ctx context.Context, c acting.Caller, teamID string) (authz.TeamFlags, domain.TeamState, bool, error) {
	t, err := s.Get(ctx, c, teamID)
	if domain.KindOf(err) == domain.KindNotFound {
		return authz.TeamFlags{}, "", false, nil
	}
	if err != nil {
		return authz.TeamFlags{}, "", false, err
	}
	return authz.TeamFlags{MayManageAwsAccounts: t.MayManageAwsAccounts, ViewerKubectl: t.ViewerKubectl,
		MayAddExternalSystems: t.MayAddExternalSystems}, t.State, true, nil
}

// errDenied carries a refusal out of a transaction so the denied audit is written after rollback.
type errDenied struct{ perm authz.Permission }

func (e errDenied) Error() string { return "denied " + string(e.perm) }

func (s *Service) finishDenied(ctx context.Context, c acting.Caller, action, teamID string, err error) error {
	var d errDenied
	if errors.As(err, &d) {
		return s.Denied(ctx, c, action, teamID, d.perm)
	}
	return err
}

// Update implements UpdateTeam (RA-US-6.1, RA-US-6.4): teams:write fields, guardrail fields need
// teams:set-guardrails, allowedAwsAccounts needs teams:set-aws-accounts (a conditional grant).
func (s *Service) Update(ctx context.Context, c acting.Caller, teamID string, expect domain.Version, p domain.TeamPatch) (domain.Team, error) {
	var out domain.Team
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		t, err := s.Repo.Get(ctx, tx, teamID, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(t.Version, expect); err != nil {
			return err
		}
		if t.State == domain.TeamStateDeleting || t.State == domain.TeamStateDeleted {
			return domain.Conflict("invalid_state", "a team in state "+string(t.State)+" cannot be changed", nil)
		}
		res := authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID, Flags: authz.TeamFlags{
			MayManageAwsAccounts: t.MayManageAwsAccounts, ViewerKubectl: t.ViewerKubectl, MayAddExternalSystems: t.MayAddExternalSystems}}
		if len(p.GuardrailFields()) > 0 && !authz.Authorize(c.Principal, authz.TeamsSetGuardrails, res).Allowed() {
			return errDenied{authz.TeamsSetGuardrails}
		}
		if p.SetsAllowedAccounts() && !authz.Authorize(c.Principal, authz.TeamsSetAWSAccounts, res).Allowed() {
			return errDenied{authz.TeamsSetAWSAccounts}
		}
		budget, err := s.Repo.BudgetAmount(ctx, tx, teamID)
		if err != nil {
			return err
		}
		n, err := t.Apply(p, budget, s.Now())
		if err != nil {
			return err
		}
		after := teamAudit(n)
		if p.SetsCeilingOverride() {
			after["ceilingOverrideReason"] = p.CeilingOverrideReason
		}
		if n, err = s.Repo.Update(ctx, tx, n, expect); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("UpdateTeam", kindTeam, teamID, teamID, teamAudit(t), after)); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventTeamUpdated, teamID, kindTeam, teamID,
			map[string]any{"fields": changedFields(p)}); err != nil {
			return err
		}
		if p.SetsCeilingOverride() {
			if err := s.Emit(ctx, tx, c, domain.EventTeamCeilingOverrideSet, teamID, kindTeam, teamID,
				map[string]any{"ceilingOverrideUsd": n.CeilingOverride.String(), "expiresAt": n.CeilingOverrideExpiresAt}); err != nil {
				return err
			}
		}
		out = n
		return nil
	})
	if err != nil {
		return domain.Team{}, s.finishDenied(ctx, c, "UpdateTeam", teamID, err)
	}
	return out, nil
}

func changedFields(p domain.TeamPatch) []string {
	f := p.GuardrailFields()
	add := func(set bool, n string) {
		if set {
			f = append(f, n)
		}
	}
	add(p.DisplayName != nil, "displayName")
	add(p.Description != nil, "description")
	add(p.OwnerEmail != nil, "ownerEmail")
	add(p.CostCenter != nil, "costCenter")
	add(p.IdentityProviderID.Set, "identityProviderId")
	add(p.SetsAllowedAccounts(), "allowedAwsAccounts")
	slices.Sort(f)
	return slices.Compact(f)
}

type lifecycle struct {
	action string
	event  domain.EventType
	apply  func(domain.Team) (domain.Team, error)
}

func (s *Service) transition(ctx context.Context, c acting.Caller, teamID string, expect *domain.Version, l lifecycle, idem *acting.Idem) (Result, error) {
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		t, err := s.Repo.Get(ctx, tx, teamID, true)
		if err != nil {
			return err
		}
		if t.State == domain.TeamStateDeleted && !c.Principal.ServiceAdmin {
			return domain.NotFound("team")
		}
		if expect != nil {
			if err := domain.CheckVersion(t.Version, *expect); err != nil {
				return err
			}
		}
		n, err := l.apply(t)
		if err != nil {
			return err
		}
		v := t.Version
		if n, err = s.Repo.Update(ctx, tx, n, v); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(l.action, kindTeam, teamID, teamID,
			map[string]any{"state": t.State}, map[string]any{"state": n.State})); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, l.event, teamID, kindTeam, teamID, map[string]any{"state": string(n.State)}); err != nil {
			return err
		}
		res.Team = n.WithDeletingStatus()
		return idem.Complete(ctx, s.Idem, tx, res.Team)
	})
	return res, err
}

// Suspend implements SuspendTeam (RA-US-16.2).
func (s *Service) Suspend(ctx context.Context, c acting.Caller, teamID string, idem *acting.Idem) (Result, error) {
	return s.transition(ctx, c, teamID, nil, lifecycle{"SuspendTeam", domain.EventTeamSuspended,
		func(t domain.Team) (domain.Team, error) { return t.Suspend(s.Now()) }}, idem)
}

// Resume implements ResumeTeam.
func (s *Service) Resume(ctx context.Context, c acting.Caller, teamID string, idem *acting.Idem) (Result, error) {
	return s.transition(ctx, c, teamID, nil, lifecycle{"ResumeTeam", domain.EventTeamResumed,
		func(t domain.Team) (domain.Team, error) { return t.Resume(s.Now()) }}, idem)
}

// Delete implements DeleteTeam: the team becomes deleting (status Deleting) and the reconciler
// finalizes it.
func (s *Service) Delete(ctx context.Context, c acting.Caller, teamID string, expect domain.Version) (domain.Team, error) {
	r, err := s.transition(ctx, c, teamID, &expect, lifecycle{"DeleteTeam", domain.EventTeamDeleted,
		func(t domain.Team) (domain.Team, error) { return t.MarkDeleting(s.Now()) }}, nil)
	return r.Team, err
}

// Models implements ListTeamModels: the team's access, stored selection and the models it may use now.
func (s *Service) Models(ctx context.Context, c acting.Caller, teamID string) (TeamModels, error) {
	if _, err := s.Get(ctx, c, teamID); err != nil {
		return TeamModels{}, err
	}
	var m TeamModels
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		m, err = s.Repo.Models(ctx, tx, teamID)
		return err
	})
	return m, err
}

// PutModels implements PutTeamModels (teams:set-guardrails, service admins only, OR-D16). expect is nil
// when If-Match is absent (PUT is an upsert, CD-2). A body with only modelIds is read as selected
// (CD-30). Narrowing never edits an agent's own selection (MOD-6).
func (s *Service) PutModels(ctx context.Context, c acting.Caller, teamID string, expect *domain.Version, access domain.ModelAccess, ids []string) (TeamModels, error) {
	var out TeamModels
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		t, err := s.Repo.Get(ctx, tx, teamID, true)
		if err != nil {
			return err
		}
		if !authz.Authorize(c.Principal, authz.TeamsSetGuardrails, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}).Allowed() {
			return errDenied{authz.TeamsSetGuardrails}
		}
		old, err := s.Repo.Models(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(t.Version, *expect); err != nil {
				return err
			}
		}
		enabled, err := s.Repo.ServiceModelIDs(ctx, tx)
		if err != nil {
			return err
		}
		want, err := domain.ValidateTeamModelAccess(access, ids, enabled)
		if err != nil {
			return err
		}
		if err := s.Repo.ReplaceModels(ctx, tx, teamID, want.ModelIDs); err != nil {
			return err
		}
		if want.Access != old.Access {
			if err := s.Repo.SetModelAccess(ctx, tx, teamID, want.Access); err != nil {
				return err
			}
		}
		before := map[string]any{"access": string(old.Access), "modelIds": nonNil(old.ModelIDs)}
		after := map[string]any{"access": string(want.Access), "modelIds": nonNil(want.ModelIDs)}
		if err := s.Audit.Write(ctx, tx, c.Record("PutTeamModels", "team_models", teamID, teamID, before, after)); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventTeamModelsChanged, teamID, kindTeam, t.ID,
			map[string]any{"teamId": teamID, "from": before, "to": after}); err != nil {
			return err
		}
		out, err = s.Repo.Models(ctx, tx, teamID)
		return err
	})
	if err != nil {
		return TeamModels{}, s.finishDenied(ctx, c, "PutTeamModels", teamID, err)
	}
	return out, nil
}

func nonNil(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// TagsResult is a team's effective tags with the Bedrock requestMetadata keys that did not fit.
type TagsResult struct {
	Tags    domain.TagSet
	Dropped []string
	Version domain.Version
}

func (s *Service) tagsResult(ctx context.Context, tx port.Tx, teamID string) (TagsResult, error) {
	svc, err := s.Repo.ServiceTags(ctx, tx)
	if err != nil {
		return TagsResult{}, err
	}
	own, v, err := s.Repo.TeamTags(ctx, tx, teamID)
	if err != nil {
		return TagsResult{}, err
	}
	set := domain.EffectiveTagSet(teamID, svc, own)
	// One pair is kept for the agent's (or pool's) id, which the gateway adds after afs-team-id, so these are
	// the keys that no call can send.
	_, dropped := set.BedrockMetadata(domain.MaxRequestMetadataPairs - 1)
	return TagsResult{Tags: set, Dropped: dropped, Version: v}, nil
}

// Tags implements GetTeamTags.
func (s *Service) Tags(ctx context.Context, c acting.Caller, teamID string) (TagsResult, error) {
	if _, err := s.Get(ctx, c, teamID); err != nil {
		return TagsResult{}, err
	}
	var r TagsResult
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		r, err = s.tagsResult(ctx, tx, teamID)
		return err
	})
	return r, err
}

// PutTags implements PutTeamTags: the team's own keys, replaced as a set (TAG-1).
func (s *Service) PutTags(ctx context.Context, c acting.Caller, teamID string, expect *domain.Version, values []domain.TagValue) (TagsResult, error) {
	var r TagsResult
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if _, err := s.Repo.Get(ctx, tx, teamID, true); err != nil {
			return err
		}
		svc, err := s.Repo.ServiceTags(ctx, tx)
		if err != nil {
			return err
		}
		old, cur, err := s.Repo.TeamTags(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur, *expect); err != nil {
				return err
			}
		}
		if err := domain.ValidateTeamTags(svc, values); err != nil {
			return err
		}
		if err := s.Repo.ReplaceTeamTags(ctx, tx, teamID, values); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("PutTeamTags", "team_tags", teamID, teamID, old, values)); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventTeamTagsChanged, teamID, kindTeam, teamID, map[string]any{"count": len(values)}); err != nil {
			return err
		}
		r, err = s.tagsResult(ctx, tx, teamID)
		return err
	})
	return r, err
}

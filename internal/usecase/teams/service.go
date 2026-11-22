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

// Service is the team use case set.
type Service struct {
	acting.Deps
	Repo     Repository
	Settings port.SettingsReader
}

// CreateTeamInput is a CreateTeam request.
type CreateTeamInput struct {
	Params     domain.NewTeamParams
	FirstAdmin domain.NewMemberParams
	Models     []string
	Tags       []domain.TagValue
	Budget     *BudgetInput
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
		"mayManageAwsAccounts": t.MayManageAwsAccounts, "mayAddExternalSystems": t.MayAddExternalSystems, "mayExposeCollaboration": t.MayExposeCollaboration,
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

// Create implements CreateTeam (RA-US-3): one transaction writes the team, its models, tags,
// lifetime budget (ephemeral), the first team-admin binding, one audit record and team.created.
func (s *Service) Create(ctx context.Context, c acting.Caller, in CreateTeamInput, idem *acting.Idem) (Result, error) {
	if d := authz.Authorize(c.Principal, authz.TeamsCreate, authz.Resource{Scope: authz.ScopeService}); !d.Allowed() {
		return Result{}, s.Denied(ctx, c, "CreateTeam", "", authz.TeamsCreate)
	}
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
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
		t, err := domain.NewTeam(id.String(), in.Params, s.Now(), days)
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
		if err := s.checkModels(ctx, tx, in.Models); err != nil {
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
		if err := s.Repo.ReplaceModels(ctx, tx, t.ID, in.Models); err != nil {
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
		got, err := s.Repo.Get(ctx, tx, t.ID, false)
		if err != nil {
			return err
		}
		res.Team = got.WithDeletingStatus()
		return idem.Complete(ctx, s.Idem, tx, res.Team)
	})
	return res, err
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
			fe = append(fe, domain.FieldError{Field: "models", Reason: id + " is disabled"})
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

// Models implements ListTeamModels.
func (s *Service) Models(ctx context.Context, c acting.Caller, teamID string) ([]ModelInfo, domain.Version, error) {
	if _, err := s.Get(ctx, c, teamID); err != nil {
		return nil, 0, err
	}
	var (
		ms []ModelInfo
		v  domain.Version
	)
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		ms, v, err = s.Repo.Models(ctx, tx, teamID)
		return err
	})
	return ms, v, err
}

// PutModels implements PutTeamModels (teams:set-guardrails). expect is nil when If-Match is absent
// (PUT is an upsert, CD-2).
func (s *Service) PutModels(ctx context.Context, c acting.Caller, teamID string, expect *domain.Version, ids []string) ([]ModelInfo, domain.Version, error) {
	var (
		ms []ModelInfo
		v  domain.Version
	)
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		t, err := s.Repo.Get(ctx, tx, teamID, true)
		if err != nil {
			return err
		}
		if !authz.Authorize(c.Principal, authz.TeamsSetGuardrails, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}).Allowed() {
			return errDenied{authz.TeamsSetGuardrails}
		}
		old, cur, err := s.Repo.Models(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur, *expect); err != nil {
				return err
			}
		}
		if err := s.checkModels(ctx, tx, ids); err != nil {
			return err
		}
		if err := s.Repo.ReplaceModels(ctx, tx, teamID, ids); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("PutTeamModels", "team_models", teamID, teamID,
			map[string]any{"modelIds": modelIDs(old)}, map[string]any{"modelIds": ids})); err != nil {
			return err
		}
		if err := s.Emit(ctx, tx, c, domain.EventTeamUpdated, teamID, kindTeam, t.ID, map[string]any{"fields": []string{"models"}}); err != nil {
			return err
		}
		ms, v, err = s.Repo.Models(ctx, tx, teamID)
		return err
	})
	if err != nil {
		return nil, 0, s.finishDenied(ctx, c, "PutTeamModels", teamID, err)
	}
	return ms, v, nil
}

func modelIDs(ms []ModelInfo) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
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

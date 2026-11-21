package teams_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams/teamstest"
)

var now = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

type rig struct {
	svc    *teams.Service
	repo   *teamstest.Repo
	audit  *fakes.Audit
	outbox *fakes.Outbox
	tx     *fakes.TxRunner
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{repo: teamstest.New(), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}, tx: &fakes.TxRunner{}}
	r.svc = &teams.Service{
		Deps: acting.Deps{Tx: r.tx, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
			Clock: &fakes.Clock{T: now}, IDs: &fakes.IDGen{}},
		Repo: r.repo, Settings: &fakes.Settings{},
	}
	r.repo.Users["boss@example.com"] = "usr_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	r.repo.Catalog["m1"] = teams.ModelInfo{ID: "m1", Enabled: true}
	r.repo.Catalog["off"] = teams.ModelInfo{ID: "off"}
	return r
}

var (
	svcAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_admin", Type: authz.PrincipalUser, ServiceAdmin: true}, RequestID: "req_1"}
)

func teamCaller(team string, role authz.Role) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_t", Type: authz.PrincipalUser,
		Bindings: []authz.Binding{{Role: role, TeamID: team}}}, RequestID: "req_2"}
}

func createInput() teams.CreateTeamInput {
	return teams.CreateTeamInput{
		Params: domain.NewTeamParams{Slug: "payments", DisplayName: "Payments", Kind: domain.TeamKindPersistent,
			CostCenter: "CC1", OwnerEmail: "o@example.com", MaxBudget: domain.MustMoney("1000")},
		FirstAdmin: domain.NewMemberParams{SubjectType: domain.SubjectTypeUser, Email: "boss@example.com"},
		Models:     []string{"m1"},
	}
}

func (r *rig) create(t *testing.T) domain.Team {
	t.Helper()
	res, err := r.svc.Create(t.Context(), svcAdmin, createInput(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Team
}

func TestCreateWritesRowAuditOutbox(t *testing.T) {
	r := newRig(t)
	tm := r.create(t)
	if tm.State != domain.TeamStateActive || tm.Status.Phase != domain.PhasePending || tm.Version != 1 {
		t.Fatalf("team = %+v", tm)
	}
	if len(r.audit.Entries) != 1 || r.audit.Entries[0].Record.Action != "CreateTeam" {
		t.Fatalf("audit = %+v", r.audit.Entries)
	}
	if len(r.outbox.Events) != 1 || r.outbox.Events[0].Type != domain.EventTeamCreated {
		t.Fatalf("outbox = %+v", r.outbox.Events)
	}
	if len(r.repo.Bindings) != 1 || len(r.repo.TeamModels[tm.ID]) != 1 {
		t.Fatal("binding or models missing")
	}
	for _, b := range r.repo.Bindings {
		if b.Role != domain.TeamRoleTeamAdmin || b.UserID == "" {
			t.Fatalf("binding = %+v", b)
		}
	}
}

func TestCreateDeniedForTeamRoles(t *testing.T) {
	for _, role := range []authz.Role{authz.RoleTeamAdmin, authz.RoleTeamViewer} {
		r := newRig(t)
		_, err := r.svc.Create(t.Context(), teamCaller("team_x", role), createInput(), nil)
		if domain.KindOf(err) != domain.KindAccessDenied {
			t.Fatalf("%s: %v", role, err)
		}
		if len(r.audit.Entries) != 1 || !r.audit.Entries[0].Own || r.audit.Entries[0].Record.Outcome != port.OutcomeDenied {
			t.Fatalf("%s: audit = %+v", role, r.audit.Entries)
		}
		if len(r.repo.Teams) != 0 || len(r.outbox.Events) != 0 {
			t.Fatal("denied create wrote something")
		}
	}
}

func TestCreateFailuresWriteNothing(t *testing.T) {
	cases := map[string]func(*rig, *teams.CreateTeamInput){
		"unknown admin":  func(r *rig, in *teams.CreateTeamInput) { in.FirstAdmin.Email = "nobody@example.com" },
		"unknown model":  func(r *rig, in *teams.CreateTeamInput) { in.Models = []string{"zzz"} },
		"disabled model": func(r *rig, in *teams.CreateTeamInput) { in.Models = []string{"off"} },
		"bad slug":       func(r *rig, in *teams.CreateTeamInput) { in.Params.Slug = "X" },
		"bad admin":      func(r *rig, in *teams.CreateTeamInput) { in.FirstAdmin = domain.NewMemberParams{} },
		"persistent budget": func(r *rig, in *teams.CreateTeamInput) {
			in.Budget = &teams.BudgetInput{Amount: domain.MustMoney("5")}
		},
		"duplicate slug": func(r *rig, in *teams.CreateTeamInput) {
			if _, err := r.svc.Create(t.Context(), svcAdmin, createInput(), nil); err != nil {
				t.Fatal(err)
			}
			r.audit.Entries, r.outbox.Events = nil, nil
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			in := createInput()
			mut(r, &in)
			before := len(r.repo.Teams)
			_, err := r.svc.Create(t.Context(), svcAdmin, in, nil)
			if err == nil {
				t.Fatal("want error")
			}
			if len(r.repo.Teams) != before || len(r.audit.Entries) != 0 || len(r.outbox.Events) != 0 || len(r.repo.Bindings) > before {
				t.Fatalf("wrote on failure: teams=%d audit=%d outbox=%d", len(r.repo.Teams), len(r.audit.Entries), len(r.outbox.Events))
			}
		})
	}
}

func TestCreateMissingRequiredTags(t *testing.T) {
	r := newRig(t)
	r.repo.Service = []domain.ServiceTag{{Key: "cost-owner", Mode: domain.ServiceTagModeRequired}}
	_, err := r.svc.Create(t.Context(), svcAdmin, createInput(), nil)
	var de *domain.Error
	if !asDomain(err, &de) || de.Kind != domain.KindValidation {
		t.Fatalf("err = %v", err)
	}
	if got := de.Details["missingTags"]; got == nil {
		t.Fatalf("details = %+v", de.Details)
	}
	in := createInput()
	in.Tags = []domain.TagValue{{Key: "cost-owner", Value: "fin"}}
	if _, err := r.svc.Create(t.Context(), svcAdmin, in, nil); err != nil {
		t.Fatal(err)
	}
}

func TestCreateEphemeral(t *testing.T) {
	r := newRig(t)
	in := createInput()
	exp := now.Add(48 * time.Hour)
	in.Params.Kind, in.Params.ExpiresAt, in.Params.TeardownRule = domain.TeamKindEphemeral, &exp, domain.TeardownRuleAtExpiry
	if _, err := r.svc.Create(t.Context(), svcAdmin, in, nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("no budget: %v", err)
	}
	in.Budget = &teams.BudgetInput{Amount: domain.MustMoney("50")}
	res, err := r.svc.Create(t.Context(), svcAdmin, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.repo.Budgets[res.Team.ID]; got.String() != "50.00" {
		t.Fatalf("budget = %v", got)
	}
	in.Params.Slug = "second"
	in.Budget = &teams.BudgetInput{Amount: domain.MustMoney("5000")}
	if _, err := r.svc.Create(t.Context(), svcAdmin, in, nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("over ceiling: %v", err)
	}
}

func idemFor(key string, in teams.CreateTeamInput) *acting.Idem {
	b, _ := json.Marshal(in.Params.Slug)
	return &acting.Idem{
		Key: port.IdempotencyKey{Key: key, Operation: "CreateTeam", RequestSHA256: [32]byte(sum(b))},
		Render: func(v any) (int, json.RawMessage, error) {
			b, err := json.Marshal(map[string]any{"slug": v.(domain.Team).Slug})
			return 201, b, err
		},
	}
}

func TestCreateIdempotency(t *testing.T) {
	r := newRig(t)
	in := createInput()
	first, err := r.svc.Create(t.Context(), svcAdmin, in, idemFor("k1", in))
	if err != nil || first.Replay != nil {
		t.Fatal(err, first.Replay)
	}
	again, err := r.svc.Create(t.Context(), svcAdmin, in, idemFor("k1", in))
	if err != nil || again.Replay == nil || again.Replay.Status != 201 {
		t.Fatalf("replay = %+v, %v", again.Replay, err)
	}
	if len(r.repo.Teams) != 1 || len(r.audit.Entries) != 1 || len(r.outbox.Events) != 1 {
		t.Fatal("replay wrote")
	}
	other := in
	other.Params.Slug = "other"
	_, err = r.svc.Create(t.Context(), svcAdmin, other, idemFor("k1", other))
	var de *domain.Error
	if !asDomain(err, &de) || de.Reason != domain.ReasonIdempotencyMismatch {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateGuardrails(t *testing.T) {
	r := newRig(t)
	tm := r.create(t)
	admin := teamCaller(tm.ID, authz.RoleTeamAdmin)
	name := "Payments 2"
	// teams:write field by a team-admin works.
	got, err := r.svc.Update(t.Context(), admin, tm.ID, 1, domain.TeamPatch{DisplayName: &name})
	if err != nil || got.DisplayName != name || got.Version != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	// every guardrail field by a team-admin: 403 plus one denied audit, nothing changed.
	yes, tier, max := true, domain.IsolationTierDedicated, domain.MustMoney("5")
	for field, p := range map[string]domain.TeamPatch{
		"viewerKubectl": {ViewerKubectl: &yes}, "isolationTier": {IsolationTier: &tier}, "maxBudgetUsd": {MaxBudget: &max},
		"quotas": {Quotas: &domain.Quotas{Pods: 3}}, "mayAdd": {MayAddExternalSystems: &yes},
	} {
		r.audit.Entries, r.outbox.Events = nil, nil
		_, err := r.svc.Update(t.Context(), admin, tm.ID, 2, p)
		if domain.KindOf(err) != domain.KindAccessDenied {
			t.Fatalf("%s: %v", field, err)
		}
		if len(r.audit.Entries) != 1 || !r.audit.Entries[0].Own || len(r.outbox.Events) != 0 {
			t.Fatalf("%s: audit=%+v", field, r.audit.Entries)
		}
	}
	if r.repo.Teams[tm.ID].Version != 2 {
		t.Fatal("denied update changed the team")
	}
	// a service admin may.
	got, err = r.svc.Update(t.Context(), svcAdmin, tm.ID, 2, domain.TeamPatch{ViewerKubectl: &yes, IsolationTier: &tier})
	if err != nil || !got.ViewerKubectl || got.IsolationTier != tier {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestUpdateAllowedAccountsConditionalGrant(t *testing.T) {
	r := newRig(t)
	tm := r.create(t)
	admin := teamCaller(tm.ID, authz.RoleTeamAdmin)
	accts := []string{"111122223333"}
	if _, err := r.svc.Update(t.Context(), admin, tm.ID, 1, domain.TeamPatch{AllowedAwsAccounts: &accts}); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("flag off: %v", err)
	}
	yes := true
	if _, err := r.svc.Update(t.Context(), svcAdmin, tm.ID, 1, domain.TeamPatch{MayManageAwsAccounts: &yes}); err != nil {
		t.Fatal(err)
	}
	got, err := r.svc.Update(t.Context(), admin, tm.ID, 2, domain.TeamPatch{AllowedAwsAccounts: &accts})
	if err != nil || len(got.AllowedAwsAccounts) != 1 {
		t.Fatalf("flag on: %+v %v", got, err)
	}
	// the team-admin still cannot flip the flag itself.
	no := false
	if _, err := r.svc.Update(t.Context(), admin, tm.ID, 3, domain.TeamPatch{MayManageAwsAccounts: &no}); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("flip: %v", err)
	}
}

func TestUpdateRulesAndVersions(t *testing.T) {
	r := newRig(t)
	tm := r.create(t)
	eph := domain.TeamKindEphemeral
	if _, err := r.svc.Update(t.Context(), svcAdmin, tm.ID, 1, domain.TeamPatch{Kind: &eph}); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("kind: %v", err)
	}
	r.repo.Budgets[tm.ID] = domain.MustMoney("800")
	low := domain.MustMoney("500")
	if _, err := r.svc.Update(t.Context(), svcAdmin, tm.ID, 1, domain.TeamPatch{MaxBudget: &low}); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("below budget: %v", err)
	}
	if _, err := r.svc.Update(t.Context(), svcAdmin, tm.ID, 7, domain.TeamPatch{}); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale: %v", err)
	}
	if _, err := r.svc.Update(t.Context(), svcAdmin, "team_nope", 1, domain.TeamPatch{}); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("missing: %v", err)
	}
}

func TestCeilingOverrideAudit(t *testing.T) {
	r := newRig(t)
	tm := r.create(t)
	amt := domain.MustMoney("2000")
	exp := now.Add(24 * time.Hour)
	p := domain.TeamPatch{CeilingOverride: domain.OptionalMoney{Set: true, Value: &amt},
		CeilingOverrideExpiresAt: domain.OptionalTime{Set: true, Value: &exp}}
	if _, err := r.svc.Update(t.Context(), svcAdmin, tm.ID, 1, p); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("no reason: %v", err)
	}
	p.CeilingOverrideReason = "quarter-end load test"
	got, err := r.svc.Update(t.Context(), svcAdmin, tm.ID, 1, p)
	if err != nil {
		t.Fatal(err)
	}
	if got.EffectiveCeiling(now).String() != "2000.00" {
		t.Fatalf("ceiling %v", got.EffectiveCeiling(now))
	}
	types := map[domain.EventType]bool{}
	for _, e := range r.outbox.Events {
		types[e.Type] = true
	}
	if !types[domain.EventTeamCeilingOverrideSet] || !types[domain.EventTeamUpdated] {
		t.Fatalf("events = %+v", r.outbox.Events)
	}
	last := r.audit.Entries[len(r.audit.Entries)-1].Record
	after, _ := last.After.(map[string]any)
	if after["ceilingOverrideReason"] != "quarter-end load test" {
		t.Fatalf("reason missing from audit: %+v", after)
	}
	for _, e := range r.outbox.Events {
		if b, _ := json.Marshal(e); contains(string(b), "quarter-end") {
			t.Fatal("reason leaked into an event")
		}
	}
}

func TestSuspendResumeDelete(t *testing.T) {
	r := newRig(t)
	tm := r.create(t)
	res, err := r.svc.Suspend(t.Context(), teamCaller(tm.ID, authz.RoleTeamAdmin), tm.ID, nil)
	if err != nil || res.Team.State != domain.TeamStateSuspended {
		t.Fatalf("%+v %v", res.Team, err)
	}
	if _, err := r.svc.Suspend(t.Context(), svcAdmin, tm.ID, nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("double suspend: %v", err)
	}
	if res, err = r.svc.Resume(t.Context(), svcAdmin, tm.ID, nil); err != nil || res.Team.State != domain.TeamStateActive {
		t.Fatalf("%+v %v", res.Team, err)
	}
	if _, err := r.svc.Resume(t.Context(), svcAdmin, tm.ID, nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("resume active: %v", err)
	}
	cur := r.repo.Teams[tm.ID].Version
	if _, err := r.svc.Delete(t.Context(), svcAdmin, tm.ID, cur+5); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale delete: %v", err)
	}
	d, err := r.svc.Delete(t.Context(), svcAdmin, tm.ID, cur)
	if err != nil || d.State != domain.TeamStateDeleting || d.Status.Phase != domain.PhaseDeleting {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := r.svc.Delete(t.Context(), svcAdmin, tm.ID, d.Version); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("double delete: %v", err)
	}
	var types []domain.EventType
	for _, e := range r.outbox.Events {
		types = append(types, e.Type)
	}
	want := []domain.EventType{domain.EventTeamCreated, domain.EventTeamSuspended, domain.EventTeamResumed, domain.EventTeamDeleted}
	if len(types) != len(want) {
		t.Fatalf("events = %v", types)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("events = %v", types)
		}
	}
}

func TestReadsAndVisibility(t *testing.T) {
	r := newRig(t)
	a := r.create(t)
	in := createInput()
	in.Params.Slug = "other"
	b, err := r.svc.Create(t.Context(), svcAdmin, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	viewer := teamCaller(a.ID, authz.RoleTeamViewer)
	list, _, err := r.svc.List(t.Context(), viewer, teams.ListFilter{}, acting.Page{})
	if err != nil || len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("viewer list = %+v %v", list, err)
	}
	if _, err := r.svc.Get(t.Context(), viewer, b.Team.ID); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("cross-team get: %v", err)
	}
	all, next, err := r.svc.List(t.Context(), svcAdmin, teams.ListFilter{}, acting.Page{Limit: 1})
	if err != nil || len(all) != 1 || next == "" {
		t.Fatalf("page = %+v next=%q %v", all, next, err)
	}
	rest, next2, _ := r.svc.List(t.Context(), svcAdmin, teams.ListFilter{}, acting.Page{Limit: 1, Cursor: next})
	if len(rest) != 1 || next2 != "" {
		t.Fatalf("page 2 = %+v %q", rest, next2)
	}
	// deleted teams: readable by service admins only.
	cur := r.repo.Teams[a.ID].Version
	if _, err := r.svc.Delete(t.Context(), svcAdmin, a.ID, cur); err != nil {
		t.Fatal(err)
	}
	tm := r.repo.Teams[a.ID]
	tm.State = domain.TeamStateDeleted
	r.repo.Teams[a.ID] = tm
	if got, err := r.svc.Get(t.Context(), svcAdmin, a.ID); err != nil || got.State != domain.TeamStateDeleted {
		t.Fatalf("admin get deleted: %+v %v", got, err)
	}
	if _, err := r.svc.Get(t.Context(), viewer, a.ID); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("viewer get deleted: %v", err)
	}
	if l, _, _ := r.svc.List(t.Context(), svcAdmin, teams.ListFilter{}, acting.Page{}); len(l) != 1 {
		t.Fatalf("default list shows deleted: %d", len(l))
	}
	if l, _, _ := r.svc.List(t.Context(), svcAdmin, teams.ListFilter{State: domain.TeamStateDeleted}, acting.Page{}); len(l) != 1 {
		t.Fatalf("deleted filter: %d", len(l))
	}
	flags, state, ok, err := r.svc.Flags(t.Context(), svcAdmin, b.Team.ID)
	if err != nil || !ok || state != domain.TeamStateActive || flags.ViewerKubectl {
		t.Fatalf("flags %+v %v %v %v", flags, state, ok, err)
	}
	if _, _, ok, _ := r.svc.Flags(t.Context(), viewer, "team_none"); ok {
		t.Fatal("flags of unknown team")
	}
}

func TestModels(t *testing.T) {
	r := newRig(t)
	tm := r.create(t)
	ms, v, err := r.svc.Models(t.Context(), teamCaller(tm.ID, authz.RoleTeamViewer), tm.ID)
	if err != nil || len(ms) != 1 {
		t.Fatalf("%v %v", ms, err)
	}
	admin := teamCaller(tm.ID, authz.RoleTeamAdmin)
	if _, _, err := r.svc.PutModels(t.Context(), admin, tm.ID, nil, []string{"m1"}); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("team-admin put: %v", err)
	}
	if len(r.audit.Entries) != 2 || !r.audit.Entries[1].Own {
		t.Fatalf("denied audit: %+v", r.audit.Entries)
	}
	stale := v + 4
	if _, _, err := r.svc.PutModels(t.Context(), svcAdmin, tm.ID, &stale, []string{"m1"}); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale: %v", err)
	}
	if _, _, err := r.svc.PutModels(t.Context(), svcAdmin, tm.ID, &v, []string{"off"}); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("disabled: %v", err)
	}
	got, nv, err := r.svc.PutModels(t.Context(), svcAdmin, tm.ID, &v, []string{})
	if err != nil || len(got) != 0 || nv <= v {
		t.Fatalf("%v %v %v", got, nv, err)
	}
}

func TestTags(t *testing.T) {
	r := newRig(t)
	r.repo.Service = []domain.ServiceTag{
		{Key: "env", Mode: domain.ServiceTagModeFixed, Value: ptr("prod")},
		{Key: "owner", Mode: domain.ServiceTagModeRequired, DefaultValue: ptr("unknown")},
	}
	tm := r.create(t)
	admin := teamCaller(tm.ID, authz.RoleTeamAdmin)
	res, err := r.svc.Tags(t.Context(), admin, tm.ID)
	if err != nil || len(res.Tags.Tags) != 3 || res.Tags.Tags[2].Source != domain.TagSourceServiceDefault {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := r.svc.PutTags(t.Context(), admin, tm.ID, nil, []domain.TagValue{{Key: "env", Value: "dev"}}); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("fixed key: %v", err)
	}
	r.outbox.Events = nil
	res, err = r.svc.PutTags(t.Context(), admin, tm.ID, nil, []domain.TagValue{{Key: "owner", Value: "fin"}, {Key: "app", Value: "pay"}})
	if err != nil || len(r.outbox.Events) != 1 || r.outbox.Events[0].Type != domain.EventTeamTagsChanged {
		t.Fatalf("%+v %v %v", res, err, r.outbox.Events)
	}
	if _, err := r.svc.PutTags(t.Context(), admin, tm.ID, ptr(domain.Version(99)), nil); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale: %v", err)
	}
	// 20 own keys: effective set has 1 system + 1 fixed + 1 required + 19 = 23; > 16 pairs drops keys.
	var many []domain.TagValue
	for i := range 20 {
		many = append(many, domain.TagValue{Key: "k" + string(rune('a'+i)), Value: "v"})
	}
	res, err = r.svc.PutTags(t.Context(), admin, tm.ID, nil, many)
	if err != nil || len(res.Dropped) != 7 {
		t.Fatalf("dropped = %v %v", res.Dropped, err)
	}
}

func ptr[T any](v T) *T { return &v }

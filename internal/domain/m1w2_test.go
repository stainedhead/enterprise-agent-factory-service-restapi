package domain

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

func baseParams() NewTeamParams {
	return NewTeamParams{Slug: "payments", DisplayName: "Payments", Kind: TeamKindPersistent, CostCenter: "CC1",
		OwnerEmail: "o@example.com", MaxBudget: MustMoney("100")}
}

func fieldsOf(err error) []string {
	var out []string
	for _, f := range FieldErrors(err) {
		out = append(out, f.Field)
	}
	return out
}

func TestNewTeam(t *testing.T) {
	soon, far, past := t0.Add(24*time.Hour), t0.Add(200*24*time.Hour), t0.Add(-time.Hour)
	tests := []struct {
		name string
		mut  func(*NewTeamParams)
		want string // field of the first error, "" for success
	}{
		{"ok", func(*NewTeamParams) {}, ""},
		{"slug", func(p *NewTeamParams) { p.Slug = "A" }, "slug"},
		{"display name", func(p *NewTeamParams) { p.DisplayName = "" }, "displayName"},
		{"email", func(p *NewTeamParams) { p.OwnerEmail = "nope" }, "ownerEmail"},
		{"cost center", func(p *NewTeamParams) { p.CostCenter = "" }, "costCenter"},
		{"kind", func(p *NewTeamParams) { p.Kind = "x" }, "kind"},
		{"negative ceiling", func(p *NewTeamParams) { p.MaxBudget = MustMoney("-1") }, "maxBudgetUsd"},
		{"tier", func(p *NewTeamParams) { p.IsolationTier = "x" }, "isolationTier"},
		{"quota", func(p *NewTeamParams) { p.Quotas = Quotas{CPU: "lots"} }, "quotas.cpu"},
		{"account", func(p *NewTeamParams) { p.AllowedAwsAccounts = []string{"12"} }, "allowedAwsAccounts"},
		{"persistent with expiry", func(p *NewTeamParams) { p.ExpiresAt = &soon }, "expiresAt"},
		{"persistent with rule", func(p *NewTeamParams) { p.TeardownRule = TeardownRuleAtExpiry }, "teardownRule"},
		{"ephemeral ok", func(p *NewTeamParams) {
			p.Kind, p.ExpiresAt, p.TeardownRule = TeamKindEphemeral, &soon, TeardownRuleAtExpiry
		}, ""},
		{"ephemeral no expiry", func(p *NewTeamParams) { p.Kind, p.TeardownRule = TeamKindEphemeral, TeardownRuleAtExpiry }, "expiresAt"},
		{"ephemeral past", func(p *NewTeamParams) {
			p.Kind, p.ExpiresAt, p.TeardownRule = TeamKindEphemeral, &past, TeardownRuleAtExpiry
		}, "expiresAt"},
		{"ephemeral too far", func(p *NewTeamParams) {
			p.Kind, p.ExpiresAt, p.TeardownRule = TeamKindEphemeral, &far, TeardownRuleAtExpiry
		}, "expiresAt"},
		{"ephemeral no rule", func(p *NewTeamParams) { p.Kind, p.ExpiresAt = TeamKindEphemeral, &soon }, "teardownRule"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := baseParams()
			tc.mut(&p)
			tm, err := NewTeam("team_01JD2Q3R4S5T6V7W8X9Y0Z0001", p, t0, 90)
			if tc.want == "" {
				if err != nil || tm.Namespace != "afs-t-payments" || tm.Status.Phase != PhasePending || tm.Version != 1 {
					t.Fatalf("%+v %v", tm, err)
				}
				return
			}
			if err == nil || fieldsOf(err)[0] != tc.want {
				t.Fatalf("err = %v, want field %s", err, tc.want)
			}
		})
	}
}

func team(t *testing.T) Team {
	t.Helper()
	tm, err := NewTeam("team_01JD2Q3R4S5T6V7W8X9Y0Z0001", baseParams(), t0, 90)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestEffectiveCeilingAndOverride(t *testing.T) {
	tm := team(t)
	amt, exp := MustMoney("500"), t0.Add(time.Hour)
	reason := "load test"
	n, err := tm.Apply(TeamPatch{CeilingOverride: OptionalMoney{Set: true, Value: &amt},
		CeilingOverrideExpiresAt: OptionalTime{Set: true, Value: &exp}, CeilingOverrideReason: reason}, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	if n.EffectiveCeiling(t0).String() != "500.00" || n.EffectiveCeiling(exp.Add(time.Second)).String() != "100.00" {
		t.Fatal("ceiling does not follow the override expiry")
	}
	bad := map[string]TeamPatch{
		"only amount": {CeilingOverride: OptionalMoney{Set: true, Value: &amt}, CeilingOverrideReason: reason},
		"no reason": {CeilingOverride: OptionalMoney{Set: true, Value: &amt},
			CeilingOverrideExpiresAt: OptionalTime{Set: true, Value: &exp}},
		"past": {CeilingOverride: OptionalMoney{Set: true, Value: &amt}, CeilingOverrideReason: reason,
			CeilingOverrideExpiresAt: OptionalTime{Set: true, Value: ptrT(t0.Add(-time.Minute))}},
		"not above max": {CeilingOverride: OptionalMoney{Set: true, Value: ptrT(MustMoney("100"))}, CeilingOverrideReason: reason,
			CeilingOverrideExpiresAt: OptionalTime{Set: true, Value: &exp}},
	}
	for name, p := range bad {
		if _, err := tm.Apply(p, nil, t0); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	// clearing both is allowed; raising max to the override is not.
	cleared, err := n.Apply(TeamPatch{CeilingOverride: OptionalMoney{Set: true}, CeilingOverrideExpiresAt: OptionalTime{Set: true}}, nil, t0)
	if err != nil || cleared.CeilingOverride != nil {
		t.Fatalf("%+v %v", cleared, err)
	}
	hi := MustMoney("600")
	if _, err := n.Apply(TeamPatch{MaxBudget: &hi}, nil, t0); err == nil {
		t.Fatal("max above the active override must be refused")
	}
}

func ptrT[T any](v T) *T { return &v }

func TestApplyFields(t *testing.T) {
	tm := team(t)
	name, desc, mail, cc := "New", "d", "x@example.com", "CC2"
	idp := "idp_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	yes, tier := true, IsolationTierDedicated
	accts := []string{"111122223333"}
	q := Quotas{CPU: "16", Memory: "64Gi", Pods: 10, Storage: "1Ti"}
	p := TeamPatch{DisplayName: &name, Description: &desc, OwnerEmail: &mail, CostCenter: &cc,
		IdentityProviderID: OptionalString{Set: true, Value: &idp}, ViewerKubectl: &yes, MayManageAwsAccounts: &yes,
		MayAddExternalSystems: &yes, IsolationTier: &tier, AllowedAwsAccounts: &accts, Quotas: &q}
	n, err := tm.Apply(p, nil, t0)
	if err != nil || n.Generation != tm.Generation+1 || n.IdentityProviderID != idp || !n.MayAddExternalSystems {
		t.Fatalf("%+v %v", n, err)
	}
	if got := p.GuardrailFields(); len(got) != 5 {
		t.Fatalf("guardrails = %v", got)
	}
	if p.OnlyTeamWrite() || !(TeamPatch{DisplayName: &name}).OnlyTeamWrite() {
		t.Fatal("OnlyTeamWrite")
	}
	cleared, _ := n.Apply(TeamPatch{IdentityProviderID: OptionalString{Set: true}}, nil, t0)
	if cleared.IdentityProviderID != "" {
		t.Fatal("idp not cleared")
	}
	empty := ""
	dup := []string{"111122223333", "111122223333"}
	badTier := IsolationTier("x")
	for name, bp := range map[string]TeamPatch{
		"name": {DisplayName: &empty}, "dup acct": {AllowedAwsAccounts: &dup}, "tier": {IsolationTier: &badTier},
		"quota": {Quotas: &Quotas{Pods: -1}},
	} {
		if _, err := tm.Apply(bp, nil, t0); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestTeamStateTransitions(t *testing.T) {
	tm := team(t)
	if _, err := tm.Resume(t0); KindOf(err) != KindConflict {
		t.Fatal("resume active")
	}
	s, err := tm.Suspend(t0)
	if err != nil || s.State != TeamStateSuspended {
		t.Fatal(err)
	}
	if _, err := s.Suspend(t0); KindOf(err) != KindConflict {
		t.Fatal("suspend suspended")
	}
	d, err := s.MarkDeleting(t0)
	if err != nil || d.Status.Phase != PhaseDeleting {
		t.Fatal(err)
	}
	if _, err := d.MarkDeleting(t0); KindOf(err) != KindConflict {
		t.Fatal("delete deleting")
	}
	if got := (Team{State: TeamStateDeleting}).WithDeletingStatus(); got.Status.Phase != PhaseDeleting {
		t.Fatal("status override")
	}
}

func TestMemberValidate(t *testing.T) {
	ok := NewMemberParams{Role: TeamRoleTeamViewer, SubjectType: SubjectTypeUser, Email: "a@example.com"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]NewMemberParams{
		"service role": {Role: "service_admin", SubjectType: SubjectTypeUser, Email: "a@example.com"},
		"both subject": {Role: TeamRoleTeamAdmin, SubjectType: SubjectTypeUser, Email: "a@example.com", UserID: "usr_01JD2Q3R4S5T6V7W8X9Y0Z0001"},
		"no subject":   {Role: TeamRoleTeamAdmin, SubjectType: SubjectTypeUser},
		"bad email":    {Role: TeamRoleTeamAdmin, SubjectType: SubjectTypeUser, Email: "x"},
		"bad user id":  {Role: TeamRoleTeamAdmin, SubjectType: SubjectTypeUser, UserID: "usr_1"},
		"user group":   {Role: TeamRoleTeamAdmin, SubjectType: SubjectTypeUser, Email: "a@example.com", GroupName: "g"},
		"user idp":     {Role: TeamRoleTeamAdmin, SubjectType: SubjectTypeUser, Email: "a@example.com", IdentityProviderID: "idp_1"},
		"group none":   {Role: TeamRoleTeamAdmin, SubjectType: SubjectTypeGroup},
		"group user":   {Role: TeamRoleTeamAdmin, SubjectType: SubjectTypeGroup, GroupName: "g", Email: "a@example.com"},
		"subject type": {Role: TeamRoleTeamAdmin, SubjectType: "robot"},
	}
	for n, p := range cases {
		if p.Validate() == nil {
			t.Errorf("%s: want error", n)
		}
	}
	if err := (NewMemberParams{Role: TeamRoleTeamAdmin, SubjectType: SubjectTypeGroup, GroupName: "eng"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if KindOf(LastAdminRemoval()) != KindConflict {
		t.Fatal("last admin")
	}
}

func TestAPIKey(t *testing.T) {
	k, err := GenerateAPIKey(bytes.NewReader(bytes.Repeat([]byte{7}, 32)))
	if err != nil || !strings.HasPrefix(k.Secret, APIKeyPrefix) || len(k.Prefix) != 9 || k.SHA256 != HashAPIKey(k.Secret) {
		t.Fatalf("%+v %v", k, err)
	}
	if _, err := GenerateAPIKey(bytes.NewReader(nil)); err == nil {
		t.Fatal("short entropy")
	}
	if strings.Contains(k.Prefix, k.Secret[9:]) {
		t.Fatal("prefix holds the secret")
	}
	good := NewAPIKeyParams{Name: "ci", Role: TeamRoleTeamViewer, ExpiresAt: t0.Add(24 * time.Hour)}
	if err := good.Validate(t0); err != nil {
		t.Fatal(err)
	}
	for n, p := range map[string]NewAPIKeyParams{
		"name": {Role: TeamRoleTeamViewer, ExpiresAt: good.ExpiresAt}, "role": {Name: "x", Role: "service_admin", ExpiresAt: good.ExpiresAt},
		"zero": {Name: "x", Role: TeamRoleTeamAdmin}, "past": {Name: "x", Role: TeamRoleTeamAdmin, ExpiresAt: t0.Add(-1)},
		"91 days": {Name: "x", Role: TeamRoleTeamAdmin, ExpiresAt: t0.Add(91 * 24 * time.Hour)},
	} {
		if p.Validate(t0) == nil {
			t.Errorf("%s: want error", n)
		}
	}
	if err := (NewAPIKeyParams{Name: "x", Role: TeamRoleTeamAdmin, ExpiresAt: t0.Add(90 * 24 * time.Hour)}).Validate(t0); err != nil {
		t.Fatalf("exactly 90 days: %v", err)
	}
}

func TestServiceTagValidate(t *testing.T) {
	ok := []ServiceTag{
		{Key: "env", Mode: ServiceTagModeFixed, Value: ptrT("prod")},
		{Key: "owner", Mode: ServiceTagModeRequired, DefaultValue: ptrT("a"), AllowedValues: []string{"a", "b"}},
		{Key: "app", Mode: ServiceTagModeRequired},
	}
	for _, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", s.Key, err)
		}
	}
	for n, s := range map[string]ServiceTag{
		"key prefix": {Key: "aws:x", Mode: ServiceTagModeRequired}, "secret key": {Key: "my-token", Mode: ServiceTagModeRequired},
		"mode": {Key: "k", Mode: "x"}, "fixed no value": {Key: "k", Mode: ServiceTagModeFixed},
		"fixed default":   {Key: "k", Mode: ServiceTagModeFixed, Value: ptrT("v"), DefaultValue: ptrT("v")},
		"required value":  {Key: "k", Mode: ServiceTagModeRequired, Value: ptrT("v")},
		"default outside": {Key: "k", Mode: ServiceTagModeRequired, DefaultValue: ptrT("z"), AllowedValues: []string{"a"}},
		"empty allowed":   {Key: "k", Mode: ServiceTagModeRequired, AllowedValues: []string{}},
		"bad value":       {Key: "k", Mode: ServiceTagModeFixed, Value: ptrT("a;b")},
		"description":     {Key: "k", Mode: ServiceTagModeRequired, Description: strings.Repeat("x", 501)},
	} {
		if s.Validate() == nil {
			t.Errorf("%s: want error", n)
		}
	}
}

func TestTagSetAndMetadata(t *testing.T) {
	svc := []ServiceTag{
		{Key: "env", Mode: ServiceTagModeFixed, Value: ptrT("prod")},
		{Key: "owner", Mode: ServiceTagModeRequired},
		{Key: "app", Mode: ServiceTagModeRequired, DefaultValue: ptrT("none")},
	}
	set := EffectiveTagSet("team_1", svc, []TagValue{{"zeta", "1"}, {"alpha", "2"}, {"owner", "fin"}})
	var got []string
	for _, tg := range set.Tags {
		got = append(got, tg.Key+":"+string(tg.Source))
	}
	want := "afs-team-id:system app:service_default env:service owner:team alpha:team zeta:team"
	if strings.Join(got, " ") != want {
		t.Fatalf("got %v", got)
	}
	empty := EffectiveTagSet("team_1", svc, nil)
	pairs, dropped := empty.BedrockMetadata(16)
	if len(pairs) != 3 || len(dropped) != 0 { // owner is missing and carries no value
		t.Fatalf("%v %v", pairs, dropped)
	}
	var many []TagValue
	for i := range 20 {
		many = append(many, TagValue{Key: "k" + string(rune('a'+i)), Value: "v"})
	}
	pairs, dropped = EffectiveTagSet("team_1", nil, many).BedrockMetadata(0)
	if len(pairs) != 16 || len(dropped) != 5 || pairs[0].Key != "afs-team-id" || dropped[0] != "kp" {
		t.Fatalf("%d %v", len(pairs), dropped)
	}
}

func TestValidateTeamTags(t *testing.T) {
	svc := []ServiceTag{
		{Key: "env", Mode: ServiceTagModeFixed, Value: ptrT("prod")},
		{Key: "owner", Mode: ServiceTagModeRequired, AllowedValues: []string{"fin", "ops"}},
	}
	if err := ValidateTeamTags(svc, []TagValue{{"owner", "fin"}}); err != nil {
		t.Fatal(err)
	}
	err := ValidateTeamTags(svc, nil)
	if e, ok := err.(*Error); !ok || e.Details["missingTags"].([]string)[0] != "owner" { //nolint:errorlint // test
		t.Fatalf("missing: %v", err)
	}
	for n, v := range map[string][]TagValue{
		"fixed": {{"env", "dev"}, {"owner", "fin"}}, "allowed": {{"owner", "x"}}, "spelling": {{"OWNER", "fin"}},
		"dup": {{"owner", "fin"}, {"Owner", "fin"}}, "reserved": {{"afs-x", "1"}, {"owner", "fin"}},
		"value": {{"owner", "fin"}, {"a", "b;c"}},
	} {
		if ValidateTeamTags(svc, v) == nil {
			t.Errorf("%s: want error", n)
		}
	}
	var many []TagValue
	for i := range 41 {
		many = append(many, TagValue{Key: "key" + string(rune('A'+i)), Value: "v"})
	}
	if ValidateTeamTags(nil, many) == nil {
		t.Error("41 keys: want error")
	}
}

func TestIdentityProviderValidate(t *testing.T) {
	ok := IdentityProvider{Kind: IdPKindOkta, Name: "Okta", IssuerURL: "https://example.okta.com", ConsoleClientID: "c", GroupsClaim: "groups", Enabled: true, IsDefault: true, //nolint:gosec // an example ARN, not a credential
		ClientSecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:x-AbCd"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for n, mut := range map[string]func(*IdentityProvider){
		"kind": func(p *IdentityProvider) { p.Kind = "x" }, "name": func(p *IdentityProvider) { p.Name = "" },
		"http": func(p *IdentityProvider) { p.IssuerURL = "http://x" }, "client": func(p *IdentityProvider) { p.ConsoleClientID = "" },
		"arn": func(p *IdentityProvider) { p.ClientSecretARN = "hunter2" }, "claim": func(p *IdentityProvider) { p.GroupsClaim = "a b" },
		"default disabled": func(p *IdentityProvider) { p.Enabled = false },
	} {
		p := ok
		mut(&p)
		if p.Validate() == nil {
			t.Errorf("%s: want error", n)
		}
	}
}

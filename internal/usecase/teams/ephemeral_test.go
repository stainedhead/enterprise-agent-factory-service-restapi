package teams_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
)

func ephemeralInput(expires time.Duration, rule domain.TeardownRule, budget string) teams.CreateTeamInput {
	in := createInput()
	exp := now.Add(expires)
	in.Params.Slug = "swarm"
	in.Params.Kind, in.Params.ExpiresAt, in.Params.TeardownRule = domain.TeamKindEphemeral, &exp, rule
	if budget != "" {
		in.Budget = &teams.BudgetInput{Amount: domain.MustMoney(budget)}
	}
	return in
}

// RA-US-10.1: an ephemeral team needs an expiry, a teardown rule and a lifetime budget, and its
// expiry is bounded by teams.ephemeral_max_days; every refusal is a 400 and writes nothing.
func TestCreateEphemeralRules(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name   string
		mutate func(in *teams.CreateTeamInput)
		field  string
	}{
		{"complete", func(*teams.CreateTeamInput) {}, ""},
		{"at the longest expiry", func(in *teams.CreateTeamInput) {
			e := now.Add(90 * day)
			in.Params.ExpiresAt = &e
		}, ""},
		{"no expiry", func(in *teams.CreateTeamInput) { in.Params.ExpiresAt = nil }, "expiresAt"},
		{"expiry in the past", func(in *teams.CreateTeamInput) {
			e := now.Add(-time.Hour)
			in.Params.ExpiresAt = &e
		}, "expiresAt"},
		{"expiry beyond teams.ephemeral_max_days", func(in *teams.CreateTeamInput) {
			e := now.Add(91 * day)
			in.Params.ExpiresAt = &e
		}, "expiresAt"},
		{"no teardown rule", func(in *teams.CreateTeamInput) { in.Params.TeardownRule = "" }, "teardownRule"},
		{"unknown teardown rule", func(in *teams.CreateTeamInput) { in.Params.TeardownRule = "never" }, "teardownRule"},
		{"no lifetime budget", func(in *teams.CreateTeamInput) { in.Budget = nil }, "budget"},
		{"zero budget", func(in *teams.CreateTeamInput) { in.Budget = &teams.BudgetInput{Amount: domain.MustMoney("0")} }, "budget.amountUsd"},
		{"budget above the ceiling", func(in *teams.CreateTeamInput) {
			in.Budget = &teams.BudgetInput{Amount: domain.MustMoney("1000.01")}
		}, "budget.amountUsd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			in := ephemeralInput(10*day, domain.TeardownRuleAtCapOrExpiry, "100")
			tc.mutate(&in)
			res, err := r.svc.Create(t.Context(), svcAdmin, in, nil)
			if tc.field == "" {
				if err != nil {
					t.Fatal(err)
				}
				if got := r.repo.Budgets[res.Team.ID]; got.String() != "100.00" {
					t.Fatalf("budget = %v", got)
				}
				return
			}
			if domain.KindOf(err) != domain.KindValidation {
				t.Fatalf("want ValidationError on %s, got %v", tc.field, err)
			}
			var de *domain.Error
			if !asDomain(err, &de) || len(de.Fields) == 0 || de.Fields[0].Field != tc.field {
				t.Fatalf("want field %s, got %v", tc.field, err)
			}
			if len(r.repo.Teams) != 0 || len(r.repo.Budgets) != 0 || len(r.audit.Entries) != 0 || len(r.outbox.Events) != 0 {
				t.Fatal("a refused create wrote something")
			}
		})
	}
}

// The setting is read each time: lowering it lowers the limit.
func TestCreateEphemeralHonorsTheSetting(t *testing.T) {
	r := newRig(t)
	st := r.svc.Settings.(interface {
		Set(string, json.RawMessage)
	})
	st.Set(domain.SettingTeamsEphemeralMaxDays, json.RawMessage(`7`))
	in := ephemeralInput(8*24*time.Hour, domain.TeardownRuleAtExpiry, "10")
	if _, err := r.svc.Create(t.Context(), svcAdmin, in, nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("8 days with a 7 day limit: %v", err)
	}
	in = ephemeralInput(7*24*time.Hour, domain.TeardownRuleAtExpiry, "10")
	if _, err := r.svc.Create(t.Context(), svcAdmin, in, nil); err != nil {
		t.Fatalf("7 days with a 7 day limit: %v", err)
	}
}

// One transaction writes the team, its budget, the audit record and team.created; a team-admin
// cannot create teams at all.
func TestCreateEphemeralWritesOnceAndIsServiceAdminOnly(t *testing.T) {
	r := newRig(t)
	in := ephemeralInput(24*time.Hour, domain.TeardownRuleAtWorkComplete, "25")
	res, err := r.svc.Create(t.Context(), svcAdmin, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Team.Kind != domain.TeamKindEphemeral || res.Team.TeardownRule != domain.TeardownRuleAtWorkComplete || res.Team.ExpiresAt == nil {
		t.Fatalf("%+v", res.Team)
	}
	if len(r.audit.Entries) != 1 || len(r.outbox.Events) != 1 || r.outbox.Events[0].Type != domain.EventTeamCreated {
		t.Fatalf("audit %d, events %d", len(r.audit.Entries), len(r.outbox.Events))
	}
	in.Params.Slug = "other"
	if _, err := r.svc.Create(t.Context(), teamCaller(res.Team.ID, "team-admin"), in, nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("team-admin: %v", err)
	}
}

// RA-US-10.2: a team's kind cannot change, so an ephemeral team never becomes persistent.
func TestEphemeralNeverBecomesPersistent(t *testing.T) {
	r := newRig(t)
	res, err := r.svc.Create(t.Context(), svcAdmin, ephemeralInput(24*time.Hour, domain.TeardownRuleAtExpiry, "25"), nil)
	if err != nil {
		t.Fatal(err)
	}
	persistent := domain.TeamKindPersistent
	_, err = r.svc.Update(t.Context(), svcAdmin, res.Team.ID, res.Team.Version, domain.TeamPatch{Kind: &persistent})
	if domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("ephemeral to persistent: %v", err)
	}
	// The same kind is accepted (a client that sends the whole body back).
	same := domain.TeamKindEphemeral
	if _, err := r.svc.Update(t.Context(), svcAdmin, res.Team.ID, res.Team.Version, domain.TeamPatch{Kind: &same}); err != nil {
		t.Fatalf("same kind: %v", err)
	}
}

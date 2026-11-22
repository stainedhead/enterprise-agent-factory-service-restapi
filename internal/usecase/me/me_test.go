package me

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const (
	teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamB = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
)

type fakeTeams struct {
	teams []TeamInfo
	err   error
	got   port.Session
}

func (f *fakeTeams) Teams(_ context.Context, s port.Session, _ []string) ([]TeamInfo, error) {
	f.got = s
	return f.teams, f.err
}

func has(r Result, team string, perm authz.Permission) bool {
	for _, t := range r.Teams {
		if t.TeamID == team {
			return slices.Contains(t.Permissions, perm)
		}
	}
	return false
}

func TestGetMe(t *testing.T) {
	teams := &fakeTeams{teams: []TeamInfo{
		{ID: teamA, Slug: "alpha", Flags: authz.TeamFlags{ViewerKubectl: true, MayManageAwsAccounts: true}},
		{ID: teamB, Slug: "beta"},
	}}
	s := &Service{Teams: teams}
	user := func(roles ...authz.Binding) authz.Principal {
		return authz.Principal{ID: "usr_1", Type: authz.PrincipalUser, Email: "ada@example.com", Name: "Ada", Bindings: roles}
	}

	t.Run("viewer", func(t *testing.T) {
		r, err := s.Get(t.Context(), user(authz.Binding{Role: authz.RoleTeamViewer, TeamID: teamA}, authz.Binding{Role: authz.RoleTeamViewer, TeamID: teamB}), "req_1")
		if err != nil {
			t.Fatal(err)
		}
		if r.ServiceAdmin || r.SecurityOperator || len(r.ServicePermissions) != 0 || len(r.Teams) != 2 {
			t.Fatalf("%+v", r)
		}
		if r.Teams[0].Role != "team_viewer" || r.Teams[0].Slug != "alpha" {
			t.Fatalf("%+v", r.Teams[0])
		}
		// Exactly what authz grants: viewerKubectl on A only, no write permissions, no AWS accounts.
		if !has(r, teamA, authz.TeamsGetKubeconfig) || has(r, teamB, authz.TeamsGetKubeconfig) {
			t.Fatal("teams:get-kubeconfig must follow viewerKubectl")
		}
		if has(r, teamA, authz.AgentsWrite) || has(r, teamA, authz.TeamsSetAWSAccounts) || !has(r, teamA, authz.AgentsRead) {
			t.Fatal("viewer permissions wrong")
		}
		if teams.got.Actor != "usr_1" || len(teams.got.TeamIDs) != 2 || teams.got.RequestID != "req_1" {
			t.Fatalf("session = %+v", teams.got)
		}
	})

	t.Run("team admin: AWS accounts only with the flag", func(t *testing.T) {
		r, _ := s.Get(t.Context(), user(authz.Binding{Role: authz.RoleTeamAdmin, TeamID: teamA}, authz.Binding{Role: authz.RoleTeamAdmin, TeamID: teamB}), "req_1")
		if !has(r, teamA, authz.TeamsSetAWSAccounts) || has(r, teamB, authz.TeamsSetAWSAccounts) {
			t.Fatal("teams:set-aws-accounts must follow mayManageAwsAccounts")
		}
		if r.Teams[0].Role != "team_admin" || !has(r, teamA, authz.AgentsWrite) || has(r, teamA, authz.TeamsCreate) {
			t.Fatalf("%+v", r.Teams[0])
		}
	})

	t.Run("admin and viewer on one team reports the admin role", func(t *testing.T) {
		r, _ := s.Get(t.Context(), user(authz.Binding{Role: authz.RoleTeamViewer, TeamID: teamA}, authz.Binding{Role: authz.RoleTeamAdmin, TeamID: teamA}), "r")
		if r.Teams[0].Role != "team_admin" {
			t.Fatalf("%+v", r.Teams)
		}
	})

	t.Run("service admin (RA-US-3a.2)", func(t *testing.T) {
		p := user()
		p.ServiceAdmin = true
		r, _ := s.Get(t.Context(), p, "r")
		if !r.ServiceAdmin || len(r.ServicePermissions) != len(authz.AllPermissions()) || len(r.Teams) != 0 {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("security operator (RA-US-3a.2)", func(t *testing.T) {
		p := authz.Principal{ID: "key_1", Type: authz.PrincipalAPIKey, SecurityOperator: true}
		r, _ := s.Get(t.Context(), p, "r")
		for _, want := range []authz.Permission{authz.WorkOrdersRead, authz.WorkOrdersWrite, authz.SwarmTemplatesRead} {
			if !slices.Contains(r.ServicePermissions, want) {
				t.Fatalf("missing %s in %v", want, r.ServicePermissions)
			}
		}
		if !r.SecurityOperator || r.ServiceAdmin || len(r.Teams) != 0 || slices.Contains(r.ServicePermissions, authz.TeamsRead) {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("team API key", func(t *testing.T) {
		p := authz.Principal{ID: "key_2", Type: authz.PrincipalAPIKey, Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: teamB}}}
		r, _ := s.Get(t.Context(), p, "r")
		if r.Principal.Type != authz.PrincipalAPIKey || len(r.Teams) != 1 || r.Teams[0].Role != "team_viewer" {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("workload", func(t *testing.T) {
		p := authz.Principal{ID: "agt_1", Type: authz.PrincipalWorkload, Workload: &authz.Workload{TeamID: teamA, AgentID: "agt_1"}}
		r, _ := s.Get(t.Context(), p, "r")
		if len(r.Teams) != 1 || r.Teams[0].Role != WorkloadRole || len(r.Teams[0].Permissions) != 1 || r.Teams[0].Permissions[0] != authz.WorkOrdersReport {
			t.Fatalf("%+v", r.Teams)
		}
		if teams.got.TeamIDs[0] != teamA {
			t.Fatalf("session = %+v", teams.got)
		}
	})

	t.Run("a bound team that no longer exists is skipped", func(t *testing.T) {
		gone := &Service{Teams: &fakeTeams{teams: []TeamInfo{{ID: teamB, Slug: "beta"}}}}
		r, _ := gone.Get(t.Context(), user(authz.Binding{Role: authz.RoleTeamViewer, TeamID: teamA}, authz.Binding{Role: authz.RoleTeamViewer, TeamID: teamB}), "r")
		if len(r.Teams) != 1 || r.Teams[0].TeamID != teamB {
			t.Fatalf("%+v", r.Teams)
		}
	})

	t.Run("no teams reads nothing", func(t *testing.T) {
		fail := &Service{Teams: &fakeTeams{err: errors.New("must not be called")}}
		if r, err := fail.Get(t.Context(), user(), "r"); err != nil || len(r.Teams) != 0 {
			t.Fatalf("%+v %v", r, err)
		}
	})

	t.Run("source error", func(t *testing.T) {
		bad := &Service{Teams: &fakeTeams{err: errors.New("db")}}
		if _, err := bad.Get(t.Context(), user(authz.Binding{Role: authz.RoleTeamViewer, TeamID: teamA}), "r"); err == nil {
			t.Fatal("error swallowed")
		}
	})
}

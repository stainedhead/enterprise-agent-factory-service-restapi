package authz

import "testing"

const teamA, teamB = "team_a", "team_b"

func TestAuthorizeMatrix(t *testing.T) {
	admin := Principal{Subject: "svc", Bindings: []Binding{{Role: RoleServiceAdmin}}}
	tAdmin := Principal{Subject: "ta", Bindings: []Binding{{Role: RoleTeamAdmin, TeamID: teamA}}}
	viewer := Principal{Subject: "tv", Bindings: []Binding{{Role: RoleTeamViewer, TeamID: teamA}}}
	nobody := Principal{Subject: "x"}

	tests := []struct {
		name string
		p    Principal
		perm Permission
		team string
		want bool
	}{
		{"service admin creates team", admin, TeamsCreate, "", true},
		{"service admin writes any team's agents", admin, AgentsWrite, teamB, true},
		{"team admin cannot create teams", tAdmin, TeamsCreate, "", false},
		{"team admin cannot delete own team", tAdmin, TeamsDelete, teamA, false},
		{"team admin writes own agents", tAdmin, AgentsWrite, teamA, true},
		{"team admin writes own secrets", tAdmin, SecretsWrite, teamA, true},
		{"team admin sets own budget", tAdmin, BudgetsWrite, teamA, true},
		{"team admin cannot set budget ceiling", tAdmin, BudgetsSetCeiling, teamA, false},
		{"team admin isolated from other team", tAdmin, AgentsRead, teamB, false},
		{"team admin no admin settings", tAdmin, AdminSettings, "", false},
		{"viewer reads agents", viewer, AgentsRead, teamA, true},
		{"viewer reads secret metadata", viewer, SecretsReadMetadata, teamA, true},
		{"viewer cannot write secrets", viewer, SecretsWrite, teamA, false},
		{"viewer cannot write agents", viewer, AgentsWrite, teamA, false},
		{"viewer cannot write budget", viewer, BudgetsWrite, teamA, false},
		{"viewer isolated from other team", viewer, TeamsRead, teamB, false},
		{"no bindings denied", nobody, TeamsRead, teamA, false},
		{"empty team id never matches team binding", tAdmin, TeamsRead, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Authorize(tc.p, tc.perm, tc.team); got != tc.want {
				t.Fatalf("Authorize(%s, %s, %q) = %v, want %v", tc.p.Subject, tc.perm, tc.team, got, tc.want)
			}
		})
	}
}

func TestEffectiveViewerHasNoWrites(t *testing.T) {
	viewer := Principal{Bindings: []Binding{{Role: RoleTeamViewer, TeamID: teamA}}}
	for _, p := range Effective(viewer, teamA) {
		switch p {
		case AgentsWrite, SecretsWrite, BudgetsWrite, MembersWrite, TeamsWrite:
			t.Fatalf("viewer unexpectedly holds %s", p)
		}
	}
	if len(Effective(viewer, teamA)) != len(viewerPerms) {
		t.Fatalf("viewer effective = %d perms, want %d", len(Effective(viewer, teamA)), len(viewerPerms))
	}
}

func TestCanGrant(t *testing.T) {
	admin := Principal{Bindings: []Binding{{Role: RoleServiceAdmin}}}
	tAdmin := Principal{Bindings: []Binding{{Role: RoleTeamAdmin, TeamID: teamA}}}
	viewer := Principal{Bindings: []Binding{{Role: RoleTeamViewer, TeamID: teamA}}}

	if !CanGrant(admin, RoleServiceAdmin, "") {
		t.Error("service admin must grant service_admin")
	}
	if CanGrant(tAdmin, RoleServiceAdmin, "") {
		t.Error("team admin must not grant service_admin")
	}
	if !CanGrant(tAdmin, RoleTeamViewer, teamA) {
		t.Error("team admin must grant viewer on own team")
	}
	if CanGrant(tAdmin, RoleTeamAdmin, teamB) {
		t.Error("team admin must not grant on another team")
	}
	if CanGrant(viewer, RoleTeamViewer, teamA) {
		t.Error("viewer must not grant")
	}
	if CanGrant(tAdmin, Role("owner"), teamA) {
		t.Error("unknown role must not be grantable")
	}
}

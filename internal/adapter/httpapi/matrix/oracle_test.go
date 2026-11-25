package matrix

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
)

func principalFor(c Caller, teamA string) (authz.Principal, bool) {
	switch c {
	case Viewer, TeamKeyViewer:
		return authz.Principal{ID: "p", Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: teamA}}}, true
	case TeamAdmin, TeamKeyAdmin:
		return authz.Principal{ID: "p", Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: teamA}}}, true
	case ServiceAdmin:
		return authz.Principal{ID: "p", ServiceAdmin: true}, true
	case SecurityOperator, ServiceKey:
		return authz.Principal{ID: "p", SecurityOperator: true}, true
	case Workload:
		return authz.Principal{ID: "agt_1", Type: authz.PrincipalWorkload, Workload: &authz.Workload{TeamID: teamA, AgentID: "agt_1"}}, true
	case Unbound:
		return authz.Principal{ID: "p"}, true
	}
	return authz.Principal{}, false
}

// The oracle is computed from the contract's JSON, not from the authz package; this test proves the
// two agree for every operation, caller and flag combination, so a drift in either fails here.
func TestOracleAgreesWithAuthz(t *testing.T) {
	c, err := LoadContract()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Operations) != 221 {
		t.Fatalf("operations = %d", len(c.Operations))
	}
	const teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	for _, op := range c.Operations {
		for mask := 0; mask < 8; mask++ {
			flags := authz.TeamFlags{MayManageAwsAccounts: mask&1 != 0, ViewerKubectl: mask&2 != 0, MayAddExternalSystems: mask&4 != 0}
			for _, caller := range AllCallers {
				p, ok := principalFor(caller, teamA)
				if !ok {
					continue
				}
				res := authz.Resource{Scope: authz.Scope(op.Scope)}
				if op.Scope == "team" {
					res.TeamID, res.Flags = teamA, flags
				}
				anyOf := make([]authz.Permission, 0)
				for _, x := range op.AnyOf {
					anyOf = append(anyOf, authz.Permission(x))
				}
				d := authz.Check(p, authz.Requirement{Permission: authz.Permission(op.Permission), AnyOf: anyOf, Condition: authz.Condition(op.Condition)}, res)
				var got Outcome
				switch d.Outcome {
				case authz.Allow:
					got = Allowed
				case authz.NotFound:
					got = NotFound
				default:
					got = Forbidden
				}
				if want := c.Expected(op, caller, flags); want != got {
					t.Errorf("%s as %s flags=%+v: oracle %s, authz %s", op.OperationID, caller, flags, want, got)
				}
			}
		}
	}
}

func TestOracleSpotChecks(t *testing.T) {
	c, err := LoadContract()
	if err != nil {
		t.Fatal(err)
	}
	none := authz.TeamFlags{}
	cases := []struct {
		op     string
		caller Caller
		flags  authz.TeamFlags
		want   Outcome
	}{
		{"GetHealth", Anonymous, none, Allowed},
		{"GetMe", Anonymous, none, Unauthorized},
		{"GetMe", Unbound, none, Allowed},
		{"GetTeam", Unbound, none, NotFound},
		{"GetTeam", SecurityOperator, none, Forbidden},
		{"GetTeam", Viewer, none, Allowed},
		{"CreateApiKey", Viewer, none, Forbidden},
		{"CreateApiKey", TeamKeyAdmin, none, Allowed},
		{"ListApiKeys", Viewer, none, Forbidden},
		{"CreateTeam", TeamAdmin, none, Forbidden},
		{"CreateTeam", ServiceAdmin, none, Allowed},
		{"ListTeamDirectory", SecurityOperator, none, Allowed},
		{"ListTeamDirectory", Viewer, none, Forbidden},
		{"GetTeamKubeconfig", Viewer, none, Forbidden},
		{"GetTeamKubeconfig", Viewer, authz.TeamFlags{ViewerKubectl: true}, Allowed},
		{"ListTeams", Unbound, none, Forbidden},
		{"ListTeams", Viewer, none, Allowed},
	}
	for _, tc := range cases {
		if got := c.Expected(c.Operations[tc.op], tc.caller, tc.flags); got != tc.want {
			t.Errorf("%s as %s: %s, want %s", tc.op, tc.caller, got, tc.want)
		}
	}
}

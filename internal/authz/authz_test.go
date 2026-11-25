package authz_test

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
)

const (
	teamA = "team_01JD00000000000000000000AA"
	teamB = "team_01JD00000000000000000000BB"
)

var (
	viewer   = authz.Principal{ID: "usr_v", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: teamA}}}
	admin    = authz.Principal{ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: teamA}}}
	svcAdmin = authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true}
	secOp    = authz.Principal{ID: "usr_o", Type: authz.PrincipalUser, SecurityOperator: true}
	secKey   = authz.Principal{ID: "key_o", Type: authz.PrincipalAPIKey, SecurityOperator: true}
	teamKey  = authz.Principal{ID: "key_t", Type: authz.PrincipalAPIKey, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: teamA}}}
	unbound  = authz.Principal{ID: "usr_u", Type: authz.PrincipalUser}
	workload = authz.Principal{ID: "agt_w", Type: authz.PrincipalWorkload, Workload: &authz.Workload{TeamID: teamA, AgentID: "agt_w"}}
)

func team(id string, flags authz.TeamFlags) authz.Resource {
	return authz.Resource{Scope: authz.ScopeTeam, TeamID: id, Flags: flags}
}

var (
	service = authz.Resource{Scope: authz.ScopeService}
	anyRes  = authz.Resource{Scope: authz.ScopeAny}
)

type permissionsFile struct {
	Roles       []string `json:"roles"`
	Permissions []struct {
		Name   string `json:"name"`
		Grants []struct {
			Role      string `json:"role"`
			Condition string `json:"condition"`
		} `json:"grants"`
	} `json:"permissions"`
	Workload struct {
		Name string `json:"name"`
	} `json:"workload"`
}

// TestCatalogEqualsContract: the catalog and grants equal API contract §3 exactly (RA-US-1.2).
func TestCatalogEqualsContract(t *testing.T) {
	raw, err := os.ReadFile("../../api/contract/permissions.json")
	if err != nil {
		t.Fatal(err)
	}
	var f permissionsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	var names []authz.Permission
	for _, p := range f.Permissions {
		names = append(names, authz.Permission(p.Name))
		var want []authz.Grant
		for _, g := range p.Grants {
			want = append(want, authz.Grant{Role: authz.Role(g.Role), Condition: authz.Condition(g.Condition)})
		}
		if got := authz.Grants(authz.Permission(p.Name)); !slices.Equal(got, want) {
			t.Errorf("%s grants = %v, want %v", p.Name, got, want)
		}
		if !authz.Permission(p.Name).Known() {
			t.Errorf("%s unknown", p.Name)
		}
	}
	if got := authz.AllPermissions(); !slices.Equal(got, names) {
		t.Errorf("catalog = %v\nwant %v", got, names)
	}
	if authz.Permission("budgets:set-ceiling").Known() {
		t.Error("budgets:set-ceiling must be gone (D33)")
	}
	for _, r := range f.Roles {
		if !authz.Role(r).Valid() {
			t.Errorf("role %s invalid", r)
		}
	}
	if authz.Role("owner").Valid() || authz.RoleServiceAdmin.TeamRole() {
		t.Error("role checks")
	}
	if f.Workload.Name != string(authz.WorkOrdersReport) {
		t.Errorf("workload grant = %s", f.Workload.Name)
	}
}

func TestCheckTeamRoutes(t *testing.T) {
	cases := []struct {
		name string
		p    authz.Principal
		perm authz.Permission
		res  authz.Resource
		want authz.Outcome
	}{
		{"viewer reads own team", viewer, authz.TeamsRead, team(teamA, authz.TeamFlags{}), authz.Allow},
		{"viewer cannot write", viewer, authz.TeamsWrite, team(teamA, authz.TeamFlags{}), authz.Deny},
		{"viewer cannot read api keys (D50)", viewer, authz.APIKeysRead, team(teamA, authz.TeamFlags{}), authz.Deny},
		{"viewer on another team is not found", viewer, authz.TeamsRead, team(teamB, authz.TeamFlags{}), authz.NotFound},
		{"unbound is not found", unbound, authz.TeamsRead, team(teamA, authz.TeamFlags{}), authz.NotFound},
		{"admin writes agents", admin, authz.AgentsWrite, team(teamA, authz.TeamFlags{}), authz.Allow},
		{"admin cannot set guardrails", admin, authz.TeamsSetGuardrails, team(teamA, authz.TeamFlags{}), authz.Deny},
		{"admin cannot delete team", admin, authz.TeamsDelete, team(teamA, authz.TeamFlags{}), authz.Deny},
		{"team key acts as its role", teamKey, authz.SecretsWrite, team(teamA, authz.TeamFlags{}), authz.Allow},
		{"team key other team", teamKey, authz.SecretsWrite, team(teamB, authz.TeamFlags{}), authz.NotFound},
		{"service admin everything", svcAdmin, authz.TeamsSetGuardrails, team(teamB, authz.TeamFlags{}), authz.Allow},
		{"security operator denied existing team", secOp, authz.TeamsRead, team(teamA, authz.TeamFlags{}), authz.Deny},
		{"security operator denied unknown team", secOp, authz.WorkOrdersRead, team("team_01JD00000000000000000000ZZ", authz.TeamFlags{}), authz.Deny},
		{"service key denied team route", secKey, authz.WorkOrdersRead, team(teamA, authz.TeamFlags{}), authz.Deny},
		{"workload denied team route", workload, authz.AgentsRead, team(teamA, authz.TeamFlags{}), authz.Deny},
		{"workload not found on another team's route", workload, authz.AgentsRead, team(teamB, authz.TeamFlags{}), authz.NotFound},
		{"workload not found on an unknown team's route", workload, authz.TeamsRead, team("team_0ZZZZZZZZZZZZZZZZZZZZZZZZZ", authz.TeamFlags{}), authz.NotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := authz.Authorize(c.p, c.perm, c.res)
			if d.Outcome != c.want {
				t.Fatalf("outcome = %v, want %v", d.Outcome, c.want)
			}
			if c.want == authz.Deny && d.RequiredPermission != c.perm {
				t.Errorf("required = %s, want %s", d.RequiredPermission, c.perm)
			}
			if d.Allowed() != (c.want == authz.Allow) {
				t.Error("Allowed")
			}
		})
	}
}

// TestConditionalGrants covers RA-US-1.5: each conditional grant has an allowed and a denied case.
func TestConditionalGrants(t *testing.T) {
	on := authz.TeamFlags{MayManageAwsAccounts: true, ViewerKubectl: true, MayAddExternalSystems: true}
	off := authz.TeamFlags{}
	declare := authz.Requirement{Permission: authz.ExternalSystemsWrite, Condition: authz.CondMayAddExternalSystems}
	cases := []struct {
		name string
		p    authz.Principal
		req  authz.Requirement
		res  authz.Resource
		want authz.Outcome
	}{
		{"set-aws-accounts with flag", admin, authz.Requirement{Permission: authz.TeamsSetAWSAccounts}, team(teamA, on), authz.Allow},
		{"set-aws-accounts without flag", admin, authz.Requirement{Permission: authz.TeamsSetAWSAccounts}, team(teamA, off), authz.Deny},
		{"set-aws-accounts never for viewers", viewer, authz.Requirement{Permission: authz.TeamsSetAWSAccounts}, team(teamA, on), authz.Deny},
		{"kubeconfig viewer with flag", viewer, authz.Requirement{Permission: authz.TeamsGetKubeconfig}, team(teamA, on), authz.Allow},
		{"kubeconfig viewer without flag", viewer, authz.Requirement{Permission: authz.TeamsGetKubeconfig}, team(teamA, off), authz.Deny},
		{"kubeconfig admin without flag", admin, authz.Requirement{Permission: authz.TeamsGetKubeconfig}, team(teamA, off), authz.Deny},
		{"kubeconfig service admin without flag", svcAdmin, authz.Requirement{Permission: authz.TeamsGetKubeconfig}, team(teamA, off), authz.Allow},
		{"declare with flag", admin, declare, team(teamA, on), authz.Allow},
		{"declare without flag", admin, declare, team(teamA, off), authz.Deny},
		{"declare viewer with flag", viewer, declare, team(teamA, on), authz.Deny},
		{"declare unbound", unbound, declare, team(teamA, on), authz.NotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if d := authz.Check(c.p, c.req, c.res); d.Outcome != c.want {
				t.Fatalf("outcome = %v, want %v", d.Outcome, c.want)
			}
		})
	}
}

func TestWorkOrderGrants(t *testing.T) {
	assigned := &authz.WorkOrderRef{AssignedTeamID: teamA}
	owned := &authz.WorkOrderRef{AssignedTeamID: teamB, RepoOwnerTeamIDs: []string{teamA}}
	other := &authz.WorkOrderRef{AssignedTeamID: teamB, RepoOwnerTeamIDs: []string{teamB}}
	unassigned := &authz.WorkOrderRef{}
	wo := func(r *authz.WorkOrderRef) authz.Resource { return authz.Resource{Scope: authz.ScopeAny, WorkOrder: r} }
	report := authz.Requirement{AnyOf: []authz.Permission{authz.WorkOrdersReport, authz.WorkOrdersWrite}}
	cases := []struct {
		name string
		p    authz.Principal
		req  authz.Requirement
		res  authz.Resource
		want authz.Outcome
	}{
		{"viewer lists at route level", viewer, authz.Requirement{Permission: authz.WorkOrdersRead}, anyRes, authz.Allow},
		{"viewer reads assigned", viewer, authz.Requirement{Permission: authz.WorkOrdersRead}, wo(assigned), authz.Allow},
		{"viewer reads own repo", viewer, authz.Requirement{Permission: authz.WorkOrdersRead}, wo(owned), authz.Allow},
		{"viewer cannot see other", viewer, authz.Requirement{Permission: authz.WorkOrdersRead}, wo(other), authz.NotFound},
		{"unbound cannot list", unbound, authz.Requirement{Permission: authz.WorkOrdersRead}, anyRes, authz.Deny},
		{"security operator reads all", secOp, authz.Requirement{Permission: authz.WorkOrdersRead}, wo(other), authz.Allow},
		{"security operator writes", secKey, authz.Requirement{Permission: authz.WorkOrdersWrite}, anyRes, authz.Allow},
		{"admin cannot write work orders", admin, authz.Requirement{Permission: authz.WorkOrdersWrite}, anyRes, authz.Deny},
		{"admin reports on assigned", admin, report, wo(assigned), authz.Allow},
		{"admin sees owned but cannot report", admin, report, wo(owned), authz.Deny},
		{"admin cannot report on hidden", admin, report, wo(other), authz.NotFound},
		{"viewer cannot report", viewer, report, wo(assigned), authz.Deny},
		{"security operator updates repo by write", secOp, report, wo(other), authz.Allow},
		{"workload reports on assigned", workload, report, wo(assigned), authz.Allow},
		{"workload route level", workload, report, anyRes, authz.Allow},
		{"workload not on other team", workload, report, wo(other), authz.NotFound},
		{"workload not on unassigned", workload, report, wo(unassigned), authz.NotFound},
		{"workload cannot read", workload, authz.Requirement{Permission: authz.WorkOrdersRead}, anyRes, authz.Deny},
		{"workload GetMe", workload, authz.Requirement{}, anyRes, authz.Allow},
		{"service admin", svcAdmin, report, wo(other), authz.Allow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if d := authz.Check(c.p, c.req, c.res); d.Outcome != c.want {
				t.Fatalf("outcome = %v, want %v", d.Outcome, c.want)
			}
		})
	}
}

func TestServiceAndPublicRoutes(t *testing.T) {
	cases := []struct {
		name string
		p    authz.Principal
		perm authz.Permission
		res  authz.Resource
		want authz.Outcome
	}{
		{"admin settings for service admin", svcAdmin, authz.AdminSettings, service, authz.Allow},
		{"admin settings denied to team admin", admin, authz.AdminSettings, service, authz.Deny},
		{"create team denied to team admin", admin, authz.TeamsCreate, service, authz.Deny},
		{"team directory for security operator", secOp, authz.TeamsReadDirectory, service, authz.Allow},
		{"swarm templates read for service key", secKey, authz.SwarmTemplatesRead, service, authz.Allow},
		{"swarm template admin denied to security operator", secOp, authz.AdminSwarmTemplates, service, authz.Deny},
		{"workload denied service", workload, authz.AdminSettings, service, authz.Deny},
		{"public for anyone", unbound, "", authz.Resource{Scope: authz.ScopePublic}, authz.Allow},
		{"list teams for viewer", viewer, authz.TeamsRead, anyRes, authz.Allow},
		{"list teams denied to security operator", secOp, authz.TeamsRead, anyRes, authz.Deny},
		{"list models for viewer", viewer, authz.ModelsRead, anyRes, authz.Allow},
		{"list models for service admin", svcAdmin, authz.ModelsRead, anyRes, authz.Allow},
		{"me for unbound", unbound, "", anyRes, authz.Allow},
		{"service route without permission is denied", svcAdmin, "", service, authz.Deny},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if d := authz.Authorize(c.p, c.perm, c.res); d.Outcome != c.want {
				t.Fatalf("outcome = %v, want %v", d.Outcome, c.want)
			}
		})
	}
}

func TestEffective(t *testing.T) {
	off := authz.Effective(viewer, teamA, authz.TeamFlags{})
	on := authz.Effective(viewer, teamA, authz.TeamFlags{ViewerKubectl: true})
	if slices.Contains(off, authz.TeamsGetKubeconfig) || !slices.Contains(on, authz.TeamsGetKubeconfig) {
		t.Errorf("viewerKubectl must decide teams:get-kubeconfig: off=%v on=%v", off, on)
	}
	if slices.Contains(off, authz.TeamsWrite) || !slices.Contains(off, authz.WorkOrdersRead) {
		t.Errorf("viewer effective = %v", off)
	}
	a := authz.Effective(admin, teamA, authz.TeamFlags{})
	if slices.Contains(a, authz.TeamsSetAWSAccounts) || !slices.Contains(authz.Effective(admin, teamA, authz.TeamFlags{MayManageAwsAccounts: true}), authz.TeamsSetAWSAccounts) {
		t.Error("mayManageAwsAccounts must decide teams:set-aws-accounts")
	}
	if !slices.Contains(a, authz.WorkOrdersReport) || slices.Contains(a, authz.AdminSettings) {
		t.Errorf("admin effective = %v", a)
	}
	if got := authz.Effective(viewer, teamB, authz.TeamFlags{}); len(got) != 0 {
		t.Errorf("unbound team effective = %v", got)
	}
	if got := authz.Effective(svcAdmin, teamB, authz.TeamFlags{}); len(got) != len(authz.AllPermissions()) {
		t.Errorf("service admin effective = %d", len(got))
	}
	if got := authz.ServicePermissions(svcAdmin); len(got) != len(authz.AllPermissions()) {
		t.Error("service admin service permissions")
	}
	want := []authz.Permission{authz.TeamsReadDirectory, authz.WorkOrdersRead, authz.WorkOrdersWrite, authz.SwarmTemplatesRead,
		authz.McpServersRead, authz.McpServersReview, authz.McpServersSuspend, authz.McpEventsRead}
	if got := authz.ServicePermissions(secOp); !slices.Equal(got, want) {
		t.Errorf("security operator column = %v, want %v", got, want)
	}
	if authz.ServicePermissions(admin) != nil {
		t.Error("team admin has no service permissions")
	}
}

func TestCanGrant(t *testing.T) {
	cases := []struct {
		name string
		p    authz.Principal
		role authz.Role
		team string
		want bool
	}{
		{"admin grants viewer", admin, authz.RoleTeamViewer, teamA, true},
		{"admin grants admin", admin, authz.RoleTeamAdmin, teamA, true},
		{"admin cannot grant on other team", admin, authz.RoleTeamViewer, teamB, false},
		{"admin cannot grant service admin", admin, authz.RoleServiceAdmin, teamA, false},
		{"admin cannot grant security operator", admin, authz.RoleSecurityOperator, "", false},
		{"viewer cannot grant", viewer, authz.RoleTeamViewer, teamA, false},
		{"service admin grants service admin", svcAdmin, authz.RoleServiceAdmin, "", true},
		{"service admin grants security operator", svcAdmin, authz.RoleSecurityOperator, "", true},
		{"service admin grants team role", svcAdmin, authz.RoleTeamAdmin, teamB, true},
		{"team role needs a team", svcAdmin, authz.RoleTeamAdmin, "", false},
		{"unknown role", svcAdmin, authz.Role("owner"), teamA, false},
	}
	for _, c := range cases {
		if got := authz.CanGrant(c.p, c.role, c.team); got != c.want {
			t.Errorf("%s: CanGrant = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPrincipalTeams(t *testing.T) {
	p := authz.Principal{Bindings: []authz.Binding{
		{Role: authz.RoleTeamViewer, TeamID: teamA}, {Role: authz.RoleTeamAdmin, TeamID: teamA},
		{Role: authz.RoleTeamViewer, TeamID: teamA}, {Role: authz.RoleTeamViewer, TeamID: teamB},
		{Role: authz.RoleServiceAdmin},
	}}
	if got := p.TeamIDs(); !slices.Equal(got, []string{teamA, teamB}) {
		t.Errorf("TeamIDs = %v", got)
	}
	if got := p.TeamRoles(teamA); !slices.Equal(got, []authz.Role{authz.RoleTeamViewer, authz.RoleTeamAdmin}) {
		t.Errorf("TeamRoles = %v", got)
	}
}

// TestModelEndpointPermissions: the four v1.5.0 permissions (API contract §3), allowed and denied per role.
func TestModelEndpointPermissions(t *testing.T) {
	collab := authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamCollaborator, TeamID: teamA}}}
	ta := team(teamA, authz.TeamFlags{})
	cases := []struct {
		name string
		p    authz.Principal
		perm authz.Permission
		res  authz.Resource
		want authz.Outcome
	}{
		{"viewer reads routes", viewer, authz.ModelEndpointsRead, ta, authz.Allow},
		{"collaborator reads routes", collab, authz.ModelEndpointsRead, ta, authz.Allow},
		{"viewer cannot opt in", viewer, authz.ModelEndpointsWrite, ta, authz.Deny},
		{"collaborator cannot opt in", collab, authz.ModelEndpointsWrite, ta, authz.Deny},
		{"team-admin reads routes", admin, authz.ModelEndpointsRead, ta, authz.Allow},
		{"team-admin opts in", admin, authz.ModelEndpointsWrite, ta, authz.Allow},
		{"team-admin of another team is not found", admin, authz.ModelEndpointsWrite, team(teamB, authz.TeamFlags{}), authz.NotFound},
		{"team-admin cannot set gateway settings", admin, authz.TeamsSetGuardrails, ta, authz.Deny},
		{"service admin writes any team's routes", svcAdmin, authz.ModelEndpointsWrite, team(teamB, authz.TeamFlags{}), authz.Allow},
		{"service admin manages endpoints", svcAdmin, authz.AdminModelEndpoints, service, authz.Allow},
		{"service admin switches providers", svcAdmin, authz.AdminInferenceProviders, service, authz.Allow},
		{"team-admin cannot manage service endpoints", admin, authz.AdminModelEndpoints, service, authz.Deny},
		{"team-admin cannot switch providers", admin, authz.AdminInferenceProviders, service, authz.Deny},
		{"viewer cannot manage service endpoints", viewer, authz.AdminModelEndpoints, service, authz.Deny},
		{"team key cannot switch providers", teamKey, authz.AdminInferenceProviders, service, authz.Deny},
		{"security operator cannot manage endpoints", secOp, authz.AdminModelEndpoints, service, authz.Deny},
		{"security operator cannot read team routes", secOp, authz.ModelEndpointsRead, ta, authz.Deny},
		{"workload cannot read routes", workload, authz.ModelEndpointsRead, ta, authz.Deny},
		{"unbound is not found", unbound, authz.ModelEndpointsRead, ta, authz.NotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := authz.Authorize(c.p, c.perm, c.res)
			if d.Outcome != c.want {
				t.Fatalf("outcome = %v, want %v", d.Outcome, c.want)
			}
			if c.want == authz.Deny && d.RequiredPermission != c.perm {
				t.Errorf("required = %s, want %s", d.RequiredPermission, c.perm)
			}
		})
	}
}

// TestMcpPermissions: contract v1.6.0 §4.14. Service paths serve service admins and security operators; team
// roles use the team paths (CD-45); team-admins pass the route for team-mcp-servers:approve and the use case
// decides mcpApprovalPolicy.
func TestMcpPermissions(t *testing.T) {
	collab := authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamCollaborator, TeamID: teamA}}}
	ta := team(teamA, authz.TeamFlags{})
	cases := []struct {
		name string
		p    authz.Principal
		perm authz.Permission
		res  authz.Resource
		want authz.Outcome
	}{
		{"service admin writes the catalog", svcAdmin, authz.McpServersWrite, service, authz.Allow},
		{"service admin writes MCP settings", svcAdmin, authz.McpSettingsWrite, service, authz.Allow},
		{"security operator reads the catalog", secOp, authz.McpServersRead, service, authz.Allow},
		{"security operator reviews", secOp, authz.McpServersReview, service, authz.Allow},
		{"security operator suspends", secOp, authz.McpServersSuspend, service, authz.Allow},
		{"security operator reads the ledger", secOp, authz.McpEventsRead, service, authz.Allow},
		{"security operator cannot write the catalog", secOp, authz.McpServersWrite, service, authz.Deny},
		{"security operator cannot write MCP settings", secOp, authz.McpSettingsWrite, service, authz.Deny},
		{"security operator is denied team MCP routes", secOp, authz.TeamMcpServersRead, ta, authz.Deny},
		{"team-admin cannot use service MCP paths", admin, authz.McpServersRead, service, authz.Deny},
		{"team-admin cannot review", admin, authz.McpServersReview, service, authz.Deny},
		{"viewer cannot read the service ledger", viewer, authz.McpEventsRead, service, authz.Deny},
		{"team key cannot suspend", teamKey, authz.McpServersSuspend, service, authz.Deny},
		{"viewer reads the team's servers", viewer, authz.TeamMcpServersRead, ta, authz.Allow},
		{"collaborator reads the team's servers", collab, authz.TeamMcpServersRead, ta, authz.Allow},
		{"viewer reads the team ledger", viewer, authz.McpEventsRead, ta, authz.Allow},
		{"viewer cannot switch servers", viewer, authz.TeamMcpServersWrite, ta, authz.Deny},
		{"collaborator cannot switch servers", collab, authz.TeamMcpServersWrite, ta, authz.Deny},
		{"viewer cannot approve", viewer, authz.TeamMcpServersApprove, ta, authz.Deny},
		{"team-admin switches servers", admin, authz.TeamMcpServersWrite, ta, authz.Allow},
		{"team-admin passes the approval route", admin, authz.TeamMcpServersApprove, ta, authz.Allow},
		{"team-admin writes federated identities", admin, authz.AgentFederatedIdentitiesWrite, ta, authz.Allow},
		{"viewer cannot write federated identities", viewer, authz.AgentFederatedIdentitiesWrite, ta, authz.Deny},
		{"team-admin of another team is not found", admin, authz.TeamMcpServersWrite, team(teamB, authz.TeamFlags{}), authz.NotFound},
		{"service admin approves any team's server", svcAdmin, authz.TeamMcpServersApprove, team(teamB, authz.TeamFlags{}), authz.Allow},
		{"workload cannot read team servers", workload, authz.TeamMcpServersRead, ta, authz.Deny},
		{"unbound is not found", unbound, authz.TeamMcpServersRead, ta, authz.NotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := authz.Check(c.p, authz.Requirement{Permission: c.perm}, c.res)
			if c.perm == authz.TeamMcpServersApprove {
				d = authz.Check(c.p, authz.Requirement{Permission: c.perm, Condition: authz.CondMcpApprovalPolicy}, c.res)
			}
			if d.Outcome != c.want {
				t.Fatalf("outcome = %v, want %v", d.Outcome, c.want)
			}
			if c.want == authz.Deny && d.RequiredPermission != c.perm {
				t.Errorf("required = %s, want %s", d.RequiredPermission, c.perm)
			}
		})
	}
}

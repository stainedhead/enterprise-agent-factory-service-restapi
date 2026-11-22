// Package authz is the single place Agent Factory access decisions are made. The permission catalog
// (catalog_gen.go) is generated from API contract §3. The API's route table names a Permission per
// route and calls Check; use cases call it again with the loaded resource where a grant depends on
// it. Clients read Effective (exposed via GET /v1/me) only to shape their UX.
package authz

import "slices"

// Role is a role of API contract §3. A workload principal holds no role; its grant is fixed (§2).
type Role string

// Roles.
const (
	RoleTeamViewer       Role = "team_viewer"
	RoleTeamCollaborator Role = "team_collaborator"
	RoleTeamAdmin        Role = "team_admin"
	RoleServiceAdmin     Role = "service_admin"
	RoleSecurityOperator Role = "security_operator"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleTeamViewer, RoleTeamCollaborator, RoleTeamAdmin, RoleServiceAdmin, RoleSecurityOperator:
		return true
	}
	return false
}

// TeamRole reports whether r is bound per team (team_viewer, team_collaborator, team_admin).
func (r Role) TeamRole() bool {
	return r == RoleTeamViewer || r == RoleTeamCollaborator || r == RoleTeamAdmin
}

// Permission names one action of the permission catalog (API contract §3).
type Permission string

// Condition names what a conditional grant or an operation depends on.
type Condition string

// Conditions used by the catalog and the route table.
const (
	// CondMayManageAwsAccounts: the team's mayManageAwsAccounts flag is on.
	CondMayManageAwsAccounts Condition = "mayManageAwsAccounts"
	// CondViewerKubectl: the team's viewerKubectl flag is on.
	CondViewerKubectl Condition = "viewerKubectl"
	// CondMayAddExternalSystems: the team's mayAddExternalSystems flag is on.
	CondMayAddExternalSystems Condition = "mayAddExternalSystems"
	// CondAssignedOrOwnRepos: the work order is assigned to the caller's team or touches a
	// repository the caller's team owns.
	CondAssignedOrOwnRepos Condition = "assignedOrOwnRepos"
	// CondAssignedTeam: the work order is assigned to the caller's team.
	CondAssignedTeam Condition = "assignedTeam"
)

// Scope says how a route is scoped.
type Scope string

// Scopes.
const (
	// ScopeTeam routes have /v1/teams/{teamId} in the path.
	ScopeTeam Scope = "team"
	// ScopeService routes are service level (*svc* in the contract).
	ScopeService Scope = "service"
	// ScopePublic routes need no credential.
	ScopePublic Scope = "public"
	// ScopeAny routes are open to any signed-in caller that holds the permission somewhere.
	ScopeAny Scope = "any"
)

// Grant is one role's grant of a permission, with an optional condition.
type Grant struct {
	Role      Role
	Condition Condition
}

// AllPermissions lists every permission in contract §3 order.
func AllPermissions() []Permission { return slices.Clone(catalogOrder) }

// Grants returns the grants of perm.
func Grants(perm Permission) []Grant { return slices.Clone(catalogGrants[perm]) }

// Known reports whether perm is in the catalog.
func (perm Permission) Known() bool {
	_, ok := catalogGrants[perm]
	return ok
}

// PrincipalType is the kind of caller.
type PrincipalType string

// Principal types.
const (
	PrincipalUser     PrincipalType = "user"
	PrincipalAPIKey   PrincipalType = "api_key"
	PrincipalWorkload PrincipalType = "workload"
)

// Binding grants a team Role on TeamID to a principal (directly or through an IdP group).
type Binding struct {
	Role   Role
	TeamID string
}

// Workload identifies a workload principal: the agent or pool, its team and the worker pod.
type Workload struct {
	TeamID    string
	AgentID   string
	PoolID    string
	WorkerPod string
}

// Principal is an authenticated caller with its resolved bindings.
type Principal struct {
	ID    string
	Type  PrincipalType
	Email string
	Name  string
	// Bindings are team role bindings only.
	Bindings []Binding
	// ServiceAdmin is true for a service_admin binding.
	ServiceAdmin bool
	// SecurityOperator is true for a security_operator binding or a service-level API key.
	SecurityOperator bool
	// Workload is set for a workload principal.
	Workload *Workload
}

// TeamRoles returns the roles p holds on teamID.
func (p Principal) TeamRoles(teamID string) []Role {
	var out []Role
	for _, b := range p.Bindings {
		if b.TeamID == teamID && b.Role.TeamRole() && !slices.Contains(out, b.Role) {
			out = append(out, b.Role)
		}
	}
	return out
}

// TeamIDs returns the teams p is bound to, in binding order without repeats.
func (p Principal) TeamIDs() []string {
	var out []string
	for _, b := range p.Bindings {
		if b.Role.TeamRole() && !slices.Contains(out, b.TeamID) {
			out = append(out, b.TeamID)
		}
	}
	return out
}

// TeamFlags are the team guardrail flags that conditional grants read.
type TeamFlags struct {
	MayManageAwsAccounts  bool
	ViewerKubectl         bool
	MayAddExternalSystems bool
}

// WorkOrderRef holds the work order facts that work order grants read.
type WorkOrderRef struct {
	AssignedTeamID   string
	RepoOwnerTeamIDs []string
}

// Resource describes what is being accessed. For a team route TeamID and Flags are set. For a work
// order route WorkOrder is nil at route level and set once the use case has loaded the work order.
type Resource struct {
	Scope     Scope
	TeamID    string
	Flags     TeamFlags
	WorkOrder *WorkOrderRef
}

// Outcome is the result of a decision.
type Outcome int

// Outcomes. The HTTP layer maps Allow to the handler, NotFound to 404 and Deny to 403.
const (
	Deny Outcome = iota
	Allow
	NotFound
)

// Decision is an Outcome with the permission a denied caller lacks.
type Decision struct {
	Outcome            Outcome
	RequiredPermission Permission
}

// Allowed reports whether the decision allows the call.
func (d Decision) Allowed() bool { return d.Outcome == Allow }

// Requirement is what a route or use case requires: one permission, or any of several, and an
// optional operation-level condition (for example DeclareTeamExternalSystem's mayAddExternalSystems).
type Requirement struct {
	Permission Permission
	AnyOf      []Permission
	Condition  Condition
}

// Authorize decides whether p may use perm on res.
func Authorize(p Principal, perm Permission, res Resource) Decision {
	return Check(p, Requirement{Permission: perm}, res)
}

// Check decides whether p meets req on res, applying API contract §2 and §3 and the PRD §3 rule:
// a caller not bound to the team gets NotFound; a security operator is denied every team route, a workload
// principal on its own team and not found on another; a bound caller without the permission is denied; service routes deny all but service
// admins and the security operator column.
func Check(p Principal, req Requirement, res Resource) Decision {
	perms := req.AnyOf
	if len(perms) == 0 && req.Permission != "" {
		perms = []Permission{req.Permission}
	}
	deny := Decision{Outcome: Deny}
	if len(perms) > 0 {
		deny.RequiredPermission = perms[0]
	}
	switch res.Scope {
	case ScopePublic:
		return Decision{Outcome: Allow}
	case ScopeTeam:
		return checkTeam(p, perms, req.Condition, res, deny)
	case ScopeService:
		if len(perms) == 0 {
			return deny
		}
		return checkService(p, perms, deny)
	default:
		if len(perms) == 0 {
			return Decision{Outcome: Allow}
		}
		return checkAny(p, perms, res, deny)
	}
}

func checkTeam(p Principal, perms []Permission, cond Condition, res Resource, deny Decision) Decision {
	switch {
	case p.ServiceAdmin:
		return Decision{Outcome: Allow}
	case p.Workload != nil:
		// A workload principal holds nothing on team routes (§2): 403 on its own team's, and 404 on
		// any other team's, so it learns nothing about teams it does not belong to (RA-US-2b.2).
		if p.Workload.TeamID == res.TeamID {
			return deny
		}
		return Decision{Outcome: NotFound}
	case p.SecurityOperator:
		// A security operator is denied every team route, existing team or not (RA-US-1.4).
		return deny
	}
	roles := p.TeamRoles(res.TeamID)
	if len(roles) == 0 {
		return Decision{Outcome: NotFound}
	}
	if !opConditionHolds(cond, res) {
		return deny
	}
	for _, perm := range perms {
		for _, g := range catalogGrants[perm] {
			if slices.Contains(roles, g.Role) && teamConditionHolds(g.Condition, res) {
				return Decision{Outcome: Allow}
			}
		}
	}
	return deny
}

func checkService(p Principal, perms []Permission, deny Decision) Decision {
	if p.ServiceAdmin {
		return Decision{Outcome: Allow}
	}
	if p.SecurityOperator {
		for _, perm := range perms {
			if grantedTo(perm, RoleSecurityOperator) {
				return Decision{Outcome: Allow}
			}
		}
	}
	return deny
}

func checkAny(p Principal, perms []Permission, res Resource, deny Decision) Decision {
	if p.ServiceAdmin {
		return Decision{Outcome: Allow}
	}
	hidden := false
	for _, perm := range perms {
		if p.SecurityOperator && grantedTo(perm, RoleSecurityOperator) {
			return Decision{Outcome: Allow}
		}
		if w := p.Workload; w != nil {
			if cond, ok := workloadGrants[perm]; ok {
				if workOrderConditionHolds(cond, []string{w.TeamID}, res.WorkOrder) {
					return Decision{Outcome: Allow}
				}
				hidden = true
			}
			continue
		}
		for _, g := range catalogGrants[perm] {
			teams := teamsWithRole(p, g.Role)
			if len(teams) == 0 {
				continue
			}
			if workOrderConditionHolds(g.Condition, teams, res.WorkOrder) {
				return Decision{Outcome: Allow}
			}
			hidden = true
		}
	}
	if hidden && res.WorkOrder != nil && !seesWorkOrder(p, res.WorkOrder) {
		// The caller could act on some work orders but cannot see this one: never reveal it exists.
		return Decision{Outcome: NotFound}
	}
	return deny
}

// seesWorkOrder reports whether a team caller can read wo (work-orders:read for team roles holds
// for work orders assigned to, or touching repositories of, the caller's teams).
func seesWorkOrder(p Principal, wo *WorkOrderRef) bool {
	return p.Workload == nil && workOrderConditionHolds(CondAssignedOrOwnRepos, p.TeamIDs(), wo)
}

// workOrderConditionHolds evaluates a grant condition for a caller whose teams are teams. At route
// level (wo nil) a relational condition is deferred to the use case and holds.
func workOrderConditionHolds(cond Condition, teams []string, wo *WorkOrderRef) bool {
	switch cond {
	case "":
		return true
	case CondAssignedTeam:
		return wo == nil || (wo.AssignedTeamID != "" && slices.Contains(teams, wo.AssignedTeamID))
	case CondAssignedOrOwnRepos:
		if wo == nil || (wo.AssignedTeamID != "" && slices.Contains(teams, wo.AssignedTeamID)) {
			return true
		}
		for _, t := range wo.RepoOwnerTeamIDs {
			if slices.Contains(teams, t) {
				return true
			}
		}
	}
	return false
}

// teamConditionHolds evaluates a catalog grant condition on a team route.
func teamConditionHolds(cond Condition, res Resource) bool {
	switch cond {
	case "":
		return true
	case CondMayManageAwsAccounts:
		return res.Flags.MayManageAwsAccounts
	case CondViewerKubectl:
		return res.Flags.ViewerKubectl
	case CondAssignedOrOwnRepos, CondAssignedTeam:
		// On a team route the team is the caller's own; the use case filters to the work orders
		// that are assigned to it or touch its repositories.
		return true
	}
	return false
}

// opConditionHolds evaluates an operation-level condition (route table) on a team route.
func opConditionHolds(cond Condition, res Resource) bool {
	if cond == CondMayAddExternalSystems {
		return res.Flags.MayAddExternalSystems
	}
	return true
}

func grantedTo(perm Permission, role Role) bool {
	return slices.ContainsFunc(catalogGrants[perm], func(g Grant) bool { return g.Role == role })
}

func teamsWithRole(p Principal, role Role) []string {
	var out []string
	for _, b := range p.Bindings {
		if b.Role == role {
			out = append(out, b.TeamID)
		}
	}
	return out
}

// Effective returns the permissions p holds on teamID with the team's flags, for GET /v1/me (D42).
// Flag-dependent grants appear only when the flag is on; work order grants that depend on the work
// order appear for the roles that hold them. Client UX only: the API decides with Check.
func Effective(p Principal, teamID string, flags TeamFlags) []Permission {
	res := Resource{Scope: ScopeTeam, TeamID: teamID, Flags: flags}
	var out []Permission
	for _, perm := range catalogOrder {
		if Authorize(p, perm, res).Allowed() {
			out = append(out, perm)
		}
	}
	return out
}

// ServicePermissions returns the service-level permissions of p: every permission for a service
// admin, the security operator column for a security operator, and nothing otherwise.
func ServicePermissions(p Principal) []Permission {
	switch {
	case p.ServiceAdmin:
		return AllPermissions()
	case p.SecurityOperator:
		var out []Permission
		for _, perm := range catalogOrder {
			if grantedTo(perm, RoleSecurityOperator) {
				out = append(out, perm)
			}
		}
		return out
	}
	return nil
}

// CanGrant reports whether p may grant role on teamID. Only service admins grant service_admin and
// security_operator; team roles are granted by whoever holds members:write on the team.
func CanGrant(p Principal, role Role, teamID string) bool {
	switch {
	case role == RoleServiceAdmin || role == RoleSecurityOperator:
		return p.ServiceAdmin
	case !role.TeamRole() || teamID == "":
		return false
	}
	return Authorize(p, MembersWrite, Resource{Scope: ScopeTeam, TeamID: teamID}).Allowed()
}

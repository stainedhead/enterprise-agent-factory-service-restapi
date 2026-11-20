// Package authz is the single place Agent Factory access decisions are made. The API's route table
// names a Permission per route and calls Authorize; nothing else decides access. Clients may call
// Effective (exposed via GET /v1/me) to shape their UX, but the API's decision is authoritative.
package authz

import "slices"

// Role is one of the three roles defined in the solution design (section 5.2).
type Role string

const (
	RoleServiceAdmin Role = "service_admin"
	RoleTeamAdmin    Role = "team_admin"
	RoleTeamViewer   Role = "team_viewer"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleServiceAdmin, RoleTeamAdmin, RoleTeamViewer:
		return true
	}
	return false
}

// Permission names one action from the permission matrix (solution design section 5.3).
type Permission string

const (
	TeamsCreate         Permission = "teams:create"
	TeamsDelete         Permission = "teams:delete"
	TeamsRead           Permission = "teams:read"
	TeamsWrite          Permission = "teams:write"
	TeamsSuspend        Permission = "teams:suspend"
	AgentsRead          Permission = "agents:read"
	AgentsWrite         Permission = "agents:write"
	MembersRead         Permission = "members:read"
	MembersWrite        Permission = "members:write"
	SecretsReadMetadata Permission = "secrets:read-metadata"
	SecretsWrite        Permission = "secrets:write"
	IdentitiesRead      Permission = "identities:read"
	IdentitiesWrite     Permission = "identities:write"
	BudgetsRead         Permission = "budgets:read"
	BudgetsWrite        Permission = "budgets:write"
	BudgetsSetCeiling   Permission = "budgets:set-ceiling"
	UsageRead           Permission = "usage:read"
	LogsRead            Permission = "logs:read"
	LogsWrite           Permission = "logs:write"
	NotificationsRead   Permission = "notifications:read"
	NotificationsWrite  Permission = "notifications:write"
	APIKeysWrite        Permission = "api-keys:write"
	AuditRead           Permission = "audit:read"
	AdminSettings       Permission = "admin:settings"
	AdminModels         Permission = "admin:models"
	AdminExport         Permission = "admin:export"
	AdminAudit          Permission = "admin:audit"
)

var viewerPerms = []Permission{
	TeamsRead, AgentsRead, MembersRead, SecretsReadMetadata, IdentitiesRead,
	BudgetsRead, UsageRead, LogsRead, NotificationsRead, AuditRead,
}

var teamAdminPerms = append(slices.Clone(viewerPerms),
	TeamsWrite, TeamsSuspend, AgentsWrite, MembersWrite, SecretsWrite, IdentitiesWrite,
	BudgetsWrite, LogsWrite, NotificationsWrite, APIKeysWrite,
)

// serviceOnly are permissions no team-scoped role can hold.
var serviceOnly = []Permission{
	TeamsCreate, TeamsDelete, BudgetsSetCeiling, AdminSettings, AdminModels, AdminExport, AdminAudit,
}

// AllPermissions lists every permission, in matrix order.
func AllPermissions() []Permission {
	return append(slices.Clone(teamAdminPerms), serviceOnly...)
}

// Binding grants Role to a principal. TeamID is empty for service_admin and required otherwise.
type Binding struct {
	Role   Role
	TeamID string
}

// Principal is an authenticated caller (human via OIDC, or an API key) with its resolved bindings,
// including bindings inherited from IdP groups.
type Principal struct {
	Subject  string
	Bindings []Binding
}

// IsServiceAdmin reports whether p holds a global service_admin binding.
func (p Principal) IsServiceAdmin() bool {
	return slices.ContainsFunc(p.Bindings, func(b Binding) bool { return b.Role == RoleServiceAdmin })
}

func rolePerms(r Role) []Permission {
	switch r {
	case RoleServiceAdmin:
		return AllPermissions()
	case RoleTeamAdmin:
		return teamAdminPerms
	case RoleTeamViewer:
		return viewerPerms
	}
	return nil
}

// Authorize reports whether p may perform perm on teamID. Pass teamID "" for service-level actions
// (creating teams, admin endpoints); team-scoped roles never satisfy a service-level check.
func Authorize(p Principal, perm Permission, teamID string) bool {
	for _, b := range p.Bindings {
		switch {
		case b.Role == RoleServiceAdmin && b.TeamID == "":
			return true
		case b.TeamID != "" && b.TeamID == teamID && slices.Contains(rolePerms(b.Role), perm):
			return true
		}
	}
	return false
}

// Effective returns the permissions p holds on teamID, for GET /v1/me. Client UX only.
func Effective(p Principal, teamID string) []Permission {
	var out []Permission
	for _, perm := range AllPermissions() {
		if Authorize(p, perm, teamID) {
			out = append(out, perm)
		}
	}
	return out
}

// CanGrant reports whether p may grant role on teamID. Only service admins grant service_admin;
// team-admins grant team roles on their own team.
func CanGrant(p Principal, role Role, teamID string) bool {
	if role == RoleServiceAdmin {
		return p.IsServiceAdmin()
	}
	return role.Valid() && teamID != "" && Authorize(p, MembersWrite, teamID)
}

package mcp

// Tier is the risk tier of a server registration (PRD §7.5).
type Tier string

// Risk tiers, lowest first.
const (
	TierLow      Tier = "low"
	TierElevated Tier = "elevated"
	TierHigh     Tier = "high"
)

// Valid reports whether t is a known tier.
func (t Tier) Valid() bool { return t == TierLow || t == TierElevated || t == TierHigh }

func (t Tier) rank() int {
	switch t {
	case TierLow:
		return 0
	case TierElevated:
		return 1
	}
	return 2
}

// Max returns the higher of two tiers. An unknown tier counts as high.
func Max(a, b Tier) Tier {
	if a.rank() >= b.rank() {
		if !a.Valid() {
			return TierHigh
		}
		return a
	}
	return b
}

// ApprovalPolicy is the service-wide setting mcp.team_server_approval (PRD §14.3, MCP-OQ-4).
type ApprovalPolicy string

// Approval policies. TeamAll is the default.
const (
	// ApprovalTeamAll: a team-admin approves every team server.
	ApprovalTeamAll ApprovalPolicy = "team_all"
	// ApprovalTeamLowSecurityHigher: a team-admin approves low-tier servers; elevated and high go to
	// a service admin (security review).
	ApprovalTeamLowSecurityHigher ApprovalPolicy = "team_low_security_higher"
	// ApprovalSecurityAll: every team server goes to a security operator or service admin.
	ApprovalSecurityAll ApprovalPolicy = "security_all"
)

// Valid reports whether p is a known policy.
func (p ApprovalPolicy) Valid() bool {
	switch p {
	case ApprovalTeamAll, ApprovalTeamLowSecurityHigher, ApprovalSecurityAll:
		return true
	}
	return false
}

// Approver names who must approve a team server.
type Approver string

// Approvers.
const (
	ApproverTeamAdmin Approver = "team_admin"
	// ApproverSecurity is a security operator or a service admin.
	ApproverSecurity Approver = "security"
)

// RequiredApprover returns who approves a team server of the given tier under a policy. An unknown
// policy or tier fails closed to security.
func RequiredApprover(p ApprovalPolicy, t Tier) Approver {
	switch p {
	case ApprovalTeamAll:
		if t.Valid() {
			return ApproverTeamAdmin
		}
	case ApprovalTeamLowSecurityHigher:
		if t == TierLow {
			return ApproverTeamAdmin
		}
	}
	return ApproverSecurity
}

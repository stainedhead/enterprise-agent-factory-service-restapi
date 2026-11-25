package domain

// Service settings added by governed MCP servers (MCP gateway PRD Appendix B, API contract v1.6.0 §4.14).
// The database holds the authoritative CHECKs (-db 000024, service_settings_mcp_value); these specs give
// the early, readable error and the defaults. Writing them needs mcp-settings:write (contract §4.14).
const (
	// SettingMcpEnabled is "mcp.enabled": the feature switch for MCP servers.
	SettingMcpEnabled = "mcp.enabled"
	// SettingMcpTeamServerApproval is "mcp.team_server_approval": who approves team MCP servers.
	SettingMcpTeamServerApproval = "mcp.team_server_approval"
	// SettingMcpHostDenyList is "mcp.host_deny_list": host or *.domain globs an MCP server may not use.
	SettingMcpHostDenyList = "mcp.host_deny_list"
	// SettingMcpCorporateDomains is "mcp.corporate_domains": *.domain globs that count as the corporate network.
	SettingMcpCorporateDomains = "mcp.corporate_domains"
	// SettingMcpAllowedImageRegistries is "mcp.allowed_image_registries": registries hosted images may come from.
	SettingMcpAllowedImageRegistries = "mcp.allowed_image_registries"
	// SettingMcpImageSignaturePolicy is "mcp.image_signature_policy": required, or warn (non-production only).
	SettingMcpImageSignaturePolicy = "mcp.image_signature_policy"
	// SettingMcpDefaultAgentMode is "mcp.default_agent_mode": search (meta-tools) or full.
	SettingMcpDefaultAgentMode = "mcp.default_agent_mode"
	// SettingMcpSearchLimitMax is "mcp.search_limit_max": the largest afs_search result count.
	SettingMcpSearchLimitMax = "mcp.search_limit_max"
	// SettingMcpRefreshIntervalMinutes is "mcp.refresh_interval_minutes": inventory refresh without list-changed.
	SettingMcpRefreshIntervalMinutes = "mcp.refresh_interval_minutes"
	// SettingMcpLimits is "mcp.limits": default per-server limits.
	SettingMcpLimits = "mcp.limits"
	// SettingMcpQueryCaptureMaxDays is "mcp.query_capture_max_days": the longest query capture window.
	SettingMcpQueryCaptureMaxDays = "mcp.query_capture_max_days"
	// SettingMcpEventsRetentionMonths is "mcp.events_retention_months": months of MCP ledger kept.
	SettingMcpEventsRetentionMonths = "mcp.events_retention_months"
	// SettingGatewayMcpPlacementDefault is "gateway.mcp.placement_default": combined or separate.
	SettingGatewayMcpPlacementDefault = "gateway.mcp.placement_default"
	// SettingFederationAllowedAudiences is "federation.allowed_audiences": audiences a federated identity may request.
	SettingFederationAllowedAudiences = "federation.allowed_audiences"
)

// Values of mcp.team_server_approval.
const (
	// McpApprovalTeamAll lets team-admins approve team servers of every tier.
	McpApprovalTeamAll = "team_all"
	// McpApprovalTeamLowSecurityHigher lets team-admins approve low-tier team servers; security approves the rest.
	McpApprovalTeamLowSecurityHigher = "team_low_security_higher"
	// McpApprovalSecurityAll sends every team server to security.
	McpApprovalSecurityAll = "security_all"
)

const mcpGlobPattern = `^(\*\.)?[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`

// McpSettingSpecs lists the settings of MCP gateway PRD Appendix B in PRD order.
var McpSettingSpecs = []SettingSpec{
	{Key: SettingMcpEnabled, Type: "boolean", Default: "false", Allowed: "boolean"},
	{Key: SettingMcpTeamServerApproval, Type: "string", Default: `"team_all"`, Pattern: "^(team_all|team_low_security_higher|security_all)$", Allowed: "team_all, team_low_security_higher, security_all"},
	{Key: SettingMcpHostDenyList, Type: "array", Default: "[]", Items: "string", Pattern: mcpGlobPattern, Allowed: "host or *.domain globs"},
	{Key: SettingMcpCorporateDomains, Type: "array", Default: "[]", Items: "string", Pattern: mcpGlobPattern, Allowed: "*.domain globs"},
	{Key: SettingMcpAllowedImageRegistries, Type: "array", Default: "[]", Items: "string", Pattern: `^[A-Za-z0-9]([A-Za-z0-9.:/-]{0,251}[A-Za-z0-9])?$`, Allowed: "registry hosts"},
	{Key: SettingMcpImageSignaturePolicy, Type: "string", Default: `"required"`, Pattern: "^(required|warn)$", Allowed: "required, warn (non-production only)"},
	{Key: SettingMcpDefaultAgentMode, Type: "string", Default: `"search"`, Pattern: "^(search|full)$", Allowed: "search, full"},
	{Key: SettingMcpSearchLimitMax, Type: "integer", Default: "25", Min: ptr(int64(5)), Max: ptr(int64(50)), Allowed: "5 to 50"},
	{Key: SettingMcpRefreshIntervalMinutes, Type: "integer", Default: "15", Min: ptr(int64(1)), Max: ptr(int64(1440)), Allowed: "1 to 1440"},
	{Key: SettingMcpLimits, Type: "object", Default: `{"requestBytes":1048576,"responseBytes":8388608,"timeoutSeconds":60,"concurrentPerAgent":8}`, Allowed: "{requestBytes, responseBytes, timeoutSeconds, concurrentPerAgent}"},
	{Key: SettingMcpQueryCaptureMaxDays, Type: "integer", Default: "7", Min: ptr(int64(1)), Max: ptr(int64(14)), Allowed: "1 to 14"},
	{Key: SettingMcpEventsRetentionMonths, Type: "integer", Default: "13", Min: ptr(int64(3)), Max: ptr(int64(36)), Allowed: "3 to 36"},
	{Key: SettingGatewayMcpPlacementDefault, Type: "string", Default: `"combined"`, Pattern: "^(combined|separate)$", Allowed: "combined, separate"},
	{Key: SettingFederationAllowedAudiences, Type: "array", Default: "[]", Items: "string", Pattern: `^[\x21-\x7e]{1,253}$`, Allowed: "exact audiences or globs"},
}

// IsMcpSetting reports whether key is one of the Appendix B keys, which need mcp-settings:write and emit
// mcp_settings.changed (CD-49).
func IsMcpSetting(key string) bool {
	for _, s := range McpSettingSpecs {
		if s.Key == key {
			return true
		}
	}
	return false
}

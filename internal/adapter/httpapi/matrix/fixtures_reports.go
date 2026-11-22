package matrix

// Cases of M3-H (reports, RA-6). All four operations are reads: no audit row and no outbox event.

func init() {
	Register(Case{Op: "GetUsage", Path: "/v1/teams/{teamId}/usage?groupBy=model"})
	Register(Case{Op: "GetTeamReport", Path: "/v1/teams/{teamId}/reports/usage?range=24h&groupBy=agent,tag:business-unit"})
	Register(Case{Op: "GetAgentReport", Path: "/v1/teams/{teamId}/agents/{agentId}/reports/activity?range=7d", Prepare: agentPrep})
	Register(Case{Op: "GetServiceReport", Path: "/v1/reports/traffic?range=30d&groupBy=team"})
}

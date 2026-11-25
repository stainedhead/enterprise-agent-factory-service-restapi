package httpapi

// PendingMcpOps are the API contract v1.6.0 §4.14 operations whose handlers land with the implementation
// change of release 1.3.0 (specs/261125-mcp-gateway). The contract change merges first so the CLI and console
// can build from it; until then these answer 501 with details.reason not_implemented, exactly as an unbound
// placeholder does. The list may only shrink.
var PendingMcpOps = []string{
	"ListMcpServers", "CreateMcpServer", "GetMcpServer", "UpdateMcpServer", "DeleteMcpServer",
	"ImportMcpServerManifest", "PublishMcpServer", "RetireMcpServer", "SuspendMcpServer", "ResumeMcpServer",
	"RefreshMcpInventory", "ListMcpPrimitives", "GetMcpPrimitiveChange", "ApproveMcpPrimitive", "RejectMcpPrimitive",
	"PutMcpPolicy", "PutMcpServerCredential", "ApproveMcpServer", "RejectMcpServer", "RequestMcpServerChanges",
	"ListMcpReviews", "ListMcpEvents", "GetMcpUsageReport", "StartMcpQueryCapture", "StopMcpQueryCapture",
	"GetMcpQueryCapture", "ListTeamMcpServers", "GetTeamMcpServer", "PutTeamMcpServer", "CreateTeamMcpServer",
	"UpdateTeamMcpServer", "DeleteTeamMcpServer", "SubmitMcpServerForReview", "ApproveTeamMcpServer",
	"RejectTeamMcpServer", "RefreshTeamMcpInventory", "ListTeamMcpPrimitives", "ApproveTeamMcpPrimitive",
	"RejectTeamMcpPrimitive", "PutTeamMcpPolicy", "PutTeamMcpServerCredential", "StartMcpOAuthConnect",
	"CompleteMcpOAuthConnect", "DisconnectMcpOAuth", "TestMcpFederation", "GetEffectiveAgentMcp", "GetEffectivePoolMcp",
	"ListTeamMcpEvents", "GetTeamMcpUsageReport", "ListAgentFederatedIdentities", "PutAgentFederatedIdentity",
	"DeleteAgentFederatedIdentity", "ListPoolFederatedIdentities", "PutPoolFederatedIdentity",
	"DeletePoolFederatedIdentity",
}

// BindMcpPending binds the pending operations to the not_implemented answer.
func BindMcpPending() Registrar {
	return func(g *Registry) error {
		for _, op := range PendingMcpOps {
			if err := g.Handle(op, placeholder(op)); err != nil {
				return err
			}
		}
		return nil
	}
}

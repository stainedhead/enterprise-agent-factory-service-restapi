package httpapi

// PendingModelEndpointOps are the API contract v1.5.0 §4.13 operations whose handlers land with the
// implementation change of release 1.2.0 (specs/261125-model-endpoint-gateway). The contract change merges
// first so the CLI and console can build from it; until then these answer 501 with details.reason
// not_implemented, exactly as an unbound placeholder does. The list may only shrink.
var PendingModelEndpointOps = []string{
	"ListModelEndpoints", "CreateModelEndpoint", "GetModelEndpoint", "UpdateModelEndpoint", "DeleteModelEndpoint",
	"PutEndpointModels", "CreateEndpointModelPrice", "ListEndpointPriceProposals", "ApproveEndpointPriceProposal",
	"RejectEndpointPriceProposal", "ListEndpointTeams", "PutModelEndpointCredential", "RotateModelEndpointCredential",
	"PutEndpointFederation", "ImportProviderStatement", "ListInferenceProviders", "UpdateInferenceProvider",
	"PutTeamGatewaySettings", "ListTeamModelEndpoints", "PutTeamModelEndpoints", "CreateTeamModelEndpoint",
	"UpdateTeamModelEndpoint", "DeleteTeamModelEndpoint", "PutTeamEndpointModels", "PutTeamModelEndpointCredential",
	"GetInferenceReport", "GetTeamInferenceReport",
}

// BindModelEndpointsPending binds the pending operations to the not_implemented answer.
func BindModelEndpointsPending() Registrar {
	return func(g *Registry) error {
		for _, op := range PendingModelEndpointOps {
			if err := g.Handle(op, placeholder(op)); err != nil {
				return err
			}
		}
		return nil
	}
}

package httpapi

import (
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// mcpRequest is the v1.6.0 mcp object of the agent and pool create and update bodies (contract §4.14).
type mcpRequest struct {
	Access    *string                 `json:"access"`
	ServerIDs *[]string               `json:"serverIds"`
	Mode      *string                 `json:"mode"`
	Blocks    *[]domain.McpAgentBlock `json:"blocks"`
}

// refuseMcpUntilBuilt answers 501 not_implemented for a body that sets mcp, until the implementation change of
// release 1.3.0 binds it (specs/261125-mcp-gateway). An mcp member is refused, never silently ignored.
func refuseMcpUntilBuilt(w http.ResponseWriter, r *http.Request, m *mcpRequest) bool {
	if m == nil {
		return false
	}
	e := domain.Internal(nil)
	e.Message = "mcp is not implemented yet"
	e.Reason = domain.ReasonNotImplemented
	writeErrorStatus(w, r, e, http.StatusNotImplemented)
	return true
}

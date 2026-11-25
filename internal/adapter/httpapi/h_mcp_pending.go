package httpapi

// PendingMcpOps are the API contract v1.6.0 §4.14 operations still without a handler in release 1.3.0
// (specs/261125-mcp-gateway): the OAuth connect flow, which needs protected-resource discovery (RFC 9728) and a
// code exchange against the server's authorization server. They answer 501 with details.reason not_implemented,
// exactly as an unbound placeholder does. The list may only shrink.
var PendingMcpOps = []string{"StartMcpOAuthConnect", "CompleteMcpOAuthConnect"}

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

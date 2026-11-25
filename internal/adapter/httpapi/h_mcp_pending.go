package httpapi

// PendingMcpOps are the API contract v1.6.0 §4.14 operations still without a handler. Release 1.3.0 binds all of
// them (specs/261125-mcp-gateway); the list stays, empty, so the release can be checked against it.
var PendingMcpOps = []string{}

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

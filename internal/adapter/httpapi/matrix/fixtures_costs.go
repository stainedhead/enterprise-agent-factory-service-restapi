package matrix

// Cases of M4-C5 (BI-7). Both operations are reads: no audit row and no outbox event.

func init() {
	Register(Case{Op: "ListCosts", Path: "/v1/teams/{teamId}/costs"})
	Register(Case{Op: "ListCostReconciliations", Path: "/v1/admin/cost-reconciliations?status=over_threshold"})
}

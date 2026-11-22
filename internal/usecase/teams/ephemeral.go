package teams

import "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"

// checkBudget enforces the lifetime budget rule of an ephemeral team (RA-US-10.1, RA-8): an
// ephemeral team is created with a lifetime budget between zero (exclusive) and its ceiling
// (maxBudgetUsd); a persistent team is created without one, because its budget is PutBudget's.
// The database backs this with a deferred constraint (ephemeral_team_lifetime_budget) that blocks at
// the cap, so the team and its budget are written in the one create transaction.
func (s *Service) checkBudget(t domain.Team, b *BudgetInput) error {
	switch {
	case t.Kind == domain.TeamKindEphemeral && b == nil:
		return domain.Validation(domain.FieldError{Field: "budget", Reason: "an ephemeral team needs a lifetime budget"})
	case t.Kind == domain.TeamKindPersistent && b != nil:
		return domain.Validation(domain.FieldError{Field: "budget", Reason: "is only for an ephemeral team; set a persistent team's budget with PutBudget"})
	case b == nil:
		return nil
	case b.Amount.Micros() <= 0:
		return domain.Validation(domain.FieldError{Field: "budget.amountUsd", Reason: "must be above zero"})
	case b.Amount.Cmp(t.MaxBudget) > 0:
		return domain.Validation(domain.FieldError{Field: "budget.amountUsd", Reason: "is above maxBudgetUsd " + t.MaxBudget.String()})
	}
	return nil
}

// Package budgets implements GetBudget and PutBudget (API contract §4.9, RA-14). A team has one
// budget, monthly or lifetime. The rules live in domain.Budget: the amount stays within the effective
// ceiling (an unexpired override counts), notify-only exhaustion needs teams:set-guardrails and a
// persistent team, and an ephemeral team keeps a lifetime budget that blocks. The row is desired
// state: the database notifies the reconciler, which carries a changed amount into the spend counter's
// limit (M3-F2). Contract §7 has no budget-changed event, so a PutBudget writes the row and an audit
// record (CD-7); budget.threshold_crossed, .exhausted, .reset and .ceiling_override_expired come from
// the gateway and the reconciler.
package budgets

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kind = "budgets"

// Repository is the budget store. Every method runs in the transaction it is given, so row-level
// security applies with the caller's session.
type Repository interface {
	// Team returns the facts of the team a budget rule needs (NotFound when it does not exist).
	Team(ctx context.Context, tx port.Tx, teamID string) (domain.BudgetTeam, error)
	// Get returns the team's budget (NotFound "budget" when it has none); forUpdate locks the row.
	Get(ctx context.Context, tx port.Tx, teamID string, forUpdate bool) (domain.Budget, error)
	// Insert stores a team's first budget.
	Insert(ctx context.Context, tx port.Tx, b domain.Budget) error
	// Update stores the replaced budget; PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, b domain.Budget, expect domain.Version) error
	// Counter returns the spend counter of the budget's period starting at periodStart; the zero
	// counter when the period has none yet.
	Counter(ctx context.Context, tx port.Tx, budgetID string, periodStart time.Time) (domain.SpendCounter, error)
}

// Service is the budget use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// check re-applies the route's authorization in the use case: team scope, the caller's role on the
// team and the permission. An unbound caller learns nothing (NotFound). A denial is returned as
// AccessDenied and not audited here: the pipeline's DeniedAuditor.Stage writes the one denied record
// of every 403 a handler produces (RA-US-1.7), and a second one would double it.
func (s *Service) check(c acting.Caller, teamID string, perm authz.Permission) error {
	switch authz.Authorize(c.Principal, perm, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}).Outcome {
	case authz.Allow:
		return nil
	case authz.NotFound:
		return domain.NotFound("team")
	}
	return domain.AccessDenied(string(perm))
}

func auditOf(b domain.Budget) map[string]any {
	m := map[string]any{"period": b.Period, "amountUsd": b.Amount.String(), "thresholds": b.Thresholds, "onExhaust": b.OnExhaust}
	if b.ExpiresAt != nil {
		m["expiresAt"] = b.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return m
}

// position reads the budget's current-period counter and computes its position.
func (s *Service) position(ctx context.Context, tx port.Tx, b domain.Budget) (domain.BudgetPosition, error) {
	now := s.Now()
	start, _ := b.CurrentPeriod(now)
	ctr, err := s.Repo.Counter(ctx, tx, b.ID, start)
	if err != nil {
		return domain.BudgetPosition{}, err
	}
	return domain.NewBudgetPosition(b, ctr, now), nil
}

// Get implements GetBudget.
func (s *Service) Get(ctx context.Context, c acting.Caller, teamID string) (domain.BudgetPosition, error) {
	if err := s.check(c, teamID, authz.BudgetsRead); err != nil {
		return domain.BudgetPosition{}, err
	}
	var out domain.BudgetPosition
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		b, err := s.Repo.Get(ctx, tx, teamID, false)
		if err != nil {
			return err
		}
		out, err = s.position(ctx, tx, b)
		return err
	})
	return out, err
}

// Put implements PutBudget: it creates the team's budget or replaces it. expect is the optional
// If-Match (CD-2): with it, a missing or newer budget is PreconditionFailed.
func (s *Service) Put(ctx context.Context, c acting.Caller, teamID string, p domain.PutBudgetParams, expect *domain.Version) (domain.BudgetPosition, error) {
	if err := s.check(c, teamID, authz.BudgetsWrite); err != nil {
		return domain.BudgetPosition{}, err
	}
	if p.RequestsNotify() {
		if err := s.check(c, teamID, authz.TeamsSetGuardrails); err != nil {
			return domain.BudgetPosition{}, err
		}
	}
	var out domain.BudgetPosition
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		team, err := s.Repo.Team(ctx, tx, teamID)
		if err != nil {
			return err
		}
		cur, err := s.Repo.Get(ctx, tx, teamID, true)
		var after domain.Budget
		var before any
		switch {
		case domain.KindOf(err) == domain.KindNotFound:
			if expect != nil {
				return domain.PreconditionFailed()
			}
			id, err := s.IDs.NewID(domain.PrefixBudget)
			if err != nil {
				return err
			}
			if after, err = domain.NewBudget(id.String(), teamID, p, team, s.Now()); err != nil {
				return err
			}
			if err := s.Repo.Insert(ctx, tx, after); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			if expect != nil {
				if err := domain.CheckVersion(cur.Version, *expect); err != nil {
					return err
				}
			}
			if after, err = cur.Replace(p, team, s.Now()); err != nil {
				return err
			}
			if err := s.Repo.Update(ctx, tx, after, cur.Version); err != nil {
				return err
			}
			before = auditOf(cur)
		}
		stored, err := s.Repo.Get(ctx, tx, teamID, false)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("PutBudget", kind, stored.ID, teamID, before, auditOf(stored))); err != nil {
			return err
		}
		out, err = s.position(ctx, tx, stored)
		return err
	})
	return out, err
}

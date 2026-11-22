// Package budgetstest is an in-memory budgets.Repository for use case tests.
package budgetstest

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/budgets"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repo is an in-memory budget store.
type Repo struct {
	// Teams maps a team id to its facts; a team that is not here does not exist.
	Teams map[string]domain.BudgetTeam
	// Budgets maps a team id to its budget.
	Budgets map[string]domain.Budget
	// Counters maps a budget id to the counter every period of it returns.
	Counters map[string]domain.SpendCounter
	// Locked records the forUpdate reads.
	Locked int
}

var _ budgets.Repository = (*Repo)(nil)

// New returns an empty store.
func New() *Repo {
	return &Repo{Teams: map[string]domain.BudgetTeam{}, Budgets: map[string]domain.Budget{}, Counters: map[string]domain.SpendCounter{}}
}

// Team implements budgets.Repository.
func (r *Repo) Team(_ context.Context, _ port.Tx, teamID string) (domain.BudgetTeam, error) {
	t, ok := r.Teams[teamID]
	if !ok {
		return domain.BudgetTeam{}, domain.NotFound("team")
	}
	return t, nil
}

// Get implements budgets.Repository.
func (r *Repo) Get(_ context.Context, _ port.Tx, teamID string, forUpdate bool) (domain.Budget, error) {
	if forUpdate {
		r.Locked++
	}
	b, ok := r.Budgets[teamID]
	if !ok {
		return domain.Budget{}, domain.NotFound("budget")
	}
	return b, nil
}

// Insert implements budgets.Repository.
func (r *Repo) Insert(_ context.Context, _ port.Tx, b domain.Budget) error {
	if _, ok := r.Budgets[b.TeamID]; ok {
		return domain.Conflict("duplicate", "the team already has a budget", nil)
	}
	r.Budgets[b.TeamID] = b
	return nil
}

// Update implements budgets.Repository; like the database it bumps the version.
func (r *Repo) Update(_ context.Context, _ port.Tx, b domain.Budget, expect domain.Version) error {
	cur, ok := r.Budgets[b.TeamID]
	if !ok {
		return domain.NotFound("budget")
	}
	if cur.Version != expect {
		return domain.PreconditionFailed()
	}
	b.Version = cur.Version + 1
	r.Budgets[b.TeamID] = b
	return nil
}

// Counter implements budgets.Repository.
func (r *Repo) Counter(_ context.Context, _ port.Tx, budgetID string, _ time.Time) (domain.SpendCounter, error) {
	return r.Counters[budgetID], nil
}

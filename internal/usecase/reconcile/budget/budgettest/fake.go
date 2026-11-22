// Package budgettest holds in-memory fakes for the budget reconciler's ports.
package budgettest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/budget"
)

// Calls records the order of calls across the fakes of one test.
type Calls struct {
	mu   sync.Mutex
	List []string
}

// Add records one call.
func (c *Calls) Add(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.List = append(c.List, fmt.Sprintf(format, args...))
}

// Store is a budget.Store whose answers a test sets and whose calls it reads.
type Store struct {
	Calls *Calls

	Budgets     []domain.Budget
	Limits      []budget.LimitChange
	Released    int64
	Expiries    []budget.OverrideExpiry
	Teams       []budget.TeamBudget
	ErrOn       map[string]error // method name → error
	OpenedStart map[string][]time.Time
}

var _ budget.Store = (*Store)(nil)

// NewStore returns a Store that records into calls.
func NewStore(calls *Calls) *Store {
	return &Store{Calls: calls, ErrOn: map[string]error{}, OpenedStart: map[string][]time.Time{}}
}

// LiveBudgets implements budget.Store.
func (s *Store) LiveBudgets(context.Context) ([]domain.Budget, error) {
	s.Calls.Add("LiveBudgets")
	return s.Budgets, s.ErrOn["LiveBudgets"]
}

// OpenPeriod implements budget.Store.
func (s *Store) OpenPeriod(_ context.Context, b domain.Budget, start time.Time, announce bool) (bool, error) {
	s.Calls.Add("OpenPeriod %s %s announce=%v", b.ID, start.Format("2006-01-02T15:04"), announce)
	if err := s.ErrOn["OpenPeriod"]; err != nil {
		return false, err
	}
	s.OpenedStart[b.ID] = append(s.OpenedStart[b.ID], start)
	return announce, nil
}

// CarryLimits implements budget.Store.
func (s *Store) CarryLimits(context.Context, time.Time) ([]budget.LimitChange, error) {
	s.Calls.Add("CarryLimits")
	return s.Limits, s.ErrOn["CarryLimits"]
}

// ReleaseExpired implements budget.Store.
func (s *Store) ReleaseExpired(context.Context) (int64, error) {
	s.Calls.Add("ReleaseExpired")
	return s.Released, s.ErrOn["ReleaseExpired"]
}

// ExpireOverrides implements budget.Store.
func (s *Store) ExpireOverrides(context.Context, time.Time) ([]budget.OverrideExpiry, error) {
	s.Calls.Add("ExpireOverrides")
	return s.Expiries, s.ErrOn["ExpireOverrides"]
}

// Exhaustion implements budget.Store.
func (s *Store) Exhaustion(context.Context, time.Time) ([]budget.TeamBudget, error) {
	s.Calls.Add("Exhaustion")
	return s.Teams, s.ErrOn["Exhaustion"]
}

// MarkExhausted implements budget.Store.
func (s *Store) MarkExhausted(_ context.Context, teamID string) error {
	s.Calls.Add("MarkExhausted %s", teamID)
	return s.ErrOn["MarkExhausted"]
}

// ClearExhausted implements budget.Store.
func (s *Store) ClearExhausted(_ context.Context, teamID string) error {
	s.Calls.Add("ClearExhausted %s", teamID)
	return s.ErrOn["ClearExhausted"]
}

// IAM is a budget.RoleDeny that remembers which roles are denied.
type IAM struct {
	Calls  *Calls
	Denied map[string]bool // path+role → denied
	Err    map[string]error
}

var _ budget.RoleDeny = (*IAM)(nil)

// NewIAM returns an IAM that records into calls.
func NewIAM(calls *Calls) *IAM {
	return &IAM{Calls: calls, Denied: map[string]bool{}, Err: map[string]error{}}
}

// DenyBedrock implements budget.RoleDeny.
func (i *IAM) DenyBedrock(_ context.Context, path, role string) error {
	i.Calls.Add("DenyBedrock %s%s", path, role)
	if err := i.Err["DenyBedrock"]; err != nil {
		return err
	}
	i.Denied[path+role] = true
	return nil
}

// LiftBedrockDeny implements budget.RoleDeny.
func (i *IAM) LiftBedrockDeny(_ context.Context, path, role string) error {
	i.Calls.Add("LiftBedrockDeny %s%s", path, role)
	if err := i.Err["LiftBedrockDeny"]; err != nil {
		return err
	}
	delete(i.Denied, path+role)
	return nil
}

// Package schedulestest holds the in-memory store and report source of the schedule runner's tests.
package schedulestest

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/schedules"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reports"
)

// Store implements schedules.Store.
type Store struct {
	mu   sync.Mutex
	Rows map[string]domain.ReportSchedule
	// Locked ids are claimed by another runner.
	Locked map[string]bool
	// Recorded lists every outcome in order.
	Recorded []Recorded
	// DueErr, ClaimErr and RecordErr are returned by the matching method when set.
	DueErr, ClaimErr, RecordErr error
}

// Recorded is one stored outcome.
type Recorded struct {
	ID      string
	Outcome schedules.Outcome
}

var _ schedules.Store = (*Store)(nil)

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{Rows: map[string]domain.ReportSchedule{}, Locked: map[string]bool{}}
}

// DueIDs implements schedules.Store.
func (s *Store) DueIDs(_ context.Context, _ port.Tx, now time.Time, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.DueErr != nil {
		return nil, s.DueErr
	}
	var due []domain.ReportSchedule
	for _, r := range s.Rows {
		if r.Due(now) {
			due = append(due, r)
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].NextRunAt.Equal(due[j].NextRunAt) {
			return due[i].NextRunAt.Before(due[j].NextRunAt)
		}
		return due[i].ID < due[j].ID
	})
	var ids []string
	for _, r := range due {
		if len(ids) < limit {
			ids = append(ids, r.ID)
		}
	}
	return ids, nil
}

// Claim implements schedules.Store.
func (s *Store) Claim(_ context.Context, _ port.Tx, id string, now time.Time) (domain.ReportSchedule, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ClaimErr != nil {
		return domain.ReportSchedule{}, false, s.ClaimErr
	}
	r, ok := s.Rows[id]
	if !ok || s.Locked[id] || !r.Due(now) {
		return domain.ReportSchedule{}, false, nil
	}
	return r, true, nil
}

// Record implements schedules.Store.
func (s *Store) Record(_ context.Context, tx port.Tx, id string, o schedules.Outcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.RecordErr != nil {
		return s.RecordErr
	}
	r := s.Rows[id]
	apply := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		run := o.RunAt
		r.LastRunAt, r.LastStatus, r.LastError = &run, o.Status, o.Error
		if o.NextRunAt != nil {
			r.NextRunAt = *o.NextRunAt
		}
		s.Rows[id] = r
		s.Recorded = append(s.Recorded, Recorded{ID: id, Outcome: o})
	}
	// Like a real transaction, the outcome becomes visible only when it commits.
	tx.(interface{ OnCommit(func()) }).OnCommit(apply)
	return nil
}

// Reports is an in-memory report source that also implements reports.Repository, so the on-demand
// report service and the schedule runner can read the same rows.
type Reports struct {
	mu sync.Mutex
	// Rows and Totals are what every query returns.
	Rows   []domain.ReportRow
	Totals []any
	// Queries lists every query run, in order.
	Queries []domain.ReportQuery
	// Err is returned by Run when set.
	Err error
}

var (
	_ schedules.ReportSource = (*Reports)(nil)
	_ reports.Repository     = (*Reports)(nil)
)

// Run implements schedules.ReportSource and reports.Repository.
func (r *Reports) Run(_ context.Context, _ port.Tx, q domain.ReportQuery, offset, limit int) (reports.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Queries = append(r.Queries, q)
	if r.Err != nil {
		return reports.Result{}, r.Err
	}
	rows := r.Rows
	if offset > len(rows) {
		offset = len(rows)
	}
	rows = rows[offset:]
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return reports.Result{Rows: rows, Totals: r.Totals}, nil
}

// TeamIsEphemeral implements reports.Repository.
func (*Reports) TeamIsEphemeral(context.Context, port.Tx, string) (bool, error) { return false, nil }

// AgentExists implements reports.Repository.
func (*Reports) AgentExists(context.Context, port.Tx, string, string) error { return nil }

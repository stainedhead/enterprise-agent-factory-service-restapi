// Package snapshottest holds in-memory fakes of the snapshot manager's ports.
package snapshottest

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/snapshot"
)

// Store is an in-memory snapshot.Store.
type Store struct {
	mu   sync.Mutex
	Rows []*snapshot.Row
	// Schedules maps agent id to its schedule.
	Schedules map[string]string
	// Clock stamps created rows.
	Clock func() time.Time
	n     int
}

// NewStore returns an empty Store.
func NewStore(clock func() time.Time) *Store {
	return &Store{Schedules: map[string]string{}, Clock: clock}
}

// Scheduled implements snapshot.Store.
func (s *Store) Scheduled(_ context.Context) ([]snapshot.Scheduled, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []snapshot.Scheduled
	for id, sched := range s.Schedules {
		if sched == snapshot.ScheduleOff {
			continue
		}
		item := snapshot.Scheduled{AgentID: id, Schedule: sched}
		for _, r := range s.Rows {
			if r.AgentID == id && r.Reason == snapshot.ReasonScheduled && (item.Last == nil || r.RequestedAt.After(*item.Last)) {
				at := r.RequestedAt
				item.Last = &at
			}
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}

// Open implements snapshot.Store.
func (s *Store) Open(_ context.Context) ([]snapshot.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []snapshot.Row
	for _, r := range s.Rows {
		if r.State == snapshot.StateRequested || r.State == snapshot.StatePending {
			out = append(out, *r)
		}
	}
	return out, nil
}

// Completed implements snapshot.Store.
func (s *Store) Completed(_ context.Context, agentID string) ([]snapshot.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []snapshot.Row
	for _, r := range s.Rows {
		if r.AgentID == agentID && r.State == snapshot.StateCompleted {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestedAt.After(out[j].RequestedAt) })
	return out, nil
}

// ForAgent implements snapshot.Store.
func (s *Store) ForAgent(_ context.Context, agentID, reason string) ([]snapshot.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []snapshot.Row
	for _, r := range s.Rows {
		if r.AgentID == agentID && r.Reason == reason && r.State != snapshot.StateFailed && r.State != snapshot.StateDeleted {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RequestedAt.After(out[j].RequestedAt) })
	return out, nil
}

// Create implements snapshot.Store.
func (s *Store) Create(_ context.Context, teamID, agentID, reason string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	id := fmt.Sprintf("vsnap_%026d", s.n)
	s.Rows = append(s.Rows, &snapshot.Row{ID: id, TeamID: teamID, AgentID: agentID, Reason: reason, State: snapshot.StateRequested, RequestedAt: s.Clock()})
	return id, nil
}

func (s *Store) find(id string) (*snapshot.Row, error) {
	for _, r := range s.Rows {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, fmt.Errorf("no snapshot %s", id)
}

// MarkPending implements snapshot.Store.
func (s *Store) MarkPending(_ context.Context, id, ebsID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.find(id)
	if err != nil {
		return err
	}
	r.State, r.EBSID = snapshot.StatePending, ebsID
	return nil
}

// MarkCompleted implements snapshot.Store.
func (s *Store) MarkCompleted(_ context.Context, id, ebsID string, size int, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.find(id)
	if err != nil {
		return err
	}
	r.State, r.EBSID, r.SizeGiB = snapshot.StateCompleted, ebsID, size
	return nil
}

// MarkFailed implements snapshot.Store.
func (s *Store) MarkFailed(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.find(id)
	if err != nil {
		return err
	}
	r.State = snapshot.StateFailed
	return nil
}

// MarkDeleted implements snapshot.Store.
func (s *Store) MarkDeleted(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.find(id)
	if err != nil {
		return err
	}
	r.State = snapshot.StateDeleted
	return nil
}

// States returns the rows' states by id.
func (s *Store) States() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, r := range s.Rows {
		out[r.ID] = r.State
	}
	return out
}

// Add appends a row (for arranging a test).
func (s *Store) Add(r snapshot.Row) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Rows = append(s.Rows, &r)
}

// Cluster is a fake snapshot.Cluster: a VolumeSnapshot becomes ready when the test says so.
type Cluster struct {
	mu        sync.Mutex
	Prepared  []string
	Requested map[string]bool
	Deleted   []string
	// Ready maps a snapshot name to its status once the test completes it.
	Ready map[string]snapshot.Status
	Err   error
}

// NewCluster returns an empty fake.
func NewCluster() *Cluster {
	return &Cluster{Requested: map[string]bool{}, Ready: map[string]snapshot.Status{}}
}

// Prepare implements snapshot.Cluster.
func (c *Cluster) Prepare(_ context.Context, d agent.Desired) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Prepared = append(c.Prepared, d.ID)
	return c.Err
}

// Request implements snapshot.Cluster.
func (c *Cluster) Request(_ context.Context, _ agent.Desired, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Err != nil {
		return c.Err
	}
	c.Requested[name] = true
	return nil
}

// Status implements snapshot.Cluster.
func (c *Cluster) Status(_ context.Context, _ agent.Desired, name string) (snapshot.Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, ok := c.Ready[name]; ok {
		st.Exists = true
		return st, nil
	}
	return snapshot.Status{Exists: c.Requested[name]}, nil
}

// Delete implements snapshot.Cluster.
func (c *Cluster) Delete(_ context.Context, _ agent.Desired, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Deleted = append(c.Deleted, name)
	delete(c.Requested, name)
	return c.Err
}

// Complete makes the named snapshot ready with an EBS id.
func (c *Cluster) Complete(name, ebsID string, size int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Ready[name] = snapshot.Status{EBSID: ebsID, Ready: true, SizeGiB: size}
}

// Handle gives the named snapshot an EBS id before it is ready.
func (c *Cluster) Handle(name, ebsID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Ready[name] = snapshot.Status{EBSID: ebsID}
}

// Fail makes the named snapshot report an error.
func (c *Cluster) Fail(name, msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Ready[name] = snapshot.Status{Error: msg}
}

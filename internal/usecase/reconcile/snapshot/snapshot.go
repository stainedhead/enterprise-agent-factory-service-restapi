// Package snapshot manages the EBS snapshots of agent volumes (RC-16): the daily or weekly schedule, manual
// requests and the final snapshot of a delete, each recorded in agent_volume_snapshots, tagged with the
// agent's tags and pruned to the agent's snapshotRetain.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/agent"
)

// Reasons and states of a snapshot row (database 000004).
const (
	ReasonScheduled = "scheduled"
	ReasonManual    = "manual"
	ReasonPreDelete = "pre_delete"
	ReasonPreChange = "pre_change"

	StateRequested = "requested"
	StatePending   = "pending"
	StateCompleted = "completed"
	StateFailed    = "failed"
	StateDeleted   = "deleted"
)

// Schedules (agents.snapshot_schedule) and their intervals.
const (
	ScheduleOff    = "off"
	ScheduleDaily  = "daily"
	ScheduleWeekly = "weekly"
)

// Interval is how often a schedule takes a snapshot; zero for off.
func Interval(schedule string) time.Duration {
	switch schedule {
	case ScheduleDaily:
		return 24 * time.Hour
	case ScheduleWeekly:
		return 7 * 24 * time.Hour
	}
	return 0
}

// StuckAfter is how long a snapshot may stay requested or pending before it is marked failed.
const StuckAfter = time.Hour

// Row is one row of agent_volume_snapshots.
type Row struct {
	ID          string
	TeamID      string
	AgentID     string
	Reason      string
	State       string
	EBSID       string
	SizeGiB     int
	RequestedAt time.Time
}

// Scheduled is an agent with a snapshot schedule and the time of its latest scheduled snapshot.
type Scheduled struct {
	AgentID  string
	Schedule string
	// Last is the request time of the newest scheduled snapshot; nil when there is none yet.
	Last *time.Time
}

// Store is the database side (adapter/postgres/snapshots.go).
type Store interface {
	// Scheduled lists the live agents whose schedule is not off.
	Scheduled(ctx context.Context) ([]Scheduled, error)
	// Open lists the rows in state requested or pending, oldest first.
	Open(ctx context.Context) ([]Row, error)
	// Completed lists the agent's completed rows, newest first.
	Completed(ctx context.Context, agentID string) ([]Row, error)
	// ForAgent returns the agent's rows of a reason that are not failed or deleted, newest first.
	ForAgent(ctx context.Context, agentID, reason string) ([]Row, error)
	// Create inserts a row in state requested and returns its id.
	Create(ctx context.Context, teamID, agentID, reason string) (string, error)
	// MarkPending records the EBS snapshot id once AWS accepted the request.
	MarkPending(ctx context.Context, id, ebsID string) error
	// MarkCompleted records a ready snapshot.
	MarkCompleted(ctx context.Context, id, ebsID string, sizeGiB int, at time.Time) error
	// MarkFailed marks a snapshot that will not complete.
	MarkFailed(ctx context.Context, id string) error
	// MarkDeleted marks a pruned snapshot; the row stays as history.
	MarkDeleted(ctx context.Context, id string) error
}

// Status is the Kubernetes side's view of one VolumeSnapshot.
type Status struct {
	// Exists is false when the VolumeSnapshot object is not there.
	Exists bool
	// EBSID is the snapshot handle, once the driver created the snapshot.
	EBSID string
	// Ready is true when the snapshot is usable.
	Ready bool
	// SizeGiB is the restore size.
	SizeGiB int
	// Error is the driver's error, empty when none.
	Error string
}

// Cluster is the Kubernetes side (adapter/k8s/snapshots.go).
type Cluster interface {
	// Prepare applies the agent's VolumeSnapshotClass, which carries its tags.
	Prepare(ctx context.Context, d agent.Desired) error
	// Request creates the VolumeSnapshot called name for the agent's claim.
	Request(ctx context.Context, d agent.Desired, name string) error
	// Status reports the VolumeSnapshot.
	Status(ctx context.Context, d agent.Desired, name string) (Status, error)
	// Delete removes the snapshot and its EBS snapshot: the content's deletion policy is flipped to Delete
	// first, then the VolumeSnapshot is removed. A missing snapshot is success.
	Delete(ctx context.Context, d agent.Desired, name string) error
}

// Manager runs the snapshot work. One pass is idempotent and safe to repeat.
type Manager struct {
	Store   Store
	Agents  agent.Source
	Volumes agent.Volumes
	Cluster Cluster
	// Name maps a row id to its VolumeSnapshot name.
	Name func(id string) string
	Now  func() time.Time
	Log  *slog.Logger
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Run is one pass: it requests the snapshots that are due, advances the open ones and prunes.
func (m *Manager) Run(ctx context.Context) error {
	var errs []error
	if err := m.requestDue(ctx); err != nil {
		errs = append(errs, err)
	}
	agents, err := m.advanceOpen(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	if err := m.prune(ctx, agents); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// requestDue inserts a scheduled row for every agent whose schedule has elapsed and whose volume exists.
// An agent with no scheduled snapshot yet is due as soon as its volume exists.
func (m *Manager) requestDue(ctx context.Context) error {
	list, err := m.Store.Scheduled(ctx)
	if err != nil {
		return fmt.Errorf("snapshot: list schedules: %w", err)
	}
	now := m.now()
	var errs []error
	for _, s := range list {
		every := Interval(s.Schedule)
		if every == 0 || (s.Last != nil && now.Sub(*s.Last) < every) {
			continue
		}
		d, err := m.Agents.LoadAgent(ctx, s.AgentID)
		if err != nil {
			errs = append(errs, fmt.Errorf("snapshot: load %s: %w", s.AgentID, err))
			continue
		}
		if d.State == agent.StateDeleting || d.State == agent.StateDeleted {
			continue
		}
		vol, err := m.Volumes.Volume(ctx, d)
		if err != nil {
			errs = append(errs, fmt.Errorf("snapshot: volume of %s: %w", s.AgentID, err))
			continue
		}
		if !vol.Exists || !vol.Bound {
			continue // nothing to snapshot yet
		}
		if _, err := m.Store.Create(ctx, d.Team.ID, d.ID, ReasonScheduled); err != nil {
			errs = append(errs, fmt.Errorf("snapshot: request for %s: %w", s.AgentID, err))
		}
	}
	return errors.Join(errs...)
}

// advanceOpen moves requested rows to pending and pending rows to completed, and returns the agents whose
// snapshots completed, for pruning.
func (m *Manager) advanceOpen(ctx context.Context) (map[string]agent.Desired, error) {
	rows, err := m.Store.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot: list open: %w", err)
	}
	touched := map[string]agent.Desired{}
	var errs []error
	for _, r := range rows {
		d, err := m.Agents.LoadAgent(ctx, r.AgentID)
		if err != nil {
			errs = append(errs, fmt.Errorf("snapshot: load %s: %w", r.AgentID, err))
			continue
		}
		done, err := m.advance(ctx, d, r)
		if err != nil {
			errs = append(errs, fmt.Errorf("snapshot %s: %w", r.ID, err))
			continue
		}
		if done {
			touched[d.ID] = d
		}
	}
	return touched, errors.Join(errs...)
}

func (m *Manager) advance(ctx context.Context, d agent.Desired, r Row) (completed bool, err error) {
	name := m.name(r.ID)
	if r.State == StateRequested {
		if err := m.Cluster.Prepare(ctx, d); err != nil {
			return false, fmt.Errorf("prepare snapshot class: %w", err)
		}
		if err := m.Cluster.Request(ctx, d, name); err != nil {
			return false, fmt.Errorf("request: %w", err)
		}
	}
	st, err := m.Cluster.Status(ctx, d, name)
	if err != nil {
		return false, err
	}
	switch {
	case st.Error != "" && !st.Ready:
		m.logf("snapshot failed", "id", r.ID, "error", st.Error)
		return false, m.Store.MarkFailed(ctx, r.ID)
	case st.Ready && st.EBSID != "":
		return true, m.Store.MarkCompleted(ctx, r.ID, st.EBSID, st.SizeGiB, m.now())
	case st.EBSID != "" && r.State == StateRequested:
		return false, m.Store.MarkPending(ctx, r.ID, st.EBSID)
	case m.now().Sub(r.RequestedAt) > StuckAfter:
		m.logf("snapshot stuck, marking failed", "id", r.ID)
		return false, m.Store.MarkFailed(ctx, r.ID)
	}
	return false, nil
}

// prune keeps the newest snapshotRetain completed snapshots of an agent (scheduled and manual) and deletes
// the rest. The final snapshot of a delete is never pruned: it is the only copy of the data.
func (m *Manager) prune(ctx context.Context, touched map[string]agent.Desired) error {
	// Agents with completed snapshots are pruned every pass, not only when one completed now, so a lowered
	// snapshotRetain takes effect.
	list, err := m.Store.Scheduled(ctx)
	if err != nil {
		return fmt.Errorf("snapshot: list for prune: %w", err)
	}
	ids := map[string]bool{}
	for id := range touched {
		ids[id] = true
	}
	for _, s := range list {
		ids[s.AgentID] = true
	}
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	var errs []error
	for _, id := range keys {
		d, err := m.Agents.LoadAgent(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("snapshot: load %s: %w", id, err))
			continue
		}
		rows, err := m.Store.Completed(ctx, id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		kept := 0
		for _, r := range rows {
			if r.Reason == ReasonPreDelete || r.Reason == ReasonPreChange {
				continue
			}
			kept++
			if kept <= d.Volume.SnapshotRetain {
				continue
			}
			if err := m.Cluster.Delete(ctx, d, m.name(r.ID)); err != nil {
				errs = append(errs, fmt.Errorf("snapshot: delete %s: %w", r.ID, err))
				continue
			}
			if err := m.Store.MarkDeleted(ctx, r.ID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) name(id string) string {
	if m.Name != nil {
		return m.Name(id)
	}
	return id
}

func (m *Manager) logf(msg string, args ...any) {
	if m.Log != nil {
		m.Log.Warn(msg, args...)
	}
}

// Latest implements agent.Snapshots on a Store: the EBS id of the newest completed snapshot.
type Latest struct{ Store Store }

var _ agent.Snapshots = Latest{}

// LatestReady implements agent.Snapshots.
func (l Latest) LatestReady(ctx context.Context, agentID string) (string, bool, error) {
	rows, err := l.Store.Completed(ctx, agentID)
	if err != nil || len(rows) == 0 {
		return "", false, err
	}
	return rows[0].EBSID, true, nil
}

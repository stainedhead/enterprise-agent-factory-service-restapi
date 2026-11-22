// Package reportschedulestest is an in-memory reportschedules.Repository.
package reportschedulestest

import (
	"context"
	"sort"
	"sync"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reportschedules"
)

// Repo implements reportschedules.Repository.
type Repo struct {
	mu sync.Mutex
	// Rows are the schedules by id.
	Rows map[string]domain.ReportSchedule
	// Teams are the team ids that exist.
	Teams map[string]bool
	// Channels maps a channel id to its team id ("" for a service-level channel).
	Channels map[string]string
	// Fail, when set, is returned by the named method ("TeamExists", "List", "Get", "ChannelOwnedByTeam",
	// "Insert", "Update", "Delete").
	Fail map[string]error
}

func (r *Repo) failed(method string) error { return r.Fail[method] }

var _ reportschedules.Repository = (*Repo)(nil)

// New returns an empty Repo.
func New() *Repo {
	return &Repo{Fail: map[string]error{}, Rows: map[string]domain.ReportSchedule{}, Teams: map[string]bool{}, Channels: map[string]string{}}
}

// TeamExists implements reportschedules.Repository.
func (r *Repo) TeamExists(_ context.Context, _ port.Tx, teamID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failed("TeamExists"); err != nil {
		return false, err
	}
	return r.Teams[teamID], nil
}

// List implements reportschedules.Repository.
func (r *Repo) List(_ context.Context, _ port.Tx, teamID, after string, limit int) ([]domain.ReportSchedule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failed("List"); err != nil {
		return nil, err
	}
	var out []domain.ReportSchedule
	for _, d := range r.Rows {
		if d.TeamID == teamID && d.ID > after {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Get implements reportschedules.Repository.
func (r *Repo) Get(_ context.Context, _ port.Tx, teamID, id string, _ bool) (domain.ReportSchedule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failed("Get"); err != nil {
		return domain.ReportSchedule{}, err
	}
	d, ok := r.Rows[id]
	if !ok || d.TeamID != teamID {
		return domain.ReportSchedule{}, domain.NotFound("report schedule")
	}
	return d, nil
}

// ChannelOwnedByTeam implements reportschedules.Repository.
func (r *Repo) ChannelOwnedByTeam(_ context.Context, _ port.Tx, teamID, channelID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failed("ChannelOwnedByTeam"); err != nil {
		return false, err
	}
	owner, ok := r.Channels[channelID]
	return ok && owner == teamID && teamID != "", nil
}

// Insert implements reportschedules.Repository.
func (r *Repo) Insert(_ context.Context, _ port.Tx, s domain.ReportSchedule) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failed("Insert"); err != nil {
		return err
	}
	for _, d := range r.Rows {
		if d.TeamID == s.TeamID && d.Name == s.Name {
			return domain.Conflict("name_in_use", "a report schedule named "+s.Name+" exists", nil)
		}
	}
	r.Rows[s.ID] = s
	return nil
}

// Update implements reportschedules.Repository.
func (r *Repo) Update(_ context.Context, _ port.Tx, s domain.ReportSchedule, expect domain.Version) (domain.ReportSchedule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failed("Update"); err != nil {
		return domain.ReportSchedule{}, err
	}
	cur, ok := r.Rows[s.ID]
	if !ok {
		return domain.ReportSchedule{}, domain.NotFound("report schedule")
	}
	if cur.Version != expect {
		return domain.ReportSchedule{}, domain.PreconditionFailed()
	}
	s.Version = cur.Version + 1
	r.Rows[s.ID] = s
	return s, nil
}

// Delete implements reportschedules.Repository.
func (r *Repo) Delete(_ context.Context, _ port.Tx, teamID, id string, expect domain.Version) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.failed("Delete"); err != nil {
		return err
	}
	cur, ok := r.Rows[id]
	if !ok || cur.TeamID != teamID {
		return domain.NotFound("report schedule")
	}
	if cur.Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.Rows, id)
	return nil
}

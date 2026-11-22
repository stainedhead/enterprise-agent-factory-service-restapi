// Package memberstest is an in-memory members.Repository.
package memberstest

import (
	"context"
	"sort"
	"sync"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/members"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repo implements members.Repository.
type Repo struct {
	mu    sync.Mutex
	Rows  map[string]domain.Member
	Users map[string]string // email -> id
}

// New returns an empty Repo.
func New() *Repo { return &Repo{Rows: map[string]domain.Member{}, Users: map[string]string{}} }

// List implements members.Repository.
func (r *Repo) List(_ context.Context, _ port.Tx, teamID, after string, limit int) ([]domain.Member, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Member
	for _, m := range r.Rows {
		if m.TeamID == teamID && m.ID > after {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Get implements members.Repository.
func (r *Repo) Get(_ context.Context, _ port.Tx, teamID, id string, _ bool) (domain.Member, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.Rows[id]
	if !ok || m.TeamID != teamID {
		return domain.Member{}, domain.NotFound("member")
	}
	return m, nil
}

// ResolveUser implements members.Repository.
func (r *Repo) ResolveUser(_ context.Context, _ port.Tx, userID, email string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if userID != "" {
		for _, id := range r.Users {
			if id == userID {
				return id, true, nil
			}
		}
		return "", false, nil
	}
	id, ok := r.Users[email]
	return id, ok, nil
}

// Insert implements members.Repository.
func (r *Repo) Insert(_ context.Context, _ port.Tx, m domain.Member) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.Rows {
		if o.TeamID == m.TeamID && o.Role == m.Role && o.UserID == m.UserID && o.GroupName == m.GroupName {
			return domain.Conflict("duplicate", "binding exists", nil)
		}
	}
	m.Version = 1
	r.Rows[m.ID] = m
	return nil
}

// UpdateRole implements members.Repository.
func (r *Repo) UpdateRole(_ context.Context, _ port.Tx, teamID, id string, role domain.TeamRole, expect domain.Version) (domain.Member, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.Rows[id]
	if !ok || m.TeamID != teamID {
		return domain.Member{}, domain.NotFound("member")
	}
	if m.Version != expect {
		return domain.Member{}, domain.PreconditionFailed()
	}
	m.Role, m.Version = role, m.Version+1
	r.Rows[id] = m
	return m, nil
}

// Delete implements members.Repository.
func (r *Repo) Delete(_ context.Context, _ port.Tx, teamID, id string, expect domain.Version) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.Rows[id]
	if !ok || m.TeamID != teamID {
		return domain.NotFound("member")
	}
	if m.Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.Rows, id)
	return nil
}

var _ members.Repository = (*Repo)(nil)

// Package secopstest is an in-memory secops.Repository.
package secopstest

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secops"
)

// Repo implements secops.Repository.
type Repo struct {
	mu sync.Mutex
	// Rows are the stored bindings by id.
	Rows map[string]secops.Operator
	// Users maps a user id or email to the user id.
	Users map[string]string
}

// New returns an empty Repo.
func New() *Repo { return &Repo{Rows: map[string]secops.Operator{}, Users: map[string]string{}} }

// List implements secops.Repository.
func (r *Repo) List(_ context.Context, _ port.Tx, after string, limit int) ([]secops.Operator, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []secops.Operator
	for _, m := range r.Rows {
		if m.ID > after {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Get implements secops.Repository.
func (r *Repo) Get(_ context.Context, _ port.Tx, id string, _ bool) (secops.Operator, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.Rows[id]
	if !ok {
		return secops.Operator{}, domain.NotFound("security operator")
	}
	return m, nil
}

// ResolveUser implements secops.Repository.
func (r *Repo) ResolveUser(_ context.Context, _ port.Tx, userID, email string) (string, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range []string{userID, email} {
		if id, ok := r.Users[k]; ok && k != "" {
			return id, true, nil
		}
	}
	return "", false, nil
}

// Insert implements secops.Repository.
func (r *Repo) Insert(_ context.Context, _ port.Tx, m secops.Operator) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.Rows {
		if o.SubjectType == m.SubjectType && o.UserID == m.UserID && o.GroupName == m.GroupName && o.IdentityProviderID == m.IdentityProviderID {
			return domain.Conflict("duplicate", "already a security operator", nil)
		}
	}
	now := time.Now().UTC()
	m.Version, m.CreatedAt, m.UpdatedAt = 1, now, now
	r.Rows[m.ID] = m
	return nil
}

// Delete implements secops.Repository.
func (r *Repo) Delete(_ context.Context, _ port.Tx, id string, expect domain.Version) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.Rows[id]
	if !ok {
		return domain.NotFound("security operator")
	}
	if m.Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.Rows, id)
	return nil
}

var _ secops.Repository = (*Repo)(nil)

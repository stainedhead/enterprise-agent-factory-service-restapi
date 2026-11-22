// Package logdesttest is an in-memory logdest.Repository.
package logdesttest

import (
	"context"
	"sort"
	"sync"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/logdest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repo implements logdest.Repository.
type Repo struct {
	mu      sync.Mutex
	Rows    map[string]domain.LogDestination
	Secrets map[string]string // secret id -> team id
}

var _ logdest.Repository = (*Repo)(nil)

// New returns an empty Repo.
func New() *Repo {
	return &Repo{Rows: map[string]domain.LogDestination{}, Secrets: map[string]string{}}
}

// List implements logdest.Repository.
func (r *Repo) List(_ context.Context, _ port.Tx, teamID, after string, limit int) ([]domain.LogDestination, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.LogDestination
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

// Get implements logdest.Repository.
func (r *Repo) Get(_ context.Context, _ port.Tx, teamID, id string, _ bool) (domain.LogDestination, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.Rows[id]
	if !ok || d.TeamID != teamID {
		return domain.LogDestination{}, domain.NotFound("log destination")
	}
	return d, nil
}

// SecretExists implements logdest.Repository.
func (r *Repo) SecretExists(_ context.Context, _ port.Tx, teamID, secretID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Secrets[secretID] == teamID && teamID != "", nil
}

// Insert implements logdest.Repository.
func (r *Repo) Insert(_ context.Context, _ port.Tx, d domain.LogDestination) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.Rows {
		if o.TeamID == d.TeamID && o.Name == d.Name {
			return domain.Conflict("duplicate", "a log destination named "+d.Name+" exists", nil)
		}
	}
	d.Version, d.Generation = 1, 1
	r.Rows[d.ID] = d
	return nil
}

// Update implements logdest.Repository.
func (r *Repo) Update(_ context.Context, _ port.Tx, d domain.LogDestination, expect domain.Version) (domain.LogDestination, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.Rows[d.ID]
	if !ok || cur.TeamID != d.TeamID {
		return domain.LogDestination{}, domain.NotFound("log destination")
	}
	if cur.Version != expect {
		return domain.LogDestination{}, domain.PreconditionFailed()
	}
	for _, o := range r.Rows {
		if o.ID != d.ID && o.TeamID == d.TeamID && o.Name == d.Name {
			return domain.LogDestination{}, domain.Conflict("duplicate", "a log destination named "+d.Name+" exists", nil)
		}
	}
	d.Version, d.Generation = cur.Version+1, domain.Generation(cur.Version+1)
	d.CreatedAt, d.CreatedBy, d.Status = cur.CreatedAt, cur.CreatedBy, cur.Status
	r.Rows[d.ID] = d
	return d, nil
}

// Delete implements logdest.Repository.
func (r *Repo) Delete(_ context.Context, _ port.Tx, teamID, id string, expect domain.Version) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.Rows[id]
	if !ok || cur.TeamID != teamID {
		return domain.NotFound("log destination")
	}
	if cur.Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.Rows, id)
	return nil
}

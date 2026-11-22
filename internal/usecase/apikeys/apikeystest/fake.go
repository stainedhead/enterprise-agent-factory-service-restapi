// Package apikeystest is an in-memory apikeys.Repository.
package apikeystest

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repo implements apikeys.Repository.
type Repo struct {
	mu     sync.Mutex
	Keys   map[string]domain.APIKey
	Hashes map[string][32]byte
}

// New returns an empty Repo.
func New() *Repo { return &Repo{Keys: map[string]domain.APIKey{}, Hashes: map[string][32]byte{}} }

// List implements apikeys.Repository.
func (r *Repo) List(_ context.Context, _ port.Tx, teamID, after string, limit int) ([]domain.APIKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.APIKey
	for _, k := range r.Keys {
		if k.TeamID == teamID && k.ID > after {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Insert implements apikeys.Repository.
func (r *Repo) Insert(_ context.Context, _ port.Tx, k domain.APIKey, sha [32]byte) (domain.APIKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.Keys {
		if o.TeamID == k.TeamID && o.Name == k.Name {
			return domain.APIKey{}, domain.Conflict("duplicate", "key name exists", nil)
		}
	}
	k.CreatedAt = time.Now().UTC()
	r.Keys[k.ID], r.Hashes[k.ID] = k, sha
	return k, nil
}

// Delete implements apikeys.Repository.
func (r *Repo) Delete(_ context.Context, _ port.Tx, teamID, id string) (domain.APIKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, ok := r.Keys[id]
	if !ok || k.TeamID != teamID {
		return domain.APIKey{}, domain.NotFound("api key")
	}
	delete(r.Keys, id)
	delete(r.Hashes, id)
	return k, nil
}

var _ apikeys.Repository = (*Repo)(nil)

// ServiceRepo implements apikeys.ServiceRepository.
type ServiceRepo struct{ *Repo }

// NewService returns an empty ServiceRepo.
func NewService() *ServiceRepo { return &ServiceRepo{New()} }

// List implements apikeys.ServiceRepository.
func (r *ServiceRepo) List(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.APIKey, error) {
	return r.Repo.List(ctx, tx, "", after, limit)
}

// Delete implements apikeys.ServiceRepository.
func (r *ServiceRepo) Delete(ctx context.Context, tx port.Tx, id string) (domain.APIKey, error) {
	return r.Repo.Delete(ctx, tx, "", id)
}

var _ apikeys.ServiceRepository = (*ServiceRepo)(nil)

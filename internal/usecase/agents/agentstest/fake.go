// Package agentstest has an in-memory agents.Repository for tests.
package agentstest

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repo implements agents.Repository.
type Repo struct {
	mu        sync.Mutex
	Rows      map[string]domain.Agent
	Templates map[string]agents.TemplateInfo
	// Models maps team id to the model ids enabled for it.
	Models map[string][]string
	// Classes are the offered storage classes.
	Classes   map[string]bool
	SecretsBy map[string]agents.SecretInfo
	Snaps     []domain.Snapshot
	// TemplateWrites counts Update calls that wrote the template columns.
	TemplateWrites int
}

// NewRepo returns a Repo with the gp3 class offered.
func NewRepo() *Repo {
	return &Repo{Rows: map[string]domain.Agent{}, Templates: map[string]agents.TemplateInfo{}, Models: map[string][]string{},
		Classes: map[string]bool{"gp3": true}, SecretsBy: map[string]agents.SecretInfo{}}
}

// List implements agents.Repository.
func (r *Repo) List(_ context.Context, _ port.Tx, teamID, after string, limit int) ([]domain.Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Agent
	for _, a := range r.Rows {
		if a.TeamID == teamID && a.ID > after && a.State != domain.AgentDeleted {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Get implements agents.Repository.
func (r *Repo) Get(_ context.Context, _ port.Tx, teamID, id string, _ bool) (domain.Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.Rows[id]
	if !ok || a.TeamID != teamID || a.State == domain.AgentDeleted {
		return domain.Agent{}, domain.NotFound("agent")
	}
	return a, nil
}

// Insert implements agents.Repository.
func (r *Repo) Insert(_ context.Context, _ port.Tx, a domain.Agent, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.Rows {
		if x.TeamID == a.TeamID && x.Name == a.Name && x.State != domain.AgentDeleted {
			return domain.Conflict("duplicate", "an agent with that name exists", nil)
		}
	}
	a.CreatedAt, a.UpdatedAt = time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC()
	r.Rows[a.ID] = a
	return nil
}

// Update implements agents.Repository.
func (r *Repo) Update(_ context.Context, _ port.Tx, a domain.Agent, _ string, templateChanged bool, expect domain.Version) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := r.Rows[a.ID]
	if cur.Version != expect {
		return domain.PreconditionFailed()
	}
	if templateChanged {
		r.TemplateWrites++
	} else {
		a.Template, a.Image = cur.Template, cur.Image
	}
	a.Version = cur.Version + 1
	a.CreatedAt, a.Status = cur.CreatedAt, cur.Status
	r.Rows[a.ID] = a
	return nil
}

// Template implements agents.Repository.
func (r *Repo) Template(_ context.Context, _ port.Tx, name string) (agents.TemplateInfo, bool, error) {
	t, ok := r.Templates[name]
	return t, ok, nil
}

// ModelEnabled implements agents.Repository.
func (r *Repo) ModelEnabled(_ context.Context, _ port.Tx, teamID, modelID string) (bool, error) {
	for _, m := range r.Models[teamID] {
		if m == modelID {
			return true, nil
		}
	}
	return false, nil
}

// StorageClassEnabled implements agents.Repository.
func (r *Repo) StorageClassEnabled(_ context.Context, _ port.Tx, name string) (bool, error) {
	return r.Classes[name], nil
}

// Secrets implements agents.Repository.
func (r *Repo) Secrets(_ context.Context, _ port.Tx, _ string, ids []string) (map[string]agents.SecretInfo, error) {
	out := map[string]agents.SecretInfo{}
	for _, id := range ids {
		if s, ok := r.SecretsBy[id]; ok {
			out[id] = s
		}
	}
	return out, nil
}

// Snapshots implements agents.Repository.
func (r *Repo) Snapshots(_ context.Context, _ port.Tx, _, agentID, after string, limit int) ([]domain.Snapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Snapshot
	for _, s := range r.Snaps {
		if s.AgentID == agentID && (after == "" || s.ID < after) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// InsertSnapshot implements agents.Repository.
func (r *Repo) InsertSnapshot(_ context.Context, _ port.Tx, _ string, s domain.Snapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Snaps = append(r.Snaps, s)
	return nil
}

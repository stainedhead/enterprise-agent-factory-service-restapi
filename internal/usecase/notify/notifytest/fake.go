// Package notifytest is an in-memory notify.Repository.
package notifytest

import (
	"context"
	"sort"
	"sync"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/notify"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repo implements notify.Repository.
type Repo struct {
	mu      sync.Mutex
	Rows    map[string]domain.NotificationChannel
	Secrets map[string]string // secret id -> team id
	// WorkOrderChannel is the value of setting work_orders.notification_channel_id.
	WorkOrderChannel string
}

var _ notify.Repository = (*Repo)(nil)

// New returns an empty Repo.
func New() *Repo {
	return &Repo{Rows: map[string]domain.NotificationChannel{}, Secrets: map[string]string{}}
}

// List implements notify.Repository.
func (r *Repo) List(_ context.Context, _ port.Tx, teamID, after string, limit int) ([]domain.NotificationChannel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.NotificationChannel
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

// Get implements notify.Repository.
func (r *Repo) Get(_ context.Context, _ port.Tx, teamID, id string, _ bool) (domain.NotificationChannel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.Rows[id]
	if !ok || d.TeamID != teamID {
		return domain.NotificationChannel{}, domain.NotFound("notification channel")
	}
	return d, nil
}

// SecretExists implements notify.Repository.
func (r *Repo) SecretExists(_ context.Context, _ port.Tx, teamID, secretID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Secrets[secretID] == teamID && teamID != "", nil
}

// Insert implements notify.Repository.
func (r *Repo) Insert(_ context.Context, _ port.Tx, c domain.NotificationChannel) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.Rows {
		if o.TeamID == c.TeamID && o.Name == c.Name {
			return domain.Conflict("name_taken", "a channel with this name exists", nil)
		}
	}
	r.Rows[c.ID] = c
	return nil
}

// Update implements notify.Repository.
func (r *Repo) Update(_ context.Context, _ port.Tx, c domain.NotificationChannel, expect domain.Version) (domain.NotificationChannel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.Rows[c.ID]
	if !ok {
		return domain.NotificationChannel{}, domain.NotFound("notification channel")
	}
	if cur.Version != expect {
		return domain.NotificationChannel{}, domain.PreconditionFailed()
	}
	c.Version, c.Generation = cur.Version+1, cur.Generation+1
	r.Rows[c.ID] = c
	return c, nil
}

// Delete implements notify.Repository.
func (r *Repo) Delete(_ context.Context, _ port.Tx, teamID, id string, expect domain.Version) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.Rows[id]
	if !ok || cur.TeamID != teamID {
		return domain.NotFound("notification channel")
	}
	if cur.Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.Rows, id)
	return nil
}

// NamedByWorkOrderSetting implements notify.Repository.
func (r *Repo) NamedByWorkOrderSetting(_ context.Context, _ port.Tx, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.WorkOrderChannel == id, nil
}

// SetStatus lets a test play the reconciler: it replaces a channel's status conditions.
func (r *Repo) SetStatus(id string, conds []domain.Condition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.Rows[id]
	c.Status.Conditions = conds
	r.Rows[id] = c
}

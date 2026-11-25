// Package secretstest has in-memory implementations of secrets.Repository and secrets.Writer.
package secretstest

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
)

// Repo implements secrets.Repository.
type Repo struct {
	mu   sync.Mutex
	Rows map[string]domain.Secret
	// Namespace is the team namespace used to build paths.
	Namespace string
	// Agents are the agent ids (to names) that exist.
	Agents map[string]string
	// InsertErr, UpdateErr and VersionErr fail the matching call.
	InsertErr, UpdateErr, VersionErr error
}

// NewRepo returns an empty Repo.
func NewRepo() *Repo {
	return &Repo{Rows: map[string]domain.Secret{}, Namespace: "ns", Agents: map[string]string{}}
}

func rollback(tx port.Tx, fn func()) {
	if t, ok := tx.(*fakes.Tx); ok {
		t.OnRollback(fn)
	}
}

// List implements secrets.Repository.
func (r *Repo) List(_ context.Context, _ port.Tx, teamID, after string, managedBy domain.SecretManagedBy, limit int) ([]domain.Secret, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Secret
	for _, s := range r.Rows {
		if s.TeamID == teamID && s.ID > after && (managedBy == "" || s.ManagedBy == managedBy || (managedBy == domain.ManagedByUser && s.ManagedBy == "")) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Get implements secrets.Repository.
func (r *Repo) Get(_ context.Context, _ port.Tx, teamID, id string, _ bool) (domain.Secret, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.Rows[id]
	if !ok || s.TeamID != teamID {
		return domain.Secret{}, domain.NotFound("secret")
	}
	return s, nil
}

// NameTaken implements secrets.Repository.
func (r *Repo) NameTaken(_ context.Context, _ port.Tx, teamID, name string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.Rows {
		if s.TeamID == teamID && s.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// Path implements secrets.Repository.
func (r *Repo) Path(_ context.Context, _ port.Tx, _, agentID, poolID, name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	owner := "_shared"
	switch {
	case agentID != "":
		n, ok := r.Agents[agentID]
		if !ok {
			return "", domain.Validation(domain.FieldError{Field: "agentId", Reason: "no such agent in the team"})
		}
		owner = n
	case poolID != "":
		owner = "pool-" + poolID[len(poolID)-4:]
	}
	return "afs/" + r.Namespace + "/" + owner + "/" + name, nil
}

// Insert implements secrets.Repository.
func (r *Repo) Insert(_ context.Context, tx port.Tx, s domain.Secret) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.InsertErr != nil {
		return r.InsertErr
	}
	s.Version, s.CreatedAt, s.UpdatedAt = 1, time.Unix(0, 0).UTC(), time.Unix(0, 0).UTC()
	r.Rows[s.ID] = s
	rollback(tx, func() { r.mu.Lock(); delete(r.Rows, s.ID); r.mu.Unlock() })
	return nil
}

// Update implements secrets.Repository.
func (r *Repo) Update(_ context.Context, _ port.Tx, s domain.Secret, expect domain.Version) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.UpdateErr != nil {
		return r.UpdateErr
	}
	cur := r.Rows[s.ID]
	if cur.Version != expect {
		return domain.PreconditionFailed()
	}
	s.Version = cur.Version + 1
	r.Rows[s.ID] = s
	return nil
}

// SetCurrentVersion implements secrets.Repository.
func (r *Repo) SetCurrentVersion(_ context.Context, _ port.Tx, teamID, id, versionID string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.VersionErr != nil {
		return r.VersionErr
	}
	s := r.Rows[id]
	s.VersionID, s.LastChangedAt, s.Version = versionID, &at, s.Version+1
	r.Rows[id] = s
	return nil
}

// Delete implements secrets.Repository.
func (r *Repo) Delete(_ context.Context, _ port.Tx, _, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.Rows, id)
	return nil
}

var _ secrets.Repository = (*Repo)(nil)

// Writer implements secrets.Writer and remembers every call. Values are kept so tests can search
// outputs for them; they are the test's own fixtures.
type Writer struct {
	mu sync.Mutex
	// ByName maps a Secrets Manager name to its ARN (a repeat of the same token returns it).
	ByName map[string]secrets.Created
	// Tokens maps a token to the name it created.
	Tokens map[string]string
	// Calls lists operations in order: "create", "put", "force-delete", "tag-orphan", "schedule".
	Calls []string
	// Tags holds the tags of the last Create per name.
	Tags map[string]map[string]string
	// Scheduled lists ARNs scheduled for deletion.
	Scheduled []string
	// Orphaned lists ARNs tagged afs-orphan.
	Orphaned []string
	// Written lists every value passed to Create and PutValue.
	Written []string
	// CreateErr, PutErr, ForceDeleteErr, TagErr and ScheduleErr fail the matching call.
	CreateErr, PutErr, ForceDeleteErr, TagErr, ScheduleErr error
	n                                                      int
}

// NewWriter returns an empty Writer.
func NewWriter() *Writer {
	return &Writer{ByName: map[string]secrets.Created{}, Tokens: map[string]string{}, Tags: map[string]map[string]string{}}
}

// Create implements secrets.Writer.
func (w *Writer) Create(_ context.Context, name string, v domain.SecretValue, token string, tags map[string]string) (secrets.Created, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Calls = append(w.Calls, "create")
	if w.CreateErr != nil {
		return secrets.Created{}, w.CreateErr
	}
	if c, ok := w.ByName[name]; ok {
		if w.Tokens[token] == name {
			return c, nil
		}
		return secrets.Created{}, secrets.ErrNameTaken
	}
	w.n++
	c := secrets.Created{ARN: fmt.Sprintf("arn:aws:secretsmanager:us-east-1:111122223333:secret:%s-AbC%03d", name, w.n%1000), VersionID: fmt.Sprintf("v%d", w.n)}
	w.ByName[name], w.Tokens[token], w.Tags[name] = c, name, tags
	w.Written = append(w.Written, v.Expose())
	return c, nil
}

// PutValue implements secrets.Writer.
func (w *Writer) PutValue(_ context.Context, _ string, v domain.SecretValue, _ string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Calls = append(w.Calls, "put")
	if w.PutErr != nil {
		return "", w.PutErr
	}
	w.n++
	w.Written = append(w.Written, v.Expose())
	return fmt.Sprintf("v%d", w.n), nil
}

// ForceDelete implements secrets.Writer.
func (w *Writer) ForceDelete(_ context.Context, arn string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Calls = append(w.Calls, "force-delete")
	if w.ForceDeleteErr != nil {
		return w.ForceDeleteErr
	}
	for n, c := range w.ByName {
		if c.ARN == arn {
			delete(w.ByName, n)
		}
	}
	return nil
}

// TagOrphan implements secrets.Writer.
func (w *Writer) TagOrphan(_ context.Context, arn string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Calls = append(w.Calls, "tag-orphan")
	if w.TagErr != nil {
		return w.TagErr
	}
	w.Orphaned = append(w.Orphaned, arn)
	return nil
}

// ScheduleDeletion implements secrets.Writer.
func (w *Writer) ScheduleDeletion(_ context.Context, arn string, _ int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Calls = append(w.Calls, "schedule")
	if w.ScheduleErr != nil {
		return w.ScheduleErr
	}
	w.Scheduled = append(w.Scheduled, arn)
	return nil
}

var _ secrets.Writer = (*Writer)(nil)

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
	// Collab is the registration mode of teams that have a collaboration agent (by team id).
	Collab map[string]domain.RegistrationMode
	// Tools is the collaboration tool of a team (default paperclip).
	Tools map[string]domain.CollabTool
	// Links records SetCollaboration calls by agent id.
	Links map[string]domain.AgentCollaboration
	// Boot records the boot secrets stored by InsertBootSecret (secret id to env name).
	Boot map[string]domain.Secret
	// BootEnv maps a stored boot secret id to the environment variable it was bound as.
	BootEnv map[string]string
	// TemplateWrites counts Update calls that wrote the template columns.
	TemplateWrites int
	// Err makes the named method fail: List, Get, Insert, Update, Template, ModelEnabled,
	// StorageClassEnabled, Secrets, Snapshots or InsertSnapshot.
	Err map[string]error
}

func (r *Repo) fail(m string) error { return r.Err[m] }

// NewRepo returns a Repo with the gp3 class offered.
func NewRepo() *Repo {
	return &Repo{Rows: map[string]domain.Agent{}, Templates: map[string]agents.TemplateInfo{}, Models: map[string][]string{},
		Classes: map[string]bool{"gp3": true}, SecretsBy: map[string]agents.SecretInfo{},
		Collab: map[string]domain.RegistrationMode{}, Links: map[string]domain.AgentCollaboration{}, Boot: map[string]domain.Secret{}, BootEnv: map[string]string{}}
}

// List implements agents.Repository.
func (r *Repo) List(_ context.Context, _ port.Tx, teamID string, role domain.AgentRole, after string, limit int) ([]domain.Agent, error) {
	if err := r.fail("List"); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Agent
	for _, a := range r.Rows {
		if a.TeamID == teamID && a.ID > after && a.State != domain.AgentDeleted && (role == "" || a.IsOrchestration() == (role.Normalize() == domain.AgentRoleOrchestration)) {
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
	if err := r.fail("Get"); err != nil {
		return domain.Agent{}, err
	}
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
	if err := r.fail("Insert"); err != nil {
		return err
	}
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
	if err := r.fail("Update"); err != nil {
		return err
	}
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
	if err := r.fail("Template"); err != nil {
		return agents.TemplateInfo{}, false, err
	}
	t, ok := r.Templates[name]
	return t, ok, nil
}

// ModelEnabled implements agents.Repository.
func (r *Repo) ModelEnabled(_ context.Context, _ port.Tx, teamID, modelID string) (bool, error) {
	if err := r.fail("ModelEnabled"); err != nil {
		return false, err
	}
	for _, m := range r.Models[teamID] {
		if m == modelID {
			return true, nil
		}
	}
	return false, nil
}

// StorageClassEnabled implements agents.Repository.
func (r *Repo) StorageClassEnabled(_ context.Context, _ port.Tx, name string) (bool, error) {
	if err := r.fail("StorageClassEnabled"); err != nil {
		return false, err
	}
	return r.Classes[name], nil
}

// Secrets implements agents.Repository.
func (r *Repo) Secrets(_ context.Context, _ port.Tx, _ string, ids []string) (map[string]agents.SecretInfo, error) {
	if err := r.fail("Secrets"); err != nil {
		return nil, err
	}
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
	if err := r.fail("Snapshots"); err != nil {
		return nil, err
	}
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
	if err := r.fail("InsertSnapshot"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Snaps = append(r.Snaps, s)
	return nil
}

// InsertCollaboration implements agents.Repository.
func (r *Repo) InsertCollaboration(_ context.Context, _ port.Tx, teamID, _ string) error {
	if err := r.fail("InsertCollaboration"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Collab[teamID] = domain.RegistrationManual
	return nil
}

// CollaborationMode implements agents.Repository.
func (r *Repo) CollaborationMode(_ context.Context, _ port.Tx, teamID string) (domain.CollabTool, domain.RegistrationMode, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.Collab[teamID]
	tool := r.Tools[teamID]
	if tool == "" {
		tool = domain.ToolPaperclip
	}
	return tool, m, ok, nil
}

// ModelSets implements agents.Repository: the team's Models are the service, team and callable sets.
func (r *Repo) ModelSets(_ context.Context, _ port.Tx, teamID string) (domain.ModelSets, error) {
	if err := r.fail("ModelSets"); err != nil {
		return domain.ModelSets{}, err
	}
	ids := r.Models[teamID]
	return domain.ModelSets{Service: ids, Team: ids, Callable: ids}, nil
}

// EffectiveModels implements agents.Repository: the agent's own set within the team's.
func (r *Repo) EffectiveModels(_ context.Context, _ port.Tx, teamID string, list []domain.Agent) error {
	for i := range list {
		list[i].Models.Effective = append([]string(nil), r.Models[teamID]...)
	}
	return nil
}

// SetCollaboration implements agents.Repository.
func (r *Repo) SetCollaboration(_ context.Context, _ port.Tx, _, agentID string, connect bool, pc *string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := domain.AgentCollaboration{Connect: connect, LinkState: domain.LinkDisabled}
	if connect {
		l.LinkState = domain.LinkPending
	}
	if pc != nil {
		l.PaperclipAgentID = *pc
	}
	r.Links[agentID] = l
	return nil
}

// SecretPath implements agents.Repository.
func (r *Repo) SecretPath(_ context.Context, _ port.Tx, _, agentID, name string) (string, error) {
	return "afs/ns/" + agentID + "/" + name, nil
}

// InsertBootSecret implements agents.Repository.
func (r *Repo) InsertBootSecret(_ context.Context, _ port.Tx, s domain.Secret, env string) error {
	if err := r.fail("InsertBootSecret"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Boot[s.ID] = s
	r.BootEnv[s.ID] = env
	return nil
}

// ManagedBindings implements agents.Repository.
func (r *Repo) ManagedBindings(_ context.Context, _ port.Tx, _, agentID string) ([]domain.SecretBinding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.SecretBinding
	for id, s := range r.Boot {
		if s.AgentID == agentID {
			out = append(out, domain.SecretBinding{SecretID: id, Delivery: "env", Target: r.BootEnv[id]})
		}
	}
	return out, nil
}

// BootWriter implements agents.BootWriter in memory.
type BootWriter struct {
	mu        sync.Mutex
	Created   []string
	Deleted   []string
	Orphaned  []string
	CreateErr error
	// FailAt makes the Nth Create (1-based) fail with CreateErr; 0 fails every call when CreateErr is set.
	FailAt    int
	DeleteErr error
	n         int
}

// Create implements agents.BootWriter.
func (w *BootWriter) Create(_ context.Context, name string, _ domain.SecretValue, _ string, _ map[string]string) (agents.BootCreated, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n++
	if w.CreateErr != nil && (w.FailAt == 0 || w.FailAt == w.n) {
		return agents.BootCreated{}, w.CreateErr
	}
	arn := "arn:aws:secretsmanager:us-east-1:111122223333:secret:" + name + "-AbCdEf"
	w.Created = append(w.Created, arn)
	return agents.BootCreated{ARN: arn, VersionID: "v1"}, nil
}

// ForceDelete implements agents.BootWriter.
func (w *BootWriter) ForceDelete(_ context.Context, arn string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.DeleteErr != nil {
		return w.DeleteErr
	}
	w.Deleted = append(w.Deleted, arn)
	return nil
}

// TagOrphan implements agents.BootWriter.
func (w *BootWriter) TagOrphan(_ context.Context, arn string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.Orphaned = append(w.Orphaned, arn)
	return nil
}

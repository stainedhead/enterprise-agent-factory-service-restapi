// Package identitytest is an in-memory identity.Repository.
package identitytest

import (
	"context"
	"sort"
	"sync"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/identity"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repo implements identity.Repository.
type Repo struct {
	mu         sync.Mutex
	Agents     map[string]identity.AgentInfo // agent id
	AgentTeam  map[string]string             // agent id -> team id
	Allowed    map[string]bool               // team|account
	Secrets    map[string]string             // secret id -> team id
	Links      map[string]domain.RoleLink
	Identities map[string]domain.AgentIdentity
	// Fail makes the named method return the error (tests of the failure paths).
	Fail map[string]error
}

// fail returns the error injected for method, if any.
func (r *Repo) fail(method string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Fail[method]
}

var _ identity.Repository = (*Repo)(nil)

// New returns an empty Repo.
func New() *Repo {
	return &Repo{Agents: map[string]identity.AgentInfo{}, AgentTeam: map[string]string{}, Allowed: map[string]bool{},
		Secrets: map[string]string{}, Links: map[string]domain.RoleLink{}, Identities: map[string]domain.AgentIdentity{}}
}

// Agent implements identity.Repository.
func (r *Repo) Agent(_ context.Context, _ port.Tx, teamID, agentID string) (identity.AgentInfo, error) {
	if err := r.fail("Agent"); err != nil {
		var zero identity.AgentInfo
		return zero, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.Agents[agentID]
	if !ok || r.AgentTeam[agentID] != teamID {
		return a, domain.NotFound("agent")
	}
	return a, nil
}

// AccountAllowed implements identity.Repository.
func (r *Repo) AccountAllowed(_ context.Context, _ port.Tx, teamID, account string) (bool, error) {
	if err := r.fail("AccountAllowed"); err != nil {
		var zero bool
		return zero, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Allowed[teamID+"|"+account], nil
}

// SecretInTeam implements identity.Repository.
func (r *Repo) SecretInTeam(_ context.Context, _ port.Tx, teamID, secretID string) (bool, error) {
	if err := r.fail("SecretInTeam"); err != nil {
		var zero bool
		return zero, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Secrets[secretID] == teamID, nil
}

// ListLinks implements identity.Repository.
func (r *Repo) ListLinks(_ context.Context, _ port.Tx, teamID, agentID, after string, limit int) ([]domain.RoleLink, error) {
	if err := r.fail("ListLinks"); err != nil {
		var zero []domain.RoleLink
		return zero, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.RoleLink
	for _, l := range r.Links {
		if l.TeamID == teamID && l.AgentID == agentID && l.ID > after {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetLink implements identity.Repository.
func (r *Repo) GetLink(_ context.Context, _ port.Tx, teamID, agentID, id string, _ bool) (domain.RoleLink, error) {
	if err := r.fail("GetLink"); err != nil {
		var zero domain.RoleLink
		return zero, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.Links[id]
	if !ok || l.TeamID != teamID || l.AgentID != agentID {
		return l, domain.NotFound("role link")
	}
	return l, nil
}

// InsertLink implements identity.Repository.
func (r *Repo) InsertLink(_ context.Context, _ port.Tx, l domain.RoleLink) error {
	if err := r.fail("InsertLink"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.Links {
		if o.AgentID == l.AgentID && (o.RoleARN == l.RoleARN || o.ProfileName == l.ProfileName) {
			return domain.Conflict("duplicate", "the agent already has a link with this role or profile name", nil)
		}
	}
	r.Links[l.ID] = l
	return nil
}

// DeleteLink implements identity.Repository.
func (r *Repo) DeleteLink(_ context.Context, _ port.Tx, _, _, id string, expect domain.Version) error {
	if err := r.fail("DeleteLink"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Links[id].Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.Links, id)
	return nil
}

// RequestVerify implements identity.Repository.
func (r *Repo) RequestVerify(_ context.Context, _ port.Tx, _, _, id string) (domain.RoleLink, error) {
	if err := r.fail("RequestVerify"); err != nil {
		var zero domain.RoleLink
		return zero, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.Links[id]
	l.Status, l.LastError, l.Version = domain.RoleLinkStatusPendingTrust, "", l.Version+1
	r.Links[id] = l
	return l, nil
}

// ListIdentities implements identity.Repository.
func (r *Repo) ListIdentities(_ context.Context, _ port.Tx, teamID, agentID, after string, limit int) ([]domain.AgentIdentity, error) {
	if err := r.fail("ListIdentities"); err != nil {
		var zero []domain.AgentIdentity
		return zero, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.AgentIdentity
	for _, i := range r.Identities {
		if i.TeamID == teamID && i.AgentID == agentID && i.ID > after {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetIdentity implements identity.Repository.
func (r *Repo) GetIdentity(_ context.Context, _ port.Tx, teamID, agentID, id string, _ bool) (domain.AgentIdentity, error) {
	if err := r.fail("GetIdentity"); err != nil {
		var zero domain.AgentIdentity
		return zero, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	i, ok := r.Identities[id]
	if !ok || i.TeamID != teamID || i.AgentID != agentID {
		return i, domain.NotFound("identity")
	}
	return i, nil
}

// InsertIdentity implements identity.Repository.
func (r *Repo) InsertIdentity(_ context.Context, _ port.Tx, i domain.AgentIdentity) error {
	if err := r.fail("InsertIdentity"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, o := range r.Identities {
		if o.AgentID == i.AgentID && o.Kind == i.Kind {
			return domain.Conflict("duplicate", "the agent already has an identity of this kind", nil)
		}
	}
	r.Identities[i.ID] = i
	return nil
}

// DeleteIdentity implements identity.Repository.
func (r *Repo) DeleteIdentity(_ context.Context, _ port.Tx, _, _, id string, expect domain.Version) error {
	if err := r.fail("DeleteIdentity"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Identities[id].Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.Identities, id)
	return nil
}

// Package poolstest is an in-memory pools.Repository for use case tests. Writes are undone when the
// fake transaction rolls back.
package poolstest

import (
	"context"
	"maps"
	"slices"
	"sort"
	"sync"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pools"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

// Repo implements pools.Repository.
type Repo struct {
	mu   sync.Mutex
	seen map[*fakes.Tx]bool
	// Kinds maps a team id to its kind; a team missing from it is invisible (NotFound).
	Kinds     map[string]domain.TeamKind
	Pools     map[string]domain.Pool
	Stages    map[string]domain.Stage
	Handoff   map[string][]string
	Binds     map[string][]domain.SecretBinding
	Configs   map[string]domain.PoolConfigValue // key pool/name
	Namespace map[string]bool                   // key team/name
	Optouts   map[string]bool                   // key pool/system
	Systems   map[string]domain.ExternalSystemAccess
	Links     map[string]domain.PoolRoleLink
	Allowed   map[string]bool
	Templates map[string]pools.TemplateInfo
	Models    map[string]bool
	Agents    map[string]bool // key team/name
	Secrets_  map[string]pools.SecretInfo
	Counts    domain.PoolWorkers
	// Fail, when set, makes the named method fail with the error.
	Fail map[string]error
	// StageUse lists stage ids that pools still name (DeleteStage refuses them).
	StageUse map[string]bool
}

// New returns an empty Repo.
func New() *Repo {
	return &Repo{Kinds: map[string]domain.TeamKind{}, Pools: map[string]domain.Pool{}, Stages: map[string]domain.Stage{},
		Handoff: map[string][]string{}, Binds: map[string][]domain.SecretBinding{}, Configs: map[string]domain.PoolConfigValue{},
		Namespace: map[string]bool{}, Optouts: map[string]bool{}, Systems: map[string]domain.ExternalSystemAccess{},
		Links: map[string]domain.PoolRoleLink{}, Allowed: map[string]bool{}, Templates: map[string]pools.TemplateInfo{},
		Models: map[string]bool{}, Agents: map[string]bool{}, Secrets_: map[string]pools.SecretInfo{}, Fail: map[string]error{},
		StageUse: map[string]bool{}}
}

func (r *Repo) guard(tx port.Tx) {
	ft, ok := tx.(*fakes.Tx)
	if !ok || r.seen[ft] {
		return
	}
	if r.seen == nil {
		r.seen = map[*fakes.Tx]bool{}
	}
	r.seen[ft] = true
	p, s, h, b, c, o, l := maps.Clone(r.Pools), maps.Clone(r.Stages), maps.Clone(r.Handoff), maps.Clone(r.Binds),
		maps.Clone(r.Configs), maps.Clone(r.Optouts), maps.Clone(r.Links)
	ft.OnRollback(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.Pools, r.Stages, r.Handoff, r.Binds, r.Configs, r.Optouts, r.Links = p, s, h, b, c, o, l
	})
}

func (r *Repo) fail(m string) error { return r.Fail[m] }

// TeamKind implements pools.Repository.
func (r *Repo) TeamKind(_ context.Context, _ port.Tx, teamID string) (domain.TeamKind, error) {
	if err := r.fail("TeamKind"); err != nil {
		return "", err
	}
	k, ok := r.Kinds[teamID]
	if !ok {
		return "", domain.NotFound("team")
	}
	return k, nil
}

// List implements pools.Repository.
func (r *Repo) List(_ context.Context, _ port.Tx, teamID, after string, limit int) ([]domain.Pool, error) {
	if err := r.fail("List"); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Pool
	for _, p := range r.Pools {
		if p.TeamID == teamID && p.State != domain.PoolStateDeleted && p.ID > after {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Get implements pools.Repository.
func (r *Repo) Get(_ context.Context, _ port.Tx, teamID, id string, _ bool) (domain.Pool, error) {
	if err := r.fail("Get"); err != nil {
		return domain.Pool{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.Pools[id]
	if !ok || p.TeamID != teamID || p.State == domain.PoolStateDeleted {
		return domain.Pool{}, domain.NotFound("pool")
	}
	return p, nil
}

// Insert implements pools.Repository.
func (r *Repo) Insert(_ context.Context, tx port.Tx, p domain.Pool, _ string) error {
	if err := r.fail("Insert"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	for _, o := range r.Pools {
		if o.TeamID == p.TeamID && o.Name == p.Name && o.State != domain.PoolStateDeleted {
			return domain.Conflict("duplicate", "pool name exists", nil)
		}
	}
	r.Pools[p.ID] = p
	return nil
}

// Update implements pools.Repository.
func (r *Repo) Update(_ context.Context, tx port.Tx, p domain.Pool, _ string, _ bool, expect domain.Version) error {
	if err := r.fail("Update"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	cur := r.Pools[p.ID]
	if cur.Version != expect {
		return domain.PreconditionFailed()
	}
	p.Version = cur.Version + 1
	r.Pools[p.ID] = p
	return nil
}

// Touch implements pools.Repository.
func (r *Repo) Touch(_ context.Context, tx port.Tx, _ string, ids []string) error {
	if err := r.fail("Touch"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	for _, id := range ids {
		p := r.Pools[id]
		p.Generation++
		p.Version++
		r.Pools[id] = p
	}
	return nil
}

// Template implements pools.Repository.
func (r *Repo) Template(_ context.Context, _ port.Tx, name string) (pools.TemplateInfo, bool, error) {
	if err := r.fail("Template"); err != nil {
		return pools.TemplateInfo{}, false, err
	}
	t, ok := r.Templates[name]
	return t, ok, nil
}

// ModelEnabled implements pools.Repository.
func (r *Repo) ModelEnabled(_ context.Context, _ port.Tx, _, model string) (bool, error) {
	return r.Models[model], r.fail("ModelEnabled")
}

// AgentNameTaken implements pools.Repository.
func (r *Repo) AgentNameTaken(_ context.Context, _ port.Tx, teamID, name string) (bool, error) {
	return r.Agents[teamID+"/"+name], r.fail("AgentNameTaken")
}

// Workers implements pools.Repository.
func (r *Repo) Workers(context.Context, port.Tx, string, string) (domain.PoolWorkers, error) {
	return r.Counts, r.fail("Workers")
}

// ListStages implements pools.Repository.
func (r *Repo) ListStages(_ context.Context, _ port.Tx, teamID, after string, limit int) ([]domain.Stage, error) {
	if err := r.fail("ListStages"); err != nil {
		return nil, err
	}
	var out []domain.Stage
	for _, s := range r.Stages {
		if s.TeamID == teamID && s.ID > after {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetStage implements pools.Repository.
func (r *Repo) GetStage(_ context.Context, _ port.Tx, teamID, id string, _ bool) (domain.Stage, error) {
	if err := r.fail("GetStage"); err != nil {
		return domain.Stage{}, err
	}
	s, ok := r.Stages[id]
	if !ok || s.TeamID != teamID {
		return domain.Stage{}, domain.NotFound("stage")
	}
	return s, nil
}

// InsertStage implements pools.Repository.
func (r *Repo) InsertStage(_ context.Context, tx port.Tx, s domain.Stage) error {
	if err := r.fail("InsertStage"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	for _, o := range r.Stages {
		if o.TeamID == s.TeamID && (o.Name == s.Name || o.Position == s.Position) {
			return domain.Conflict("duplicate", "stage name or position exists", nil)
		}
	}
	r.Stages[s.ID] = s
	return nil
}

// UpdateStage implements pools.Repository.
func (r *Repo) UpdateStage(_ context.Context, tx port.Tx, s domain.Stage, expect domain.Version) error {
	if err := r.fail("UpdateStage"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	if r.Stages[s.ID].Version != expect {
		return domain.PreconditionFailed()
	}
	s.Version = expect + 1
	r.Stages[s.ID] = s
	return nil
}

// DeleteStage implements pools.Repository.
func (r *Repo) DeleteStage(_ context.Context, tx port.Tx, _, id string, expect domain.Version) error {
	if err := r.fail("DeleteStage"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	if r.StageUse[id] {
		return domain.Conflict("dependents_exist", "pools use the stage", nil)
	}
	if r.Stages[id].Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.Stages, id)
	return nil
}

// Handoffs implements pools.Repository.
func (r *Repo) Handoffs(_ context.Context, _ port.Tx, _, poolID string) ([]string, error) {
	if err := r.fail("Handoffs"); err != nil {
		return nil, err
	}
	return append([]string{}, r.Handoff[poolID]...), nil
}

// PoolsExist implements pools.Repository.
func (r *Repo) PoolsExist(_ context.Context, _ port.Tx, teamID string, ids []string) (int, error) {
	if err := r.fail("PoolsExist"); err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if p, ok := r.Pools[id]; ok && p.TeamID == teamID && p.State != domain.PoolStateDeleted {
			n++
		}
	}
	return n, nil
}

// ReplaceHandoffs implements pools.Repository.
func (r *Repo) ReplaceHandoffs(_ context.Context, tx port.Tx, _, poolID string, to []string) error {
	if err := r.fail("ReplaceHandoffs"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	r.Handoff[poolID] = slices.Sorted(slices.Values(to))
	return nil
}

// Bindings implements pools.Repository.
func (r *Repo) Bindings(_ context.Context, _ port.Tx, _, poolID string) ([]domain.SecretBinding, error) {
	if err := r.fail("Bindings"); err != nil {
		return nil, err
	}
	return append([]domain.SecretBinding{}, r.Binds[poolID]...), nil
}

// Secrets implements pools.Repository.
func (r *Repo) Secrets(_ context.Context, _ port.Tx, _ string, ids []string) (map[string]pools.SecretInfo, error) {
	if err := r.fail("Secrets"); err != nil {
		return nil, err
	}
	out := map[string]pools.SecretInfo{}
	for _, id := range ids {
		if s, ok := r.Secrets_[id]; ok {
			out[id] = s
		}
	}
	return out, nil
}

// ReplaceBindings implements pools.Repository.
func (r *Repo) ReplaceBindings(_ context.Context, tx port.Tx, _, poolID string, bs []domain.SecretBinding) error {
	if err := r.fail("ReplaceBindings"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	r.Binds[poolID] = append([]domain.SecretBinding{}, bs...)
	return nil
}

func ck(poolID string, name domain.ConfigName) string { return poolID + "/" + string(name) }

// ListConfig implements pools.Repository.
func (r *Repo) ListConfig(_ context.Context, _ port.Tx, _, poolID, after string, limit int) ([]domain.PoolConfigValue, error) {
	if err := r.fail("ListConfig"); err != nil {
		return nil, err
	}
	var out []domain.PoolConfigValue
	for _, v := range r.Configs {
		if v.PoolID == poolID && string(v.Name) > after {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetConfig implements pools.Repository.
func (r *Repo) GetConfig(_ context.Context, _ port.Tx, _, poolID string, name domain.ConfigName, _ bool) (domain.PoolConfigValue, error) {
	if err := r.fail("GetConfig"); err != nil {
		return domain.PoolConfigValue{}, err
	}
	v, ok := r.Configs[ck(poolID, name)]
	if !ok {
		return domain.PoolConfigValue{}, domain.NotFound("configuration value")
	}
	return v, nil
}

// NamespaceValueExists implements pools.Repository.
func (r *Repo) NamespaceValueExists(_ context.Context, _ port.Tx, teamID string, name domain.ConfigName) (bool, error) {
	return r.Namespace[teamID+"/"+string(name)], r.fail("NamespaceValueExists")
}

// InsertConfig implements pools.Repository.
func (r *Repo) InsertConfig(_ context.Context, tx port.Tx, v domain.PoolConfigValue) error {
	if err := r.fail("InsertConfig"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	v.Version = 1
	v.ResolvedValue = v.Value
	r.Configs[ck(v.PoolID, v.Name)] = v
	return nil
}

// UpdateConfig implements pools.Repository.
func (r *Repo) UpdateConfig(_ context.Context, tx port.Tx, v domain.PoolConfigValue, expect domain.Version) error {
	if err := r.fail("UpdateConfig"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	if r.Configs[ck(v.PoolID, v.Name)].Version != expect {
		return domain.PreconditionFailed()
	}
	v.Version = expect + 1
	v.ResolvedValue = v.Value
	r.Configs[ck(v.PoolID, v.Name)] = v
	return nil
}

// DeleteConfig implements pools.Repository.
func (r *Repo) DeleteConfig(_ context.Context, tx port.Tx, _, poolID string, name domain.ConfigName, expect domain.Version) error {
	if err := r.fail("DeleteConfig"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	if r.Configs[ck(poolID, name)].Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.Configs, ck(poolID, name))
	return nil
}

func (r *Repo) access(poolID string, s domain.ExternalSystemAccess) domain.ExternalSystemAccess {
	if s.DisabledAt == "" && r.Optouts[poolID+"/"+s.ID] {
		s.DisabledAt = domain.ExtSystemDisabledAtPool
	}
	return s
}

// ListSystems implements pools.Repository.
func (r *Repo) ListSystems(_ context.Context, _ port.Tx, _, poolID, after string, limit int) ([]domain.ExternalSystemAccess, error) {
	if err := r.fail("ListSystems"); err != nil {
		return nil, err
	}
	var out []domain.ExternalSystemAccess
	for _, s := range r.Systems {
		if s.ID > after {
			out = append(out, r.access(poolID, s))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetSystem implements pools.Repository.
func (r *Repo) GetSystem(_ context.Context, _ port.Tx, _, poolID, systemID string) (domain.ExternalSystemAccess, error) {
	if err := r.fail("GetSystem"); err != nil {
		return domain.ExternalSystemAccess{}, err
	}
	s, ok := r.Systems[systemID]
	if !ok {
		return domain.ExternalSystemAccess{}, domain.NotFound("external system")
	}
	return r.access(poolID, s), nil
}

// SetSystemOff implements pools.Repository.
func (r *Repo) SetSystemOff(_ context.Context, tx port.Tx, _, poolID, systemID, _ string, off bool) error {
	if err := r.fail("SetSystemOff"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	if off {
		r.Optouts[poolID+"/"+systemID] = true
	} else {
		delete(r.Optouts, poolID+"/"+systemID)
	}
	return nil
}

// AccountAllowed implements pools.Repository.
func (r *Repo) AccountAllowed(_ context.Context, _ port.Tx, _, accountID string) (bool, error) {
	return r.Allowed[accountID], r.fail("AccountAllowed")
}

// LinkRoleName implements pools.Repository.
func (r *Repo) LinkRoleName(_ context.Context, _ port.Tx, _, poolID string) (string, error) {
	if err := r.fail("LinkRoleName"); err != nil {
		return "", err
	}
	for _, l := range r.Links {
		if l.PoolID == poolID {
			return l.RoleName, nil
		}
	}
	return "", nil
}

// ListLinks implements pools.Repository.
func (r *Repo) ListLinks(_ context.Context, _ port.Tx, _, poolID, after string, limit int) ([]domain.PoolRoleLink, error) {
	if err := r.fail("ListLinks"); err != nil {
		return nil, err
	}
	var out []domain.PoolRoleLink
	for _, l := range r.Links {
		if l.PoolID == poolID && l.ID > after {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// GetLink implements pools.Repository.
func (r *Repo) GetLink(_ context.Context, _ port.Tx, _, poolID, linkID string, _ bool) (domain.PoolRoleLink, error) {
	if err := r.fail("GetLink"); err != nil {
		return domain.PoolRoleLink{}, err
	}
	l, ok := r.Links[linkID]
	if !ok || l.PoolID != poolID {
		return domain.PoolRoleLink{}, domain.NotFound("role link")
	}
	return l, nil
}

// InsertLink implements pools.Repository.
func (r *Repo) InsertLink(_ context.Context, tx port.Tx, l domain.PoolRoleLink) error {
	if err := r.fail("InsertLink"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	for _, o := range r.Links {
		if o.PoolID == l.PoolID && (o.AccountID == l.AccountID || o.ProfileName == l.ProfileName) {
			return domain.Conflict("duplicate", "link exists", nil)
		}
	}
	r.Links[l.ID] = l
	return nil
}

// DeleteLink implements pools.Repository.
func (r *Repo) DeleteLink(_ context.Context, tx port.Tx, _, _, linkID string, expect domain.Version) error {
	if err := r.fail("DeleteLink"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	if r.Links[linkID].Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.Links, linkID)
	return nil
}

// RequestVerify implements pools.Repository.
func (r *Repo) RequestVerify(_ context.Context, tx port.Tx, _, _, linkID string) (domain.PoolRoleLink, error) {
	if err := r.fail("RequestVerify"); err != nil {
		return domain.PoolRoleLink{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	l := r.Links[linkID]
	l.Status, l.LastError = domain.RoleLinkStatusPendingTrust, ""
	r.Links[linkID] = l
	return l, nil
}

var _ pools.Repository = (*Repo)(nil)

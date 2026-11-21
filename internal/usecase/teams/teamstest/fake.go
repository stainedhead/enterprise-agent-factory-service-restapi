// Package teamstest is an in-memory teams.Repository for use case tests. Writes are undone when the
// fake transaction rolls back.
package teamstest

import (
	"context"
	"maps"
	"slices"
	"sort"
	"sync"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
)

// Repo implements teams.Repository.
type Repo struct {
	mu          sync.Mutex
	Teams       map[string]domain.Team
	Budgets     map[string]domain.Money
	Users       map[string]string // email -> id
	Bindings    map[string]domain.NewMemberParams
	Catalog     map[string]teams.ModelInfo
	TeamModels  map[string][]string
	TeamModelV  map[string]int64
	Service     []domain.ServiceTag
	Tags        map[string][]domain.TagValue
	TagV        map[string]int64
	InsertErr   error
	BindingTeam map[string]string
}

// New returns an empty Repo.
func New() *Repo {
	return &Repo{Teams: map[string]domain.Team{}, Budgets: map[string]domain.Money{}, Users: map[string]string{},
		Bindings: map[string]domain.NewMemberParams{}, Catalog: map[string]teams.ModelInfo{},
		TeamModels: map[string][]string{}, TeamModelV: map[string]int64{}, Tags: map[string][]domain.TagValue{},
		TagV: map[string]int64{}, BindingTeam: map[string]string{}}
}

// guard registers a rollback that restores the state as it was at the first write of the transaction.
func (r *Repo) guard(tx port.Tx) {
	ft, ok := tx.(*fakes.Tx)
	if !ok {
		return
	}
	snap := struct {
		t  map[string]domain.Team
		b  map[string]domain.Money
		bi map[string]domain.NewMemberParams
		tm map[string][]string
		tg map[string][]domain.TagValue
	}{maps.Clone(r.Teams), maps.Clone(r.Budgets), maps.Clone(r.Bindings), maps.Clone(r.TeamModels), maps.Clone(r.Tags)}
	ft.OnRollback(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.Teams, r.Budgets, r.Bindings, r.TeamModels, r.Tags = snap.t, snap.b, snap.bi, snap.tm, snap.tg
	})
}

func visible(s port.Session, t domain.Team) bool {
	return s.ServiceAdmin || slices.Contains(s.TeamIDs, t.ID)
}

// Insert implements teams.Repository.
func (r *Repo) Insert(_ context.Context, tx port.Tx, t domain.Team) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	if r.InsertErr != nil {
		return r.InsertErr
	}
	for _, o := range r.Teams {
		if o.Slug == t.Slug {
			return domain.Conflict("duplicate", "team slug exists", nil)
		}
	}
	r.Teams[t.ID] = t
	return nil
}

// Get implements teams.Repository.
func (r *Repo) Get(_ context.Context, tx port.Tx, id string, _ bool) (domain.Team, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.Teams[id]
	if !ok || !visible(tx.Session(), t) {
		return domain.Team{}, domain.NotFound("team")
	}
	return t, nil
}

// List implements teams.Repository.
func (r *Repo) List(_ context.Context, tx port.Tx, f teams.ListFilter, after string, limit int) ([]domain.Team, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.Team
	for _, t := range r.Teams {
		switch {
		case !visible(tx.Session(), t), t.ID <= after:
		case f.State != "" && t.State != f.State, f.Kind != "" && t.Kind != f.Kind:
		case f.HideDeleted && t.State == domain.TeamStateDeleted:
		default:
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Update implements teams.Repository.
func (r *Repo) Update(_ context.Context, tx port.Tx, t domain.Team, expect domain.Version) (domain.Team, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.Teams[t.ID]
	if !ok {
		return domain.Team{}, domain.NotFound("team")
	}
	if cur.Version != expect {
		return domain.Team{}, domain.PreconditionFailed()
	}
	r.guard(tx)
	t.Version = cur.Version + 1
	r.Teams[t.ID] = t
	return t, nil
}

// BudgetAmount implements teams.Repository.
func (r *Repo) BudgetAmount(_ context.Context, _ port.Tx, id string) (*domain.Money, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.Budgets[id]; ok {
		return &m, nil
	}
	return nil, nil
}

// InsertBudget implements teams.Repository.
func (r *Repo) InsertBudget(_ context.Context, tx port.Tx, id string, b teams.BudgetInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	r.Budgets[id] = b.Amount
	return nil
}

// ResolveUser implements teams.Repository.
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

// InsertAdminBinding implements teams.Repository.
func (r *Repo) InsertAdminBinding(_ context.Context, tx port.Tx, bid, teamID string, m domain.NewMemberParams) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	r.Bindings[bid] = m
	r.BindingTeam[bid] = teamID
	return nil
}

// Models implements teams.Repository.
func (r *Repo) Models(_ context.Context, _ port.Tx, teamID string) ([]teams.ModelInfo, domain.Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []teams.ModelInfo
	for _, id := range r.TeamModels[teamID] {
		out = append(out, r.Catalog[id])
	}
	return out, domain.Version(r.TeamModelV[teamID] + 1), nil
}

// ModelsByID implements teams.Repository.
func (r *Repo) ModelsByID(_ context.Context, _ port.Tx, ids []string) (map[string]teams.ModelInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]teams.ModelInfo{}
	for _, id := range ids {
		if m, ok := r.Catalog[id]; ok {
			out[id] = m
		}
	}
	return out, nil
}

// ReplaceModels implements teams.Repository.
func (r *Repo) ReplaceModels(_ context.Context, tx port.Tx, teamID string, ids []string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	r.TeamModels[teamID] = slices.Clone(ids)
	r.TeamModelV[teamID]++
	return nil
}

// ServiceTags implements teams.Repository.
func (r *Repo) ServiceTags(context.Context, port.Tx) ([]domain.ServiceTag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.Service), nil
}

// TeamTags implements teams.Repository.
func (r *Repo) TeamTags(_ context.Context, _ port.Tx, teamID string) ([]domain.TagValue, domain.Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.Tags[teamID]), domain.Version(r.TagV[teamID] + 1), nil
}

// ReplaceTeamTags implements teams.Repository.
func (r *Repo) ReplaceTeamTags(_ context.Context, tx port.Tx, teamID string, v []domain.TagValue) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard(tx)
	r.Tags[teamID] = slices.Clone(v)
	r.TagV[teamID]++
	return nil
}

var _ teams.Repository = (*Repo)(nil)

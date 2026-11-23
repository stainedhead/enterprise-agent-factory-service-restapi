package teams_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
)

var errInjected = errors.New("injected failure")

// flaky fails the failAt-th repository call (1-based); 0 never fails.
type flaky struct {
	teams.Repository
	failAt, calls int
}

func (f *flaky) hit() error {
	f.calls++
	if f.calls == f.failAt {
		return errInjected
	}
	return nil
}

func (f *flaky) Insert(ctx context.Context, tx port.Tx, t domain.Team) error {
	if err := f.hit(); err != nil {
		return err
	}
	return f.Repository.Insert(ctx, tx, t)
}

func (f *flaky) Get(ctx context.Context, tx port.Tx, id string, u bool) (domain.Team, error) {
	if err := f.hit(); err != nil {
		return domain.Team{}, err
	}
	return f.Repository.Get(ctx, tx, id, u)
}

func (f *flaky) List(ctx context.Context, tx port.Tx, fl teams.ListFilter, a string, l int) ([]domain.Team, error) {
	if err := f.hit(); err != nil {
		return nil, err
	}
	return f.Repository.List(ctx, tx, fl, a, l)
}

func (f *flaky) Update(ctx context.Context, tx port.Tx, t domain.Team, e domain.Version) (domain.Team, error) {
	if err := f.hit(); err != nil {
		return domain.Team{}, err
	}
	return f.Repository.Update(ctx, tx, t, e)
}

func (f *flaky) BudgetAmount(ctx context.Context, tx port.Tx, id string) (*domain.Money, error) {
	if err := f.hit(); err != nil {
		return nil, err
	}
	return f.Repository.BudgetAmount(ctx, tx, id)
}

func (f *flaky) InsertBudget(ctx context.Context, tx port.Tx, id string, b teams.BudgetInput) error {
	if err := f.hit(); err != nil {
		return err
	}
	return f.Repository.InsertBudget(ctx, tx, id, b)
}

func (f *flaky) ResolveUser(ctx context.Context, tx port.Tx, u, e string) (string, bool, error) {
	if err := f.hit(); err != nil {
		return "", false, err
	}
	return f.Repository.ResolveUser(ctx, tx, u, e)
}

func (f *flaky) InsertAdminBinding(ctx context.Context, tx port.Tx, b, tid string, m domain.NewMemberParams) error {
	if err := f.hit(); err != nil {
		return err
	}
	return f.Repository.InsertAdminBinding(ctx, tx, b, tid, m)
}

func (f *flaky) Models(ctx context.Context, tx port.Tx, id string) (teams.TeamModels, error) {
	if err := f.hit(); err != nil {
		return teams.TeamModels{}, err
	}
	return f.Repository.Models(ctx, tx, id)
}

func (f *flaky) ModelsByID(ctx context.Context, tx port.Tx, ids []string) (map[string]teams.ModelInfo, error) {
	if err := f.hit(); err != nil {
		return nil, err
	}
	return f.Repository.ModelsByID(ctx, tx, ids)
}

func (f *flaky) ReplaceModels(ctx context.Context, tx port.Tx, id string, ids []string) error {
	if err := f.hit(); err != nil {
		return err
	}
	return f.Repository.ReplaceModels(ctx, tx, id, ids)
}

func (f *flaky) ServiceTags(ctx context.Context, tx port.Tx) ([]domain.ServiceTag, error) {
	if err := f.hit(); err != nil {
		return nil, err
	}
	return f.Repository.ServiceTags(ctx, tx)
}

func (f *flaky) TeamTags(ctx context.Context, tx port.Tx, id string) ([]domain.TagValue, domain.Version, error) {
	if err := f.hit(); err != nil {
		return nil, 0, err
	}
	return f.Repository.TeamTags(ctx, tx, id)
}

func (f *flaky) ReplaceTeamTags(ctx context.Context, tx port.Tx, id string, v []domain.TagValue) error {
	if err := f.hit(); err != nil {
		return err
	}
	return f.Repository.ReplaceTeamTags(ctx, tx, id, v)
}

// Every repository call of every operation may fail; the operation then returns the error and a
// mutation leaves no audit or outbox record behind.
func TestRepositoryFailuresPropagateAndWriteNothing(t *testing.T) {
	name := "Renamed"
	ops := map[string]struct {
		mutates bool
		run     func(r *rig, tm domain.Team) error
	}{
		"Create": {true, func(r *rig, _ domain.Team) error {
			in := createInput()
			in.Budget = nil
			in.Params.Slug = "second"
			_, err := r.svc.Create(t.Context(), svcAdmin, in, nil)
			return err
		}},
		"CreateEphemeral": {true, func(r *rig, _ domain.Team) error {
			in := createInput()
			in.Params.Slug = "eph"
			in.Params.Kind = domain.TeamKindEphemeral
			exp := now.AddDate(0, 0, 10)
			in.Params.ExpiresAt = &exp
			in.Params.TeardownRule = domain.TeardownRuleAtExpiry
			in.Budget = &teams.BudgetInput{Amount: domain.MustMoney("10")}
			_, err := r.svc.Create(t.Context(), svcAdmin, in, nil)
			return err
		}},
		"Update": {true, func(r *rig, tm domain.Team) error {
			_, err := r.svc.Update(t.Context(), svcAdmin, tm.ID, tm.Version, domain.TeamPatch{DisplayName: &name, MaxBudget: ptr(domain.MustMoney("2000"))})
			return err
		}},
		"Suspend": {true, func(r *rig, tm domain.Team) error {
			_, err := r.svc.Suspend(t.Context(), svcAdmin, tm.ID, nil)
			return err
		}},
		"Delete": {true, func(r *rig, tm domain.Team) error {
			_, err := r.svc.Delete(t.Context(), svcAdmin, tm.ID, tm.Version)
			return err
		}},
		"PutModels": {true, func(r *rig, tm domain.Team) error {
			_, err := r.svc.PutModels(t.Context(), svcAdmin, tm.ID, nil, domain.ModelAccessSelected, []string{"m1"})
			return err
		}},
		"PutTags": {true, func(r *rig, tm domain.Team) error {
			_, err := r.svc.PutTags(t.Context(), svcAdmin, tm.ID, nil, []domain.TagValue{{Key: "project", Value: "x"}})
			return err
		}},
		"Get": {false, func(r *rig, tm domain.Team) error { _, err := r.svc.Get(t.Context(), svcAdmin, tm.ID); return err }},
		"Models": {false, func(r *rig, tm domain.Team) error {
			_, err := r.svc.Models(t.Context(), svcAdmin, tm.ID)
			return err
		}},
		"Tags": {false, func(r *rig, tm domain.Team) error { _, err := r.svc.Tags(t.Context(), svcAdmin, tm.ID); return err }},
		"List": {false, func(r *rig, _ domain.Team) error {
			_, _, err := r.svc.List(t.Context(), svcAdmin, teams.ListFilter{}, acting.Page{})
			return err
		}},
	}
	for opName, op := range ops {
		for n := 1; ; n++ {
			r := newRig(t)
			tm := r.create(t)
			r.audit.Entries, r.outbox.Events = nil, nil
			f := &flaky{Repository: r.repo, failAt: n}
			r.svc.Repo = f
			err := op.run(r, tm)
			if f.calls < n {
				if err != nil {
					t.Errorf("%s: unexpected error without injection: %v", opName, err)
				}
				break
			}
			if !errors.Is(err, errInjected) {
				t.Errorf("%s: failing repository call %d returned %v", opName, n, err)
			}
			if op.mutates && (len(r.audit.Entries) != 0 || len(r.outbox.Events) != 0) {
				t.Errorf("%s: failing call %d left %d audit and %d outbox records", opName, n, len(r.audit.Entries), len(r.outbox.Events))
			}
			if n > 20 {
				t.Fatalf("%s: runaway", opName)
			}
		}
	}
}

package members_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/members"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

var errInjected = errors.New("injected failure")

type flaky struct {
	members.Repository
	failAt, calls int
}

func (f *flaky) hit() error {
	f.calls++
	if f.calls == f.failAt {
		return errInjected
	}
	return nil
}

func (f *flaky) List(ctx context.Context, tx port.Tx, t, a string, l int) ([]domain.Member, error) {
	if err := f.hit(); err != nil {
		return nil, err
	}
	return f.Repository.List(ctx, tx, t, a, l)
}

func (f *flaky) Get(ctx context.Context, tx port.Tx, t, id string, u bool) (domain.Member, error) {
	if err := f.hit(); err != nil {
		return domain.Member{}, err
	}
	return f.Repository.Get(ctx, tx, t, id, u)
}

func (f *flaky) ResolveUser(ctx context.Context, tx port.Tx, u, e string) (string, bool, error) {
	if err := f.hit(); err != nil {
		return "", false, err
	}
	return f.Repository.ResolveUser(ctx, tx, u, e)
}

func (f *flaky) Insert(ctx context.Context, tx port.Tx, m domain.Member) error {
	if err := f.hit(); err != nil {
		return err
	}
	return f.Repository.Insert(ctx, tx, m)
}

func (f *flaky) UpdateRole(ctx context.Context, tx port.Tx, t, id string, r domain.TeamRole, e domain.Version) (domain.Member, error) {
	if err := f.hit(); err != nil {
		return domain.Member{}, err
	}
	return f.Repository.UpdateRole(ctx, tx, t, id, r, e)
}

func (f *flaky) Delete(ctx context.Context, tx port.Tx, t, id string, e domain.Version) error {
	if err := f.hit(); err != nil {
		return err
	}
	return f.Repository.Delete(ctx, tx, t, id, e)
}

func TestRepositoryFailuresPropagateAndWriteNothing(t *testing.T) {
	admin := caller(authz.RoleTeamAdmin, team)
	ops := map[string]struct {
		mutates bool
		run     func(r *rig, id string) error
	}{
		"Add": {true, func(r *rig, _ string) error {
			_, err := r.svc.Add(t.Context(), admin, team, add("a@example.com", domain.TeamRoleTeamViewer), nil)
			return err
		}},
		"Update": {true, func(r *rig, id string) error {
			_, err := r.svc.Update(t.Context(), admin, team, id, domain.TeamRoleTeamAdmin, 1)
			return err
		}},
		"Remove": {true, func(r *rig, id string) error { return r.svc.Remove(t.Context(), admin, team, id, 1) }},
		"List": {false, func(r *rig, _ string) error {
			_, _, err := r.svc.List(t.Context(), admin, team, acting.Page{})
			return err
		}},
	}
	for name, op := range ops {
		for n := 1; ; n++ {
			r := newRig()
			seed, err := r.svc.Add(t.Context(), admin, team, domain.NewMemberParams{Role: domain.TeamRoleTeamViewer, SubjectType: domain.SubjectTypeGroup, GroupName: "eng"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			r.audit.Entries = nil
			f := &flaky{Repository: r.repo, failAt: n}
			r.svc.Repo = f
			err = op.run(r, seed.Member.ID)
			if f.calls < n {
				if err != nil {
					t.Errorf("%s: unexpected error: %v", name, err)
				}
				break
			}
			if !errors.Is(err, errInjected) {
				t.Errorf("%s: call %d returned %v", name, n, err)
			}
			if op.mutates && len(r.audit.Entries) != 0 {
				t.Errorf("%s: call %d left an audit record", name, n)
			}
			if n > 10 {
				t.Fatalf("%s: runaway", name)
			}
		}
	}
}

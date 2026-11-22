package idp_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/idp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

var errInjected = errors.New("injected failure")

type flaky struct {
	idp.Repository
	failAt, calls int
}

func (f *flaky) hit() error {
	f.calls++
	if f.calls == f.failAt {
		return errInjected
	}
	return nil
}

func (f *flaky) List(ctx context.Context, tx port.Tx, a string, l int) ([]domain.IdentityProvider, error) {
	if err := f.hit(); err != nil {
		return nil, err
	}
	return f.Repository.List(ctx, tx, a, l)
}

func (f *flaky) Get(ctx context.Context, tx port.Tx, id string, u bool) (domain.IdentityProvider, error) {
	if err := f.hit(); err != nil {
		return domain.IdentityProvider{}, err
	}
	return f.Repository.Get(ctx, tx, id, u)
}

func (f *flaky) TeamsUsing(ctx context.Context, tx port.Tx, id string) ([]idp.TeamRef, error) {
	if err := f.hit(); err != nil {
		return nil, err
	}
	return f.Repository.TeamsUsing(ctx, tx, id)
}

func (f *flaky) ClearDefault(ctx context.Context, tx port.Tx, id string) error {
	if err := f.hit(); err != nil {
		return err
	}
	return f.Repository.ClearDefault(ctx, tx, id)
}

func (f *flaky) Insert(ctx context.Context, tx port.Tx, p domain.IdentityProvider) error {
	if err := f.hit(); err != nil {
		return err
	}
	return f.Repository.Insert(ctx, tx, p)
}

func (f *flaky) Update(ctx context.Context, tx port.Tx, p domain.IdentityProvider, e domain.Version) (domain.IdentityProvider, error) {
	if err := f.hit(); err != nil {
		return domain.IdentityProvider{}, err
	}
	return f.Repository.Update(ctx, tx, p, e)
}

func (f *flaky) Delete(ctx context.Context, tx port.Tx, id string, e domain.Version) error {
	if err := f.hit(); err != nil {
		return err
	}
	return f.Repository.Delete(ctx, tx, id, e)
}

func TestRepositoryFailuresPropagateAndWriteNothing(t *testing.T) {
	on := true
	ops := map[string]struct {
		mutates bool
		run     func(s *idp.Service, id string) error
	}{
		"Create": {true, func(s *idp.Service, _ string) error {
			_, _, err := s.Create(t.Context(), svcAdmin, provider("https://new.example.com", true), nil)
			return err
		}},
		"Update": {true, func(s *idp.Service, id string) error {
			def := true
			_, err := s.Update(t.Context(), svcAdmin, id, 1, idp.Patch{IsDefault: &def, Enabled: &on})
			return err
		}},
		"Disable": {true, func(s *idp.Service, id string) error {
			off := false
			_, err := s.Update(t.Context(), svcAdmin, id, 1, idp.Patch{Enabled: &off})
			return err
		}},
		"Delete": {true, func(s *idp.Service, id string) error { return s.Delete(t.Context(), svcAdmin, id, 1) }},
		"Get":    {false, func(s *idp.Service, id string) error { _, _, err := s.Get(t.Context(), svcAdmin, id); return err }},
		"List": {false, func(s *idp.Service, _ string) error {
			_, _, err := s.List(t.Context(), svcAdmin, pageOne())
			return err
		}},
	}
	for name, op := range ops {
		for n := 1; ; n++ {
			s, r, audit := rig()
			seed := provider("https://seed.example.com", false)
			seed.ID, seed.Version = "idp_seed", 1
			r.rows[seed.ID] = seed
			f := &flaky{Repository: r, failAt: n}
			s.Repo = f
			err := op.run(s, seed.ID)
			if f.calls < n {
				if err != nil {
					t.Errorf("%s: unexpected error: %v", name, err)
				}
				break
			}
			if !errors.Is(err, errInjected) {
				t.Errorf("%s: call %d returned %v", name, n, err)
			}
			if op.mutates && len(audit.Entries) != 0 {
				t.Errorf("%s: call %d left an audit record", name, n)
			}
			if n > 20 {
				t.Fatalf("%s: runaway", name)
			}
		}
	}
}

func TestUpdateValidationAndMissing(t *testing.T) {
	s, r, _ := rig()
	seed := provider("https://seed.example.com", false)
	seed.ID, seed.Version = "idp_seed", 1
	r.rows[seed.ID] = seed
	empty := ""
	if _, err := s.Update(t.Context(), svcAdmin, seed.ID, 1, idp.Patch{ConsoleClientID: &empty}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("empty client id = %v", err)
	}
	if _, err := s.Update(t.Context(), svcAdmin, "idp_none", 1, idp.Patch{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("missing = %v", err)
	}
	if err := s.Delete(t.Context(), svcAdmin, "idp_none", 1); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("delete missing = %v", err)
	}
	cli := "cli2"
	claim := "roles"
	if out, err := s.Update(t.Context(), svcAdmin, seed.ID, 1, idp.Patch{CLIClientID: &cli, GroupsClaim: &claim}); err != nil || out.CLIClientID != cli || out.GroupsClaim != claim {
		t.Errorf("update = %+v %v", out, err)
	}
}

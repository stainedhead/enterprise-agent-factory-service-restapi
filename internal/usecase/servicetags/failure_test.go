package servicetags_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/servicetags"
)

var errInjected = errors.New("injected failure")

type flaky struct {
	servicetags.Repository
	failAt, calls int
}

func (f *flaky) hit() error {
	f.calls++
	if f.calls == f.failAt {
		return errInjected
	}
	return nil
}

func (f *flaky) List(ctx context.Context, tx port.Tx, a string, l int) ([]domain.ServiceTag, error) {
	if err := f.hit(); err != nil {
		return nil, err
	}
	return f.Repository.List(ctx, tx, a, l)
}

func (f *flaky) Get(ctx context.Context, tx port.Tx, k string, u bool) (domain.ServiceTag, error) {
	if err := f.hit(); err != nil {
		return domain.ServiceTag{}, err
	}
	return f.Repository.Get(ctx, tx, k, u)
}

func (f *flaky) Insert(ctx context.Context, tx port.Tx, t domain.ServiceTag) error {
	if err := f.hit(); err != nil {
		return err
	}
	return f.Repository.Insert(ctx, tx, t)
}

func (f *flaky) Update(ctx context.Context, tx port.Tx, t domain.ServiceTag, e domain.Version) (domain.ServiceTag, error) {
	if err := f.hit(); err != nil {
		return domain.ServiceTag{}, err
	}
	return f.Repository.Update(ctx, tx, t, e)
}

func (f *flaky) Delete(ctx context.Context, tx port.Tx, k string, e domain.Version) error {
	if err := f.hit(); err != nil {
		return err
	}
	return f.Repository.Delete(ctx, tx, k, e)
}

func TestRepositoryFailuresPropagateAndWriteNothing(t *testing.T) {
	ops := map[string]struct {
		mutates, seed bool
		run           func(s *servicetags.Service) error
	}{
		"PutNew": {true, false, func(s *servicetags.Service) error {
			_, err := s.Put(t.Context(), svcAdmin, domain.ServiceTag{Key: "cost-line", Mode: domain.ServiceTagModeFixed, Value: str("a")}, nil)
			return err
		}},
		"PutExisting": {true, true, func(s *servicetags.Service) error {
			_, err := s.Put(t.Context(), svcAdmin, domain.ServiceTag{Key: "cost-line", Mode: domain.ServiceTagModeFixed, Value: str("b")}, nil)
			return err
		}},
		"Delete": {true, true, func(s *servicetags.Service) error { return s.Delete(t.Context(), svcAdmin, "cost-line", 1) }},
		"List": {false, true, func(s *servicetags.Service) error {
			_, _, err := s.List(t.Context(), svcAdmin, acting.Page{})
			return err
		}},
	}
	for name, op := range ops {
		for n := 1; ; n++ {
			s, audit, _ := rig()
			if op.seed {
				if _, err := s.Put(t.Context(), svcAdmin, domain.ServiceTag{Key: "cost-line", Mode: domain.ServiceTagModeFixed, Value: str("seed")}, nil); err != nil {
					t.Fatal(err)
				}
				audit.Entries = nil
			}
			f := &flaky{Repository: s.Repo, failAt: n}
			s.Repo = f
			err := op.run(s)
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
			if n > 10 {
				t.Fatalf("%s: runaway", name)
			}
		}
	}
}

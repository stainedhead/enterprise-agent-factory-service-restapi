package agenttemplates_test

import (
	"context"
	"errors"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// errBoom is what a flaky repository returns.
var errBoom = errors.New("boom")

// flaky fails its failAt-th call (1-based) with errBoom and passes every other call to the fake.
type flaky struct {
	*repo
	failAt, calls int
}

func (f *flaky) tick() error {
	f.calls++
	if f.calls == f.failAt {
		return errBoom
	}
	return nil
}

func (f *flaky) List(ctx context.Context, tx port.Tx, enabledOnly bool, after string, limit int) ([]domain.AgentTemplate, error) {
	if err := f.tick(); err != nil {
		return nil, err
	}
	return f.repo.List(ctx, tx, enabledOnly, after, limit)
}

func (f *flaky) Get(ctx context.Context, tx port.Tx, name string, forUpdate bool) (domain.AgentTemplate, error) {
	if err := f.tick(); err != nil {
		return domain.AgentTemplate{}, err
	}
	return f.repo.Get(ctx, tx, name, forUpdate)
}

func (f *flaky) Insert(ctx context.Context, tx port.Tx, t domain.AgentTemplate) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.Insert(ctx, tx, t)
}

func (f *flaky) Update(ctx context.Context, tx port.Tx, t domain.AgentTemplate, expect domain.Version) (domain.AgentTemplate, error) {
	if err := f.tick(); err != nil {
		return domain.AgentTemplate{}, err
	}
	return f.repo.Update(ctx, tx, t, expect)
}

func (f *flaky) Delete(ctx context.Context, tx port.Tx, name string, expect domain.Version) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.Delete(ctx, tx, name, expect)
}

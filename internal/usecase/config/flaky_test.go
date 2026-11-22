package config_test

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

func (f *flaky) ListNamespace(ctx context.Context, tx port.Tx, teamID string, after string, limit int) ([]domain.NamespaceConfigValue, error) {
	if err := f.tick(); err != nil {
		return nil, err
	}
	return f.repo.ListNamespace(ctx, tx, teamID, after, limit)
}

func (f *flaky) GetNamespace(ctx context.Context, tx port.Tx, teamID string, name domain.ConfigName, forUpdate bool) (domain.NamespaceConfigValue, error) {
	if err := f.tick(); err != nil {
		return domain.NamespaceConfigValue{}, err
	}
	return f.repo.GetNamespace(ctx, tx, teamID, name, forUpdate)
}

func (f *flaky) InsertNamespace(ctx context.Context, tx port.Tx, v domain.NamespaceConfigValue) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.InsertNamespace(ctx, tx, v)
}

func (f *flaky) UpdateNamespace(ctx context.Context, tx port.Tx, v domain.NamespaceConfigValue, expect domain.Version) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.UpdateNamespace(ctx, tx, v, expect)
}

func (f *flaky) DeleteNamespace(ctx context.Context, tx port.Tx, teamID string, name domain.ConfigName, expect domain.Version) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.DeleteNamespace(ctx, tx, teamID, name, expect)
}

func (f *flaky) AgentExists(ctx context.Context, tx port.Tx, teamID string, agentID string) (bool, error) {
	if err := f.tick(); err != nil {
		return false, err
	}
	return f.repo.AgentExists(ctx, tx, teamID, agentID)
}

func (f *flaky) ListAgent(ctx context.Context, tx port.Tx, teamID string, agentID string, after string, limit int) ([]domain.AgentConfigValue, error) {
	if err := f.tick(); err != nil {
		return nil, err
	}
	return f.repo.ListAgent(ctx, tx, teamID, agentID, after, limit)
}

func (f *flaky) GetAgent(ctx context.Context, tx port.Tx, teamID string, agentID string, name domain.ConfigName, forUpdate bool) (domain.AgentConfigValue, error) {
	if err := f.tick(); err != nil {
		return domain.AgentConfigValue{}, err
	}
	return f.repo.GetAgent(ctx, tx, teamID, agentID, name, forUpdate)
}

func (f *flaky) InsertAgent(ctx context.Context, tx port.Tx, v domain.AgentConfigValue) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.InsertAgent(ctx, tx, v)
}

func (f *flaky) UpdateAgent(ctx context.Context, tx port.Tx, v domain.AgentConfigValue, expect domain.Version) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.UpdateAgent(ctx, tx, v, expect)
}

func (f *flaky) DeleteAgent(ctx context.Context, tx port.Tx, teamID string, agentID string, name domain.ConfigName, expect domain.Version) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.DeleteAgent(ctx, tx, teamID, agentID, name, expect)
}

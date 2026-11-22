package extsys_test

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

func (f *flaky) TeamMayDeclare(ctx context.Context, tx port.Tx, teamID string) (bool, error) {
	if err := f.tick(); err != nil {
		return false, err
	}
	return f.repo.TeamMayDeclare(ctx, tx, teamID)
}

func (f *flaky) ListTeam(ctx context.Context, tx port.Tx, teamID string, after string, limit int) ([]domain.ExternalSystemAccess, error) {
	if err := f.tick(); err != nil {
		return nil, err
	}
	return f.repo.ListTeam(ctx, tx, teamID, after, limit)
}

func (f *flaky) GetTeam(ctx context.Context, tx port.Tx, teamID string, systemID string, forUpdate bool) (domain.ExternalSystemAccess, error) {
	if err := f.tick(); err != nil {
		return domain.ExternalSystemAccess{}, err
	}
	return f.repo.GetTeam(ctx, tx, teamID, systemID, forUpdate)
}

func (f *flaky) CatalogSlugExists(ctx context.Context, tx port.Tx, slug string) (bool, error) {
	if err := f.tick(); err != nil {
		return false, err
	}
	return f.repo.CatalogSlugExists(ctx, tx, slug)
}

func (f *flaky) Insert(ctx context.Context, tx port.Tx, s domain.ExternalSystem) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.Insert(ctx, tx, s)
}

func (f *flaky) Update(ctx context.Context, tx port.Tx, s domain.ExternalSystem, expect domain.Version) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.Update(ctx, tx, s, expect)
}

func (f *flaky) Delete(ctx context.Context, tx port.Tx, teamID string, systemID string, expect domain.Version) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.Delete(ctx, tx, teamID, systemID, expect)
}

func (f *flaky) SetTeamOff(ctx context.Context, tx port.Tx, teamID string, systemID string, optOutID string, off bool) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.SetTeamOff(ctx, tx, teamID, systemID, optOutID, off)
}

func (f *flaky) AgentExists(ctx context.Context, tx port.Tx, teamID string, agentID string) (bool, error) {
	if err := f.tick(); err != nil {
		return false, err
	}
	return f.repo.AgentExists(ctx, tx, teamID, agentID)
}

func (f *flaky) ListAgent(ctx context.Context, tx port.Tx, teamID string, agentID string, after string, limit int) ([]domain.ExternalSystemAccess, error) {
	if err := f.tick(); err != nil {
		return nil, err
	}
	return f.repo.ListAgent(ctx, tx, teamID, agentID, after, limit)
}

func (f *flaky) GetAgent(ctx context.Context, tx port.Tx, teamID string, agentID string, systemID string) (domain.ExternalSystemAccess, error) {
	if err := f.tick(); err != nil {
		return domain.ExternalSystemAccess{}, err
	}
	return f.repo.GetAgent(ctx, tx, teamID, agentID, systemID)
}

func (f *flaky) SetAgentOff(ctx context.Context, tx port.Tx, teamID string, agentID string, systemID string, optOutID string, off bool) error {
	if err := f.tick(); err != nil {
		return err
	}
	return f.repo.SetAgentOff(ctx, tx, teamID, agentID, systemID, optOutID, off)
}

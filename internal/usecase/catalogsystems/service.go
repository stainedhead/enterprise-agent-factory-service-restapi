// Package catalogsystems implements the service-level external system catalog: ListCatalogSystems,
// CreateCatalogSystem, GetCatalogSystem, UpdateCatalogSystem and DeleteCatalogSystem (API contract
// §4.11, EXT-1; M2-A5). Built-in entries can be edited and switched off but never deleted. Contract
// §7 has no external system event (CD-7).
package catalogsystems

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kind = "external_systems"

// Repository is the service-level part of the external_systems store (team_id is NULL).
type Repository interface {
	// List returns catalog systems with id greater than after, in id order.
	List(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.ExternalSystem, error)
	// Get returns one catalog system, forUpdate locking it; NotFound when absent or team-declared.
	Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.ExternalSystem, error)
	// Insert stores a system; a duplicate slug is a Conflict.
	Insert(ctx context.Context, tx port.Tx, s domain.ExternalSystem) error
	// Update stores the system; PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, s domain.ExternalSystem, expect domain.Version) (domain.ExternalSystem, error)
	// Delete removes the system; PreconditionFailed when the version is not expect.
	Delete(ctx context.Context, tx port.Tx, id string, expect domain.Version) error
}

// Service is the catalog system use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// Created is a system with the replayed response of an idempotent create.
type Created struct {
	System domain.ExternalSystem
	Replay *port.StoredResponse
}

func (s *Service) check(ctx context.Context, c acting.Caller, action string) error {
	if !authz.Authorize(c.Principal, authz.AdminExternalSystems, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return s.Denied(ctx, c, action, "", authz.AdminExternalSystems)
	}
	return nil
}

func audit(x domain.ExternalSystem) map[string]any {
	return map[string]any{"id": x.ID, "slug": x.Slug, "name": x.Name, "category": x.Category, "description": x.Description,
		"docsUrl": x.DocsURL, "endpoints": x.Endpoints, "enabled": x.Enabled, "builtin": x.Builtin}
}

// List implements ListCatalogSystems.
func (s *Service) List(ctx context.Context, c acting.Caller, p acting.Page) ([]domain.ExternalSystem, string, error) {
	if err := s.check(ctx, c, "ListCatalogSystems"); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.ExternalSystem
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(x domain.ExternalSystem) string { return x.ID })
	return page, next, nil
}

// Get implements GetCatalogSystem.
func (s *Service) Get(ctx context.Context, c acting.Caller, id string) (domain.ExternalSystem, error) {
	if err := s.check(ctx, c, "GetCatalogSystem"); err != nil {
		return domain.ExternalSystem{}, err
	}
	var out domain.ExternalSystem
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		out, err = s.Repo.Get(ctx, tx, id, false)
		return err
	})
	return out, err
}

// Create implements CreateCatalogSystem.
func (s *Service) Create(ctx context.Context, c acting.Caller, in domain.NewCatalogSystemParams, idem *acting.Idem) (Created, error) {
	var res Created
	if err := s.check(ctx, c, "CreateCatalogSystem"); err != nil {
		return res, err
	}
	sys, err := domain.NewCatalogSystem(in)
	if err != nil {
		return res, err
	}
	err = s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Created{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		id, err := s.IDs.NewID(domain.PrefixExtSystem)
		if err != nil {
			return err
		}
		sys.ID = id.String()
		if err := s.Repo.Insert(ctx, tx, sys); err != nil {
			return err
		}
		stored, err := s.Repo.Get(ctx, tx, sys.ID, false)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateCatalogSystem", kind, stored.ID, "", nil, audit(stored))); err != nil {
			return err
		}
		res.System = stored
		return idem.Complete(ctx, s.Idem, tx, stored)
	})
	return res, err
}

// Update implements UpdateCatalogSystem: endpoints, text fields and the service-level switch
// (If-Match required).
func (s *Service) Update(ctx context.Context, c acting.Caller, id string, u domain.SystemUpdate, expect domain.Version) (domain.ExternalSystem, error) {
	if err := s.check(ctx, c, "UpdateCatalogSystem"); err != nil {
		return domain.ExternalSystem{}, err
	}
	var out domain.ExternalSystem
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		next, err := cur.Apply(u)
		if err != nil {
			return err
		}
		if out, err = s.Repo.Update(ctx, tx, next, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("UpdateCatalogSystem", kind, id, "", audit(cur), audit(out)))
	})
	return out, err
}

// Delete implements DeleteCatalogSystem (If-Match required). A built-in entry is a Conflict: switch
// it off instead.
func (s *Service) Delete(ctx context.Context, c acting.Caller, id string, expect domain.Version) error {
	if err := s.check(ctx, c, "DeleteCatalogSystem"); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if cur.Builtin {
			return domain.Conflict("builtin", "built-in system "+cur.Slug+" cannot be deleted; switch it off instead", nil)
		}
		if err := s.Repo.Delete(ctx, tx, id, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteCatalogSystem", kind, id, "", audit(cur), nil))
	})
}

// Package storageclasses implements ListStorageClasses, ListStorageClassesAdmin, PutStorageClass and
// DeleteStorageClass (API contract §4.4 and §4.11; M2-A3). Contract §7 has no storage class event,
// so a change writes the row and an audit record only (CD-7).
package storageclasses

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kind = "storage_classes"

// Repository is the storage_classes store.
type Repository interface {
	// List returns classes with name greater than after, in name order; enabledOnly hides disabled ones.
	List(ctx context.Context, tx port.Tx, enabledOnly bool, after string, limit int) ([]domain.StorageClass, error)
	// Get returns one class, forUpdate locking it; NotFound when absent.
	Get(ctx context.Context, tx port.Tx, name string, forUpdate bool) (domain.StorageClass, error)
	Insert(ctx context.Context, tx port.Tx, c domain.StorageClass) error
	// Update stores the class; PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, c domain.StorageClass, expect domain.Version) (domain.StorageClass, error)
	// Delete removes the class; a class agents use is a Conflict (dependents_exist).
	Delete(ctx context.Context, tx port.Tx, name string, expect domain.Version) error
}

// Service is the storage class use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

func (s *Service) check(ctx context.Context, c acting.Caller, action string, perm authz.Permission, scope authz.Scope) error {
	if !authz.Authorize(c.Principal, perm, authz.Resource{Scope: scope}).Allowed() {
		return s.Denied(ctx, c, action, "", perm)
	}
	return nil
}

func audit(c domain.StorageClass) map[string]any {
	return map[string]any{"name": c.Name, "description": c.Description, "volumeType": c.VolumeType, "enabled": c.Enabled}
}

func (s *Service) list(ctx context.Context, c acting.Caller, enabledOnly bool, p acting.Page) ([]domain.StorageClass, string, error) {
	p = p.Normalized()
	var rows []domain.StorageClass
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, enabledOnly, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(sc domain.StorageClass) string { return sc.Name })
	return page, next, nil
}

// ListEnabled implements ListStorageClasses: the classes agents may use (models:read).
func (s *Service) ListEnabled(ctx context.Context, c acting.Caller, p acting.Page) ([]domain.StorageClass, string, error) {
	if err := s.check(ctx, c, "ListStorageClasses", authz.ModelsRead, authz.ScopeAny); err != nil {
		return nil, "", err
	}
	return s.list(ctx, c, true, p)
}

// ListAll implements ListStorageClassesAdmin (admin:storage-classes).
func (s *Service) ListAll(ctx context.Context, c acting.Caller, p acting.Page) ([]domain.StorageClass, string, error) {
	if err := s.check(ctx, c, "ListStorageClassesAdmin", authz.AdminStorageClasses, authz.ScopeService); err != nil {
		return nil, "", err
	}
	return s.list(ctx, c, false, p)
}

// Put implements PutStorageClass: an upsert. A change to an existing class may carry If-Match.
func (s *Service) Put(ctx context.Context, c acting.Caller, in domain.StorageClass, expect *domain.Version) (domain.StorageClass, error) {
	if err := s.check(ctx, c, "PutStorageClass", authz.AdminStorageClasses, authz.ScopeService); err != nil {
		return domain.StorageClass{}, err
	}
	sc, err := domain.NewStorageClass(in.Name, in.Description, in.VolumeType, &in.Enabled)
	if err != nil {
		return domain.StorageClass{}, err
	}
	var out domain.StorageClass
	err = s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, sc.Name, true)
		switch {
		case domain.KindOf(err) == domain.KindNotFound:
			if expect != nil {
				return domain.PreconditionFailed()
			}
			if err := s.Repo.Insert(ctx, tx, sc); err != nil {
				return err
			}
			if out, err = s.Repo.Get(ctx, tx, sc.Name, false); err != nil {
				return err
			}
			return s.Audit.Write(ctx, tx, c.Record("PutStorageClass", kind, sc.Name, "", nil, audit(out)))
		case err != nil:
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		if out, err = s.Repo.Update(ctx, tx, sc, cur.Version); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("PutStorageClass", kind, sc.Name, "", audit(cur), audit(out)))
	})
	return out, err
}

// Delete implements DeleteStorageClass (If-Match required). A class agents use is a Conflict:
// disable it instead.
func (s *Service) Delete(ctx context.Context, c acting.Caller, name string, expect domain.Version) error {
	if err := s.check(ctx, c, "DeleteStorageClass", authz.AdminStorageClasses, authz.ScopeService); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, name, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if err := s.Repo.Delete(ctx, tx, name, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteStorageClass", kind, name, "", audit(cur), nil))
	})
}

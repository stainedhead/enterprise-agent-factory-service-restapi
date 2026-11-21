// Package servicetags implements ListServiceTags, PutServiceTag and DeleteServiceTag (API contract
// §4.11, TAG-1). Contract §7 has no service tag event, so a change writes the row and an audit
// record only (CD-7).
package servicetags

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repository is the service_tags store.
type Repository interface {
	// List returns tags with key greater than after, in key order.
	List(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.ServiceTag, error)
	// Get returns one tag, forUpdate locking it; NotFound when absent.
	Get(ctx context.Context, tx port.Tx, key string, forUpdate bool) (domain.ServiceTag, error)
	Insert(ctx context.Context, tx port.Tx, t domain.ServiceTag) error
	Update(ctx context.Context, tx port.Tx, t domain.ServiceTag, expect domain.Version) (domain.ServiceTag, error)
	Delete(ctx context.Context, tx port.Tx, key string, expect domain.Version) error
}

// Service is the service tag use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

func (s *Service) check(ctx context.Context, c acting.Caller, action string) error {
	if !authz.Authorize(c.Principal, authz.AdminServiceTags, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return s.Denied(ctx, c, action, "", authz.AdminServiceTags)
	}
	return nil
}

func audit(t domain.ServiceTag) map[string]any {
	return map[string]any{"key": t.Key, "mode": t.Mode, "value": t.Value, "defaultValue": t.DefaultValue,
		"allowedValues": t.AllowedValues, "description": t.Description}
}

// List implements ListServiceTags.
func (s *Service) List(ctx context.Context, c acting.Caller, p acting.Page) ([]domain.ServiceTag, string, error) {
	if err := s.check(ctx, c, "ListServiceTags"); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.ServiceTag
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(t domain.ServiceTag) string { return t.Key })
	return page, next, nil
}

// Put implements PutServiceTag: an upsert. A change to an existing tag may carry If-Match (expect).
func (s *Service) Put(ctx context.Context, c acting.Caller, in domain.ServiceTag, expect *domain.Version) (domain.ServiceTag, error) {
	if err := s.check(ctx, c, "PutServiceTag"); err != nil {
		return domain.ServiceTag{}, err
	}
	if err := in.Validate(); err != nil {
		return domain.ServiceTag{}, err
	}
	var out domain.ServiceTag
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, in.Key, true)
		switch {
		case domain.KindOf(err) == domain.KindNotFound:
			if expect != nil {
				return domain.PreconditionFailed()
			}
			if err := s.Repo.Insert(ctx, tx, in); err != nil {
				return err
			}
			if out, err = s.Repo.Get(ctx, tx, in.Key, false); err != nil {
				return err
			}
			return s.Audit.Write(ctx, tx, c.Record("PutServiceTag", "service_tags", in.Key, "", nil, audit(out)))
		case err != nil:
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		if out, err = s.Repo.Update(ctx, tx, in, cur.Version); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("PutServiceTag", "service_tags", in.Key, "", audit(cur), audit(out)))
	})
	return out, err
}

// Delete implements DeleteServiceTag (If-Match required).
func (s *Service) Delete(ctx context.Context, c acting.Caller, key string, expect domain.Version) error {
	if err := s.check(ctx, c, "DeleteServiceTag"); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, key, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if err := s.Repo.Delete(ctx, tx, key, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteServiceTag", "service_tags", key, "", audit(cur), nil))
	})
}

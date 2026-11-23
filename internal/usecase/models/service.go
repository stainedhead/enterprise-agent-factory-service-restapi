// Package models implements ListModels, ListAdminModels, CreateModel, UpdateModel, ListModelPrices
// and CreateModelPrice (API contract §4.11; M2-A1, M2-A2). Contract §7 has no model or price event
// (price proposal events belong to the billing job), so a change writes the row and an audit
// record only (CD-7).
package models

import (
	"context"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const (
	kindModel = "models"
	kindPrice = "model_prices"
)

// Repository is the models and model_prices store.
type Repository interface {
	// List returns models with id greater than after, in id order; usableOnly hides the models the
	// service may not use (disabled while the service's access is selected). withTeams loads
	// TeamsWithAccess.
	List(ctx context.Context, tx port.Tx, usableOnly, withTeams bool, after string, limit int) ([]domain.Model, error)
	// ServiceAccess returns the service-wide model access with its counts and the setting's version.
	ServiceAccess(ctx context.Context, tx port.Tx) (domain.ServiceModelAccess, error)
	// PutServiceAccess stores models.service_access; with expect set the setting must be at that
	// version (PreconditionFailed otherwise).
	PutServiceAccess(ctx context.Context, tx port.Tx, access domain.ModelAccess, expect *domain.Version) error
	// Get returns one model, forUpdate locking it; NotFound when absent.
	Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.Model, error)
	// Insert stores a model; a duplicate id is a Conflict.
	Insert(ctx context.Context, tx port.Tx, m domain.Model) error
	// Update stores m; PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, m domain.Model, expect domain.Version) (domain.Model, error)
	// ListPrices returns the prices of a model with effectiveFrom before the cursor, newest first.
	ListPrices(ctx context.Context, tx port.Tx, modelID string, before *time.Time, limit int) ([]domain.ModelPrice, error)
	// InsertPrice stores a price and returns it as stored; the same effective time is a Conflict.
	InsertPrice(ctx context.Context, tx port.Tx, p domain.ModelPrice) (domain.ModelPrice, error)
}

// Service is the model catalog use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// Created is a result with the replayed response of an idempotent create.
type Created[T any] struct {
	Value  T
	Replay *port.StoredResponse
}

func (s *Service) check(ctx context.Context, c acting.Caller, action string, perm authz.Permission, scope authz.Scope) error {
	if !authz.Authorize(c.Principal, perm, authz.Resource{Scope: scope}).Allowed() {
		return s.Denied(ctx, c, action, "", perm)
	}
	return nil
}

func auditModel(m domain.Model) map[string]any {
	return map[string]any{"id": m.ID, "provider": m.Provider, "displayName": m.DisplayName, "enabled": m.Enabled,
		"contextWindow": m.ContextWindow, "maxOutputTokens": m.MaxOutputTokens}
}

func auditPrice(p domain.ModelPrice) map[string]any {
	return map[string]any{"modelId": p.ModelID, "effectiveFrom": p.EffectiveFrom.UTC().Format(time.RFC3339), "inputUsdPerMtok": p.Input.String(),
		"outputUsdPerMtok": p.Output.String(), "cacheReadUsdPerMtok": p.CacheRead.String(), "cacheWriteUsdPerMtok": p.CacheWrite.String(), "source": p.Source}
}

func (s *Service) list(ctx context.Context, c acting.Caller, usableOnly, withTeams bool, p acting.Page) ([]domain.Model, string, error) {
	p = p.Normalized()
	var rows []domain.Model
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, usableOnly, withTeams, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(m domain.Model) string { return m.ID })
	return page, next, nil
}

// ListEnabled implements ListModels: the models the service may use (enabled, or all while the service's access is all), for any signed-in caller (models:read).
func (s *Service) ListEnabled(ctx context.Context, c acting.Caller, p acting.Page) ([]domain.Model, string, error) {
	if err := s.check(ctx, c, "ListModels", authz.ModelsRead, authz.ScopeAny); err != nil {
		return nil, "", err
	}
	return s.list(ctx, c, true, false, p)
}

// ListAll implements ListAdminModels: every model, enabled or not (admin:models).
func (s *Service) ListAll(ctx context.Context, c acting.Caller, p acting.Page) ([]domain.Model, string, error) {
	if err := s.check(ctx, c, "ListAdminModels", authz.AdminModels, authz.ScopeService); err != nil {
		return nil, "", err
	}
	return s.list(ctx, c, false, true, p)
}

// Create implements CreateModel. maxOutputTokens is required (D28).
func (s *Service) Create(ctx context.Context, c acting.Caller, in domain.NewModelParams, idem *acting.Idem) (Created[domain.Model], error) {
	var res Created[domain.Model]
	if err := s.check(ctx, c, "CreateModel", authz.AdminModels, authz.ScopeService); err != nil {
		return res, err
	}
	m, err := domain.NewModel(in)
	if err != nil {
		return res, err
	}
	err = s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Created[domain.Model]{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		if err := s.Repo.Insert(ctx, tx, m); err != nil {
			return err
		}
		stored, err := s.Repo.Get(ctx, tx, m.ID, false)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateModel", kindModel, stored.ID, "", nil, auditModel(stored))); err != nil {
			return err
		}
		res.Value = stored
		return idem.Complete(ctx, s.Idem, tx, stored)
	})
	return res, err
}

// Update implements UpdateModel (If-Match required).
func (s *Service) Update(ctx context.Context, c acting.Caller, id string, u domain.ModelUpdate, expect domain.Version) (domain.Model, error) {
	if err := s.check(ctx, c, "UpdateModel", authz.AdminModels, authz.ScopeService); err != nil {
		return domain.Model{}, err
	}
	var out domain.Model
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
		if next.Enabled != cur.Enabled {
			// With service access all every model is usable and the flag cannot change (CD-35). The database
			// guard says the same; checking here makes the answer independent of the connection's role.
			acc, err := s.Repo.ServiceAccess(ctx, tx)
			if err != nil {
				return err
			}
			if acc.Access == domain.ModelAccessAll {
				return domain.Conflict(domain.ReasonServiceModelAccessAll, "models.service_access is all: every model is usable and the enabled flag cannot change", nil)
			}
		}
		if out, err = s.Repo.Update(ctx, tx, next, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("UpdateModel", kindModel, id, "", auditModel(cur), auditModel(out)))
	})
	return out, err
}

// ListPrices implements ListModelPrices: newest first; the cursor is the effective time of the last
// price of the previous page.
func (s *Service) ListPrices(ctx context.Context, c acting.Caller, modelID string, p acting.Page) ([]domain.ModelPrice, string, error) {
	if err := s.check(ctx, c, "ListModelPrices", authz.AdminPricing, authz.ScopeService); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var before *time.Time
	if p.Cursor != "" {
		t, err := time.Parse(time.RFC3339Nano, p.Cursor)
		if err != nil {
			return nil, "", domain.Validation(domain.FieldError{Field: "nextToken", Reason: "invalid or expired token"})
		}
		before = &t
	}
	var rows []domain.ModelPrice
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if _, err := s.Repo.Get(ctx, tx, modelID, false); err != nil {
			return err
		}
		var err error
		rows, err = s.Repo.ListPrices(ctx, tx, modelID, before, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(r domain.ModelPrice) string { return r.EffectiveFrom.UTC().Format(time.RFC3339Nano) })
	return page, next, nil
}

// CreatePrice implements CreateModelPrice: a new effective-dated manual price (admin:pricing).
func (s *Service) CreatePrice(ctx context.Context, c acting.Caller, modelID string, in domain.NewModelPriceParams, idem *acting.Idem) (Created[domain.ModelPrice], error) {
	var res Created[domain.ModelPrice]
	if err := s.check(ctx, c, "CreateModelPrice", authz.AdminPricing, authz.ScopeService); err != nil {
		return res, err
	}
	price, err := domain.NewModelPrice(modelID, in)
	if err != nil {
		return res, err
	}
	err = s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Created[domain.ModelPrice]{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		if _, err := s.Repo.Get(ctx, tx, modelID, true); err != nil {
			return err
		}
		stored, err := s.Repo.InsertPrice(ctx, tx, price)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateModelPrice", kindPrice, modelID, "", nil, auditPrice(stored))); err != nil {
			return err
		}
		res.Value = stored
		return idem.Complete(ctx, s.Idem, tx, stored)
	})
	return res, err
}

// GetServiceAccess returns the service-wide model access (admin:models).
func (s *Service) GetServiceAccess(ctx context.Context, c acting.Caller) (domain.ServiceModelAccess, error) {
	var out domain.ServiceModelAccess
	if err := s.check(ctx, c, "GetServiceModelAccess", authz.AdminModels, authz.ScopeService); err != nil {
		return out, err
	}
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		out, err = s.Repo.ServiceAccess(ctx, tx)
		return err
	})
	return out, err
}

// PutServiceAccess implements PutServiceModelAccess (admin:models, MOD-1). It writes a service
// setting, so like PutSetting it emits an audit record and no event (CD-35). expect is the optional
// If-Match on the setting's version.
func (s *Service) PutServiceAccess(ctx context.Context, c acting.Caller, access string, expect *domain.Version) (domain.ServiceModelAccess, error) {
	var out domain.ServiceModelAccess
	if err := s.check(ctx, c, "PutServiceModelAccess", authz.AdminModels, authz.ScopeService); err != nil {
		return out, err
	}
	a, err := domain.ParseServiceModelAccess(access)
	if err != nil {
		return out, err
	}
	err = s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		before, err := s.Repo.ServiceAccess(ctx, tx)
		if err != nil {
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(before.Version, *expect); err != nil {
				return err
			}
		}
		if err := s.Repo.PutServiceAccess(ctx, tx, a, expect); err != nil {
			return err
		}
		if out, err = s.Repo.ServiceAccess(ctx, tx); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("PutServiceModelAccess", "service_settings", "models.service_access", "",
			map[string]any{"access": string(before.Access)}, map[string]any{"access": string(out.Access)}))
	})
	return out, err
}

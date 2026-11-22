// Package swarmtemplates implements the swarm template operations of API contract §4.10a (M6-B1):
// list and get (swarm-templates:read, also held by security operators), create, update, enable,
// disable and delete (admin:swarm-templates, service admins only). Disabling or changing a template
// touches no existing team or work order: only a work order instantiated later reads it. Contract §7
// has no template event (CD-7), so these mutations write audit rows and no outbox events.
package swarmtemplates

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kind = "swarm_templates"

// Repository is the swarm_templates store.
type Repository interface {
	// List returns templates with name greater than after, in name order.
	List(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.SwarmTemplate, error)
	// Get returns one template by id, forUpdate locking it; NotFound when absent.
	Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.SwarmTemplate, error)
	// Insert stores a new template; a name already taken is a Conflict.
	Insert(ctx context.Context, tx port.Tx, t domain.SwarmTemplate) error
	// Update stores the template when its version is expect (PreconditionFailed otherwise) and
	// returns the stored row.
	Update(ctx context.Context, tx port.Tx, t domain.SwarmTemplate, expect domain.Version) (domain.SwarmTemplate, error)
	// Delete removes the template when its version is expect.
	Delete(ctx context.Context, tx port.Tx, id string, expect domain.Version) error
	// OpenWorkOrders counts the work orders that use the template and are not done, failed or cancelled.
	OpenWorkOrders(ctx context.Context, tx port.Tx, id string) (int, error)
}

// Service is the swarm template use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// Result is a template with the replayed response of an idempotent call.
type Result struct {
	Template domain.SwarmTemplate
	Replay   *port.StoredResponse
}

func (s *Service) check(ctx context.Context, c acting.Caller, action string, perm authz.Permission) error {
	if !authz.Authorize(c.Principal, perm, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return s.Denied(ctx, c, action, "", perm)
	}
	return nil
}

// List implements ListSwarmTemplates.
func (s *Service) List(ctx context.Context, c acting.Caller, p acting.Page) ([]domain.SwarmTemplate, string, error) {
	if err := s.check(ctx, c, "ListSwarmTemplates", authz.SwarmTemplatesRead); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.SwarmTemplate
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(t domain.SwarmTemplate) string { return string(t.Name) })
	return page, next, nil
}

// Get implements GetSwarmTemplate.
func (s *Service) Get(ctx context.Context, c acting.Caller, id string) (domain.SwarmTemplate, error) {
	if err := s.check(ctx, c, "GetSwarmTemplate", authz.SwarmTemplatesRead); err != nil {
		return domain.SwarmTemplate{}, err
	}
	var out domain.SwarmTemplate
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		out, err = s.Repo.Get(ctx, tx, id, false)
		return err
	})
	return out, err
}

// Create implements CreateSwarmTemplate. A name already in use is a Conflict.
func (s *Service) Create(ctx context.Context, c acting.Caller, in domain.NewSwarmTemplateParams, idem *acting.Idem) (Result, error) {
	if err := s.check(ctx, c, "CreateSwarmTemplate", authz.AdminSwarmTemplates); err != nil {
		return Result{}, err
	}
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		id, err := s.IDs.NewID(domain.PrefixSwarmTemplate)
		if err != nil {
			return err
		}
		t, err := domain.NewSwarmTemplate(id.String(), in, s.Now())
		if err != nil {
			return err
		}
		if err := s.Repo.Insert(ctx, tx, t); err != nil {
			return err
		}
		if res.Template, err = s.Repo.Get(ctx, tx, t.ID, false); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("CreateSwarmTemplate", kind, t.ID, "", nil, res.Template.Audit())); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.Template)
	})
	return res, err
}

// Update implements UpdateSwarmTemplate (If-Match required). A body that changes nothing is a
// ValidationError.
func (s *Service) Update(ctx context.Context, c acting.Caller, id string, expect domain.Version, patch domain.SwarmTemplatePatch) (domain.SwarmTemplate, error) {
	if err := s.check(ctx, c, "UpdateSwarmTemplate", authz.AdminSwarmTemplates); err != nil {
		return domain.SwarmTemplate{}, err
	}
	if patch.Empty() {
		return domain.SwarmTemplate{}, domain.Validation(domain.FieldError{Field: "body", Reason: "names no field to change"})
	}
	var out domain.SwarmTemplate
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		next, err := cur.Apply(patch, s.Now())
		if err != nil {
			return err
		}
		if out, err = s.Repo.Update(ctx, tx, next, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("UpdateSwarmTemplate", kind, id, "", cur.Audit(), out.Audit()))
	})
	return out, err
}

// Enable implements EnableSwarmTemplate.
func (s *Service) Enable(ctx context.Context, c acting.Caller, id string, idem *acting.Idem) (Result, error) {
	return s.setEnabled(ctx, c, "EnableSwarmTemplate", id, true, idem)
}

// Disable implements DisableSwarmTemplate. A disabled template refuses new work orders; the work
// orders and teams that already use it are not changed.
func (s *Service) Disable(ctx context.Context, c acting.Caller, id string, idem *acting.Idem) (Result, error) {
	return s.setEnabled(ctx, c, "DisableSwarmTemplate", id, false, idem)
}

func (s *Service) setEnabled(ctx context.Context, c acting.Caller, action, id string, on bool, idem *acting.Idem) (Result, error) {
	if err := s.check(ctx, c, action, authz.AdminSwarmTemplates); err != nil {
		return Result{}, err
	}
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		cur, err := s.Repo.Get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if cur.Enabled == on {
			res.Template = cur
			if err := s.Audit.Write(ctx, tx, c.Record(action, kind, id, "", cur.Audit(), cur.Audit())); err != nil {
				return err
			}
			return idem.Complete(ctx, s.Idem, tx, res.Template)
		}
		if res.Template, err = s.Repo.Update(ctx, tx, cur.WithEnabled(on, s.Now()), cur.Version); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record(action, kind, id, "", cur.Audit(), res.Template.Audit())); err != nil {
			return err
		}
		return idem.Complete(ctx, s.Idem, tx, res.Template)
	})
	return res, err
}

// Delete implements DeleteSwarmTemplate (If-Match required). A template an open work order uses is a
// Conflict (template_in_use); one a closed work order still names is a Conflict too (the work order
// keeps the reference), and disabling is the way to retire it.
func (s *Service) Delete(ctx context.Context, c acting.Caller, id string, expect domain.Version) error {
	if err := s.check(ctx, c, "DeleteSwarmTemplate", authz.AdminSwarmTemplates); err != nil {
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
		open, err := s.Repo.OpenWorkOrders(ctx, tx, id)
		if err != nil {
			return err
		}
		if open > 0 {
			return domain.Conflict("template_in_use", "an open work order uses swarm template "+string(cur.Name)+"; disable it instead",
				map[string]any{"openWorkOrders": open})
		}
		if err := s.Repo.Delete(ctx, tx, id, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteSwarmTemplate", kind, id, "", cur.Audit(), nil))
	})
}

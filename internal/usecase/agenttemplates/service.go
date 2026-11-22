// Package agenttemplates implements ListAgentTemplates, ListAdminAgentTemplates, PutAgentTemplate and
// DeleteAgentTemplate (API contract §4.4 and §4.11, D54; M2-A4). Disabling a template or removing a
// digest changes no existing agent: only new agents and pools are refused, and the reconciler flags
// the existing ones TemplateRetired. Contract §7 has no template event (CD-7).
package agenttemplates

import (
	"context"
	"slices"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kind = "agent_templates"

// Repository is the agent_templates store.
type Repository interface {
	// List returns templates with name greater than after, in name order; enabledOnly hides disabled ones.
	List(ctx context.Context, tx port.Tx, enabledOnly bool, capability, after string, limit int) ([]domain.AgentTemplate, error)
	// Get returns one template by name, forUpdate locking it; NotFound when absent.
	Get(ctx context.Context, tx port.Tx, name string, forUpdate bool) (domain.AgentTemplate, error)
	Insert(ctx context.Context, tx port.Tx, t domain.AgentTemplate) error
	// Update stores the template (by id); PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, t domain.AgentTemplate, expect domain.Version) (domain.AgentTemplate, error)
	// Delete removes the template; one an agent or pool uses is a Conflict (dependents_exist).
	Delete(ctx context.Context, tx port.Tx, name string, expect domain.Version) error
	// ReferencedBySwarm reports whether a swarm template names the template as its collaboration agent.
	ReferencedBySwarm(ctx context.Context, tx port.Tx, id string) (bool, error)
}

// Service is the agent template use case set.
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

func audit(t domain.AgentTemplate) map[string]any {
	return map[string]any{"id": t.ID, "name": t.Name, "description": t.Description, "harness": t.Harness,
		"defaultResources": t.DefaultResources, "allowedDigests": t.AllowedDigests, "currentDigest": t.CurrentDigest, "enabled": t.Enabled, "capabilities": t.Capabilities}
}

func (s *Service) list(ctx context.Context, c acting.Caller, enabledOnly bool, capability string, p acting.Page) ([]domain.AgentTemplate, string, error) {
	p = p.Normalized()
	var rows []domain.AgentTemplate
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, enabledOnly, capability, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(t domain.AgentTemplate) string { return t.Name })
	return page, next, nil
}

// ListEnabled implements ListAgentTemplates: enabled templates with their allowed digests
// (models:read).
func (s *Service) ListEnabled(ctx context.Context, c acting.Caller, capability string, p acting.Page) ([]domain.AgentTemplate, string, error) {
	if err := s.check(ctx, c, "ListAgentTemplates", authz.ModelsRead, authz.ScopeAny); err != nil {
		return nil, "", err
	}
	if err := domain.ValidateCapabilityFilter(capability); err != nil {
		return nil, "", err
	}
	return s.list(ctx, c, true, capability, p)
}

// ListAll implements ListAdminAgentTemplates (admin:agent-templates).
func (s *Service) ListAll(ctx context.Context, c acting.Caller, capability string, p acting.Page) ([]domain.AgentTemplate, string, error) {
	if err := s.check(ctx, c, "ListAdminAgentTemplates", authz.AdminAgentTemplates, authz.ScopeService); err != nil {
		return nil, "", err
	}
	if err := domain.ValidateCapabilityFilter(capability); err != nil {
		return nil, "", err
	}
	return s.list(ctx, c, false, capability, p)
}

// Put implements PutAgentTemplate: an upsert by name. A change to an existing template may carry
// If-Match (expect). Existing agents are not touched.
func (s *Service) Put(ctx context.Context, c acting.Caller, in domain.PutAgentTemplateParams, expect *domain.Version) (domain.AgentTemplate, error) {
	if err := s.check(ctx, c, "PutAgentTemplate", authz.AdminAgentTemplates, authz.ScopeService); err != nil {
		return domain.AgentTemplate{}, err
	}
	t, err := domain.NewAgentTemplate(in)
	if err != nil {
		return domain.AgentTemplate{}, err
	}
	var out domain.AgentTemplate
	err = s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, t.Name, true)
		switch {
		case domain.KindOf(err) == domain.KindNotFound:
			if expect != nil {
				return domain.PreconditionFailed()
			}
			id, err := s.IDs.NewID(domain.PrefixAgentTemplate)
			if err != nil {
				return err
			}
			t.ID = id.String()
			if err := s.Repo.Insert(ctx, tx, t); err != nil {
				return err
			}
			if out, err = s.Repo.Get(ctx, tx, t.Name, false); err != nil {
				return err
			}
			return s.Audit.Write(ctx, tx, c.Record("PutAgentTemplate", kind, out.ID, "", nil, audit(out)))
		case err != nil:
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		t.ID = cur.ID
		if cur.Enabled && !t.Enabled || slices.Contains(cur.Capabilities, domain.CapabilityPaperclip) && !slices.Contains(t.Capabilities, domain.CapabilityPaperclip) {
			if err := s.notReferenced(ctx, tx, cur.ID); err != nil {
				return err
			}
		}
		if out, err = s.Repo.Update(ctx, tx, t, cur.Version); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("PutAgentTemplate", kind, out.ID, "", audit(cur), audit(out)))
	})
	return out, err
}

// notReferenced is the 409 for a template a swarm template uses as its collaboration agent (RA-22).
func (s *Service) notReferenced(ctx context.Context, tx port.Tx, id string) error {
	used, err := s.Repo.ReferencedBySwarm(ctx, tx, id)
	if err != nil {
		return err
	}
	if used {
		return domain.Conflict(domain.ReasonReferencedBySwarm, "a swarm template names this agent template as its collaboration agent", nil)
	}
	return nil
}

// Delete implements DeleteAgentTemplate (If-Match required). A template an agent or pool uses is a
// Conflict: disable it instead.
func (s *Service) Delete(ctx context.Context, c acting.Caller, name string, expect domain.Version) error {
	if err := s.check(ctx, c, "DeleteAgentTemplate", authz.AdminAgentTemplates, authz.ScopeService); err != nil {
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
		if err := s.notReferenced(ctx, tx, cur.ID); err != nil {
			return err
		}
		if err := s.Repo.Delete(ctx, tx, name, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteAgentTemplate", kind, cur.ID, "", audit(cur), nil))
	})
}

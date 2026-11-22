// Package config implements the configuration value operations of API contract §4.7: the team's
// namespace values (ListNamespaceConfigValues, PutNamespaceConfigValue, DeleteNamespaceConfigValue)
// and each agent's values (ListAgentConfigValues, PutAgentConfigValue, DeleteAgentConfigValue)
// (M2-D1, M2-D2, CFG-7). Values are never secret; secrets go through the secrets operations.
// Contract §7 has no configuration event, so a change writes the row and an audit record only
// (CD-7); the reconciler picks the change up from the change feed and restarts the agents that use it.
package config

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const (
	kindNamespace = "namespace_config_values"
	kindAgent     = "agent_config_values"
)

// Repository is the namespace_config_values and agent_config_values store.
type Repository interface {
	// ListNamespace returns the team's values with name greater than after, in name order, each with
	// the agents that link to it.
	ListNamespace(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.NamespaceConfigValue, error)
	// GetNamespace returns one value with its linking agents, forUpdate locking it; NotFound when absent.
	GetNamespace(ctx context.Context, tx port.Tx, teamID string, name domain.ConfigName, forUpdate bool) (domain.NamespaceConfigValue, error)
	InsertNamespace(ctx context.Context, tx port.Tx, v domain.NamespaceConfigValue) error
	// UpdateNamespace stores the text; PreconditionFailed when the version is not expect.
	UpdateNamespace(ctx context.Context, tx port.Tx, v domain.NamespaceConfigValue, expect domain.Version) error
	// DeleteNamespace removes the value; PreconditionFailed when the version is not expect.
	DeleteNamespace(ctx context.Context, tx port.Tx, teamID string, name domain.ConfigName, expect domain.Version) error

	// AgentExists reports whether the team has a live agent with this id.
	AgentExists(ctx context.Context, tx port.Tx, teamID, agentID string) (bool, error)
	// ListAgent returns the agent's values (resolved) with name greater than after, in name order.
	ListAgent(ctx context.Context, tx port.Tx, teamID, agentID, after string, limit int) ([]domain.AgentConfigValue, error)
	// GetAgent returns one resolved value, forUpdate locking it; NotFound when absent.
	GetAgent(ctx context.Context, tx port.Tx, teamID, agentID string, name domain.ConfigName, forUpdate bool) (domain.AgentConfigValue, error)
	InsertAgent(ctx context.Context, tx port.Tx, v domain.AgentConfigValue) error
	// UpdateAgent stores source and literal text; PreconditionFailed when the version is not expect.
	UpdateAgent(ctx context.Context, tx port.Tx, v domain.AgentConfigValue, expect domain.Version) error
	DeleteAgent(ctx context.Context, tx port.Tx, teamID, agentID string, name domain.ConfigName, expect domain.Version) error
}

// Service is the configuration value use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// check re-applies the route's authorization in the use case: team scope, the caller's role on the
// team and the permission. An unbound caller learns nothing (NotFound).
func (s *Service) check(ctx context.Context, c acting.Caller, action, teamID string, perm authz.Permission) error {
	switch authz.Authorize(c.Principal, perm, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}).Outcome {
	case authz.Allow:
		return nil
	case authz.NotFound:
		return domain.NotFound("team")
	}
	return s.Denied(ctx, c, action, teamID, perm)
}

func auditNS(v domain.NamespaceConfigValue) map[string]any {
	return map[string]any{"name": v.Name, "value": v.Value}
}

func auditAgent(v domain.AgentConfigValue) map[string]any {
	m := map[string]any{"agentId": v.AgentID, "name": v.Name, "source": v.Source}
	if v.Value != nil {
		m["value"] = *v.Value
	}
	return m
}

// ListNamespace implements ListNamespaceConfigValues.
func (s *Service) ListNamespace(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.NamespaceConfigValue, string, error) {
	if err := s.check(ctx, c, "ListNamespaceConfigValues", teamID, authz.ConfigRead); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.NamespaceConfigValue
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.ListNamespace(ctx, tx, teamID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(v domain.NamespaceConfigValue) string { return string(v.Name) })
	return page, next, nil
}

// PutNamespace implements PutNamespaceConfigValue: creates or updates. A change to an existing value
// may carry If-Match (expect).
func (s *Service) PutNamespace(ctx context.Context, c acting.Caller, teamID string, name domain.ConfigName, value string, expect *domain.Version) (domain.NamespaceConfigValue, error) {
	if err := s.check(ctx, c, "PutNamespaceConfigValue", teamID, authz.ConfigWrite); err != nil {
		return domain.NamespaceConfigValue{}, err
	}
	if _, err := domain.ParseConfigName("name", string(name)); err != nil {
		return domain.NamespaceConfigValue{}, err
	}
	if err := domain.ValidateConfigValue("value", name, value); err != nil {
		return domain.NamespaceConfigValue{}, err
	}
	var out domain.NamespaceConfigValue
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.GetNamespace(ctx, tx, teamID, name, true)
		switch {
		case domain.KindOf(err) == domain.KindNotFound:
			if expect != nil {
				return domain.PreconditionFailed()
			}
			id, err := s.IDs.NewID(domain.PrefixNamespaceConfig)
			if err != nil {
				return err
			}
			if err := s.Repo.InsertNamespace(ctx, tx, domain.NamespaceConfigValue{ID: id.String(), TeamID: teamID, Name: name, Value: value}); err != nil {
				return err
			}
			if out, err = s.Repo.GetNamespace(ctx, tx, teamID, name, false); err != nil {
				return err
			}
			return s.Audit.Write(ctx, tx, c.Record("PutNamespaceConfigValue", kindNamespace, out.ID, teamID, nil, auditNS(out)))
		case err != nil:
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		next := cur
		next.Value = value
		if err := s.Repo.UpdateNamespace(ctx, tx, next, cur.Version); err != nil {
			return err
		}
		if out, err = s.Repo.GetNamespace(ctx, tx, teamID, name, false); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("PutNamespaceConfigValue", kindNamespace, out.ID, teamID, auditNS(cur), auditNS(out)))
	})
	return out, err
}

// DeleteNamespace implements DeleteNamespaceConfigValue (If-Match required). A value agents link to
// is a Conflict that lists them.
func (s *Service) DeleteNamespace(ctx context.Context, c acting.Caller, teamID string, name domain.ConfigName, expect domain.Version) error {
	if err := s.check(ctx, c, "DeleteNamespaceConfigValue", teamID, authz.ConfigWrite); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.GetNamespace(ctx, tx, teamID, name, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if len(cur.UsedBy) > 0 {
			return domain.Conflict("dependents_exist", "namespace value "+string(name)+" is used by agents; switch them to their own value first",
				map[string]any{"agents": cur.UsedBy})
		}
		if err := s.Repo.DeleteNamespace(ctx, tx, teamID, name, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteNamespaceConfigValue", kindNamespace, cur.ID, teamID, auditNS(cur), nil))
	})
}

func (s *Service) requireAgent(ctx context.Context, tx port.Tx, teamID, agentID string) error {
	ok, err := s.Repo.AgentExists(ctx, tx, teamID, agentID)
	if err != nil {
		return err
	}
	if !ok {
		return domain.NotFound("agent")
	}
	return nil
}

// ListAgent implements ListAgentConfigValues: the agent's own entries, resolved, with their source.
func (s *Service) ListAgent(ctx context.Context, c acting.Caller, teamID, agentID string, p acting.Page) ([]domain.AgentConfigValue, string, error) {
	if err := s.check(ctx, c, "ListAgentConfigValues", teamID, authz.ConfigRead); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.AgentConfigValue
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.requireAgent(ctx, tx, teamID, agentID); err != nil {
			return err
		}
		var err error
		rows, err = s.Repo.ListAgent(ctx, tx, teamID, agentID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(v domain.AgentConfigValue) string { return string(v.Name) })
	return page, next, nil
}

// AgentValueInput is the body of PutAgentConfigValue: a literal needs a value, a link must not have one.
type AgentValueInput struct {
	Source domain.ConfigSource
	Value  *string
}

func (in AgentValueInput) validate(name domain.ConfigName) error {
	switch in.Source {
	case domain.ConfigSourceLiteral:
		if in.Value == nil {
			return domain.Validation(domain.FieldError{Field: "value", Reason: "is required when source is literal"})
		}
		return domain.ValidateConfigValue("value", name, *in.Value)
	case domain.ConfigSourceNamespace:
		if in.Value != nil {
			return domain.Validation(domain.FieldError{Field: "value", Reason: "must be absent when source is namespace"})
		}
		return nil
	}
	return domain.Validation(domain.FieldError{Field: "source", Reason: "must be literal or namespace"})
}

// PutAgent implements PutAgentConfigValue: creates or updates the agent's entry. A link needs the
// namespace value of the same name to exist. A change to an existing entry may carry If-Match.
func (s *Service) PutAgent(ctx context.Context, c acting.Caller, teamID, agentID string, name domain.ConfigName, in AgentValueInput, expect *domain.Version) (domain.AgentConfigValue, error) {
	if err := s.check(ctx, c, "PutAgentConfigValue", teamID, authz.ConfigWrite); err != nil {
		return domain.AgentConfigValue{}, err
	}
	if _, err := domain.ParseConfigName("name", string(name)); err != nil {
		return domain.AgentConfigValue{}, err
	}
	if err := in.validate(name); err != nil {
		return domain.AgentConfigValue{}, err
	}
	var out domain.AgentConfigValue
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.requireAgent(ctx, tx, teamID, agentID); err != nil {
			return err
		}
		if in.Source == domain.ConfigSourceNamespace {
			if _, err := s.Repo.GetNamespace(ctx, tx, teamID, name, false); err != nil {
				if domain.KindOf(err) == domain.KindNotFound {
					return domain.Validation(domain.FieldError{Field: "source", Reason: "there is no namespace value named " + string(name) + " to link to"})
				}
				return err
			}
		}
		next := domain.AgentConfigValue{TeamID: teamID, AgentID: agentID, Name: name, Source: in.Source, Value: in.Value}
		cur, err := s.Repo.GetAgent(ctx, tx, teamID, agentID, name, true)
		switch {
		case domain.KindOf(err) == domain.KindNotFound:
			if expect != nil {
				return domain.PreconditionFailed()
			}
			id, err := s.IDs.NewID(domain.PrefixAgentConfig)
			if err != nil {
				return err
			}
			next.ID = id.String()
			if err := s.Repo.InsertAgent(ctx, tx, next); err != nil {
				return err
			}
			if out, err = s.Repo.GetAgent(ctx, tx, teamID, agentID, name, false); err != nil {
				return err
			}
			return s.Audit.Write(ctx, tx, c.Record("PutAgentConfigValue", kindAgent, out.ID, teamID, nil, auditAgent(out)))
		case err != nil:
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		next.ID = cur.ID
		if err := s.Repo.UpdateAgent(ctx, tx, next, cur.Version); err != nil {
			return err
		}
		if out, err = s.Repo.GetAgent(ctx, tx, teamID, agentID, name, false); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("PutAgentConfigValue", kindAgent, out.ID, teamID, auditAgent(cur), auditAgent(out)))
	})
	return out, err
}

// DeleteAgent implements DeleteAgentConfigValue (If-Match required).
func (s *Service) DeleteAgent(ctx context.Context, c acting.Caller, teamID, agentID string, name domain.ConfigName, expect domain.Version) error {
	if err := s.check(ctx, c, "DeleteAgentConfigValue", teamID, authz.ConfigWrite); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.requireAgent(ctx, tx, teamID, agentID); err != nil {
			return err
		}
		cur, err := s.Repo.GetAgent(ctx, tx, teamID, agentID, name, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if err := s.Repo.DeleteAgent(ctx, tx, teamID, agentID, name, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteAgentConfigValue", kindAgent, cur.ID, teamID, auditAgent(cur), nil))
	})
}

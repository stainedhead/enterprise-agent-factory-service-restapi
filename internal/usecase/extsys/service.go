// Package extsys implements the team and agent levels of external systems (API contract §4.8,
// EXT-2..4; M2-E): the systems a team sees (service catalog plus the ones it declared) with the
// team's state, switching systems off for the team or one agent, and declaring, updating and
// deleting a team's own systems. A lower level can only narrow the one above: a team cannot switch
// on what the service level has off, and an agent cannot switch on what its team has off. Contract
// §7 has no external system event, so a change writes the row and an audit record only (CD-7); the
// reconciler renders the effective endpoints into each agent's egress policy from the change feed.
package extsys

import (
	"context"
	"errors"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const (
	kindSystem = "external_systems"
	kindState  = "external_system_optouts"
)

// Repository is the external_systems and external_system_optouts store as one team or agent sees it.
type Repository interface {
	// TeamMayDeclare reports teams.may_add_external_systems; NotFound when the team is not visible.
	TeamMayDeclare(ctx context.Context, tx port.Tx, teamID string) (bool, error)
	// ListTeam returns the systems visible to the team (service catalog and its own) with id greater
	// than after, in id order, with the team-level state.
	ListTeam(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.ExternalSystemAccess, error)
	// GetTeam returns one system visible to the team, with the team-level state; NotFound when it
	// belongs to the service catalog or the team no more than that. forUpdate locks the system row.
	GetTeam(ctx context.Context, tx port.Tx, teamID, systemID string, forUpdate bool) (domain.ExternalSystemAccess, error)
	// CatalogSlugExists reports whether the service catalog has an entry with this slug.
	CatalogSlugExists(ctx context.Context, tx port.Tx, slug string) (bool, error)
	// Insert stores a team-declared system; a duplicate slug is a Conflict.
	Insert(ctx context.Context, tx port.Tx, s domain.ExternalSystem) error
	// Update stores a team-declared system; PreconditionFailed when the version is not expect.
	Update(ctx context.Context, tx port.Tx, s domain.ExternalSystem, expect domain.Version) error
	// Delete removes a team-declared system (and its switches); PreconditionFailed when the version is not expect.
	Delete(ctx context.Context, tx port.Tx, teamID, systemID string, expect domain.Version) error
	// SetTeamOff switches the system off (true) or on (false) for the whole team; optOutID names the
	// row of a new switch. Both directions are idempotent.
	SetTeamOff(ctx context.Context, tx port.Tx, teamID, systemID, optOutID string, off bool) error

	// AgentExists reports whether the team has a live agent with this id.
	AgentExists(ctx context.Context, tx port.Tx, teamID, agentID string) (bool, error)
	// ListAgent returns the effective systems of the agent with id greater than after, in id order.
	ListAgent(ctx context.Context, tx port.Tx, teamID, agentID, after string, limit int) ([]domain.ExternalSystemAccess, error)
	// GetAgent returns one effective system of the agent; NotFound when the agent's team cannot see it.
	GetAgent(ctx context.Context, tx port.Tx, teamID, agentID, systemID string) (domain.ExternalSystemAccess, error)
	// SetAgentOff switches the system off (true) or on (false) for one agent; optOutID names the row
	// of a new switch. Both directions are idempotent.
	SetAgentOff(ctx context.Context, tx port.Tx, teamID, agentID, systemID, optOutID string, off bool) error
}

// Service is the team and agent external system use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// Created is a system with the replayed response of an idempotent declare.
type Created struct {
	System domain.ExternalSystemAccess
	Replay *port.StoredResponse
}

// check re-applies the route's authorization: team scope, the role on the team, the permission.
func (s *Service) check(ctx context.Context, c acting.Caller, action, teamID string, perm authz.Permission) error {
	switch authz.Authorize(c.Principal, perm, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}).Outcome {
	case authz.Allow:
		return nil
	case authz.NotFound:
		return domain.NotFound("team")
	}
	return s.Denied(ctx, c, action, teamID, perm)
}

func auditSystem(x domain.ExternalSystem) map[string]any {
	return map[string]any{"id": x.ID, "slug": x.Slug, "name": x.Name, "category": x.Category, "description": x.Description,
		"docsUrl": x.DocsURL, "owner": x.Owner, "purpose": x.Purpose, "endpoints": x.Endpoints}
}

func auditState(agentID string, a domain.ExternalSystemAccess, agentLevel bool) map[string]any {
	m := map[string]any{"systemId": a.ID, "slug": a.Slug, "enabled": a.On()}
	if agentLevel {
		m["agentId"] = agentID
	}
	return m
}

// ListTeam implements ListTeamExternalSystems.
func (s *Service) ListTeam(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.ExternalSystemAccess, string, error) {
	if err := s.check(ctx, c, "ListTeamExternalSystems", teamID, authz.ExternalSystemsRead); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.ExternalSystemAccess
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.ListTeam(ctx, tx, teamID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(a domain.ExternalSystemAccess) string { return a.ID })
	return page, next, nil
}

// SetTeamState implements SetTeamExternalSystemState. A team cannot switch on a system its level
// above has off (a service-level catalog entry that is disabled). If-Match, when sent, is compared
// with the system's version.
func (s *Service) SetTeamState(ctx context.Context, c acting.Caller, teamID, systemID string, enabled bool, expect *domain.Version) (domain.ExternalSystemAccess, error) {
	if err := s.check(ctx, c, "SetTeamExternalSystemState", teamID, authz.ExternalSystemsWrite); err != nil {
		return domain.ExternalSystemAccess{}, err
	}
	var out domain.ExternalSystemAccess
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.GetTeam(ctx, tx, teamID, systemID, false)
		if err != nil {
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		if enabled && cur.DisabledAt == domain.ExtSystemDisabledAtService {
			return domain.Conflict("disabled_above", "the system is switched off at the service level; a team cannot switch it on",
				map[string]any{"disabledAt": string(cur.DisabledAt)})
		}
		oid, err := s.IDs.NewID(domain.PrefixExtSystemOptout)
		if err != nil {
			return err
		}
		if err := s.Repo.SetTeamOff(ctx, tx, teamID, systemID, oid.String(), !enabled); err != nil {
			return err
		}
		if out, err = s.Repo.GetTeam(ctx, tx, teamID, systemID, false); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("SetTeamExternalSystemState", kindState, systemID, teamID, auditState("", cur, false), auditState("", out, false)))
	})
	return out, err
}

// Declare implements DeclareTeamExternalSystem. It needs the team's mayAddExternalSystems flag, set
// by a service admin: otherwise AccessDenied, whoever asks. A declared system cannot reuse a
// catalog slug.
func (s *Service) Declare(ctx context.Context, c acting.Caller, teamID string, in domain.DeclareSystemParams, idem *acting.Idem) (Created, error) {
	var res Created
	if err := s.check(ctx, c, "DeclareTeamExternalSystem", teamID, authz.ExternalSystemsWrite); err != nil {
		return res, err
	}
	sys, err := domain.NewTeamSystem(teamID, in)
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
		may, err := s.Repo.TeamMayDeclare(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if !may {
			return errNotAllowed{}
		}
		if taken, err := s.Repo.CatalogSlugExists(ctx, tx, sys.Slug); err != nil {
			return err
		} else if taken {
			return domain.Conflict("duplicate", sys.Slug+" is already in the service catalog; use that entry", nil)
		}
		id, err := s.IDs.NewID(domain.PrefixExtSystem)
		if err != nil {
			return err
		}
		sys.ID = id.String()
		if err := s.Repo.Insert(ctx, tx, sys); err != nil {
			return err
		}
		stored, err := s.Repo.GetTeam(ctx, tx, teamID, sys.ID, false)
		if err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("DeclareTeamExternalSystem", kindSystem, stored.ID, teamID, nil, auditSystem(stored.ExternalSystem))); err != nil {
			return err
		}
		res.System = stored
		return idem.Complete(ctx, s.Idem, tx, stored)
	})
	if errors.As(err, &errNotAllowed{}) {
		return res, s.Denied(ctx, c, "DeclareTeamExternalSystem", teamID, authz.ExternalSystemsWrite)
	}
	return res, err
}

type errNotAllowed struct{}

func (errNotAllowed) Error() string { return "team may not declare external systems" }

// teamDeclared loads a system the team declared; a catalog system is a Conflict, not a NotFound,
// because the team can see it but not change it.
//
// The catalog check reads without a lock: row-level security hides a catalog row from a team's
// SELECT ... FOR UPDATE, which would turn the Conflict into a NotFound.
func (s *Service) teamDeclared(ctx context.Context, tx port.Tx, teamID, systemID string) (domain.ExternalSystemAccess, error) {
	cur, err := s.Repo.GetTeam(ctx, tx, teamID, systemID, false)
	if err != nil {
		return cur, err
	}
	if !cur.TeamDeclared() {
		return cur, domain.Conflict("not_team_declared", "the system is in the service catalog; only a team-declared system can be changed here", nil)
	}
	return s.Repo.GetTeam(ctx, tx, teamID, systemID, true)
}

// Update implements UpdateTeamExternalSystem (team-declared only; If-Match required).
func (s *Service) Update(ctx context.Context, c acting.Caller, teamID, systemID string, u domain.SystemUpdate, expect domain.Version) (domain.ExternalSystemAccess, error) {
	if err := s.check(ctx, c, "UpdateTeamExternalSystem", teamID, authz.ExternalSystemsWrite); err != nil {
		return domain.ExternalSystemAccess{}, err
	}
	// The service-level switch is not the team's to flip: a team switches a system off through the state operation.
	u.Enabled = nil
	var out domain.ExternalSystemAccess
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.teamDeclared(ctx, tx, teamID, systemID)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		next, err := cur.ExternalSystem.Apply(u)
		if err != nil {
			return err
		}
		if err := s.Repo.Update(ctx, tx, next, expect); err != nil {
			return err
		}
		if out, err = s.Repo.GetTeam(ctx, tx, teamID, systemID, false); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("UpdateTeamExternalSystem", kindSystem, systemID, teamID, auditSystem(cur.ExternalSystem), auditSystem(out.ExternalSystem)))
	})
	return out, err
}

// Delete implements DeleteTeamExternalSystem (team-declared only; If-Match required).
func (s *Service) Delete(ctx context.Context, c acting.Caller, teamID, systemID string, expect domain.Version) error {
	if err := s.check(ctx, c, "DeleteTeamExternalSystem", teamID, authz.ExternalSystemsWrite); err != nil {
		return err
	}
	return s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.teamDeclared(ctx, tx, teamID, systemID)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if err := s.Repo.Delete(ctx, tx, teamID, systemID, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("DeleteTeamExternalSystem", kindSystem, systemID, teamID, auditSystem(cur.ExternalSystem), nil))
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

// ListAgent implements ListAgentExternalSystems: the effective systems of the agent, with disabledAt.
func (s *Service) ListAgent(ctx context.Context, c acting.Caller, teamID, agentID string, p acting.Page) ([]domain.ExternalSystemAccess, string, error) {
	if err := s.check(ctx, c, "ListAgentExternalSystems", teamID, authz.ExternalSystemsRead); err != nil {
		return nil, "", err
	}
	p = p.Normalized()
	var rows []domain.ExternalSystemAccess
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
	page, next := acting.Slice(rows, p.Limit, func(a domain.ExternalSystemAccess) string { return a.ID })
	return page, next, nil
}

// SetAgentState implements SetAgentExternalSystemState. An agent cannot switch on a system its team
// or the service has off. If-Match, when sent, is compared with the system's version.
func (s *Service) SetAgentState(ctx context.Context, c acting.Caller, teamID, agentID, systemID string, enabled bool, expect *domain.Version) (domain.ExternalSystemAccess, error) {
	if err := s.check(ctx, c, "SetAgentExternalSystemState", teamID, authz.ExternalSystemsWrite); err != nil {
		return domain.ExternalSystemAccess{}, err
	}
	var out domain.ExternalSystemAccess
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if err := s.requireAgent(ctx, tx, teamID, agentID); err != nil {
			return err
		}
		cur, err := s.Repo.GetAgent(ctx, tx, teamID, agentID, systemID)
		if err != nil {
			return err
		}
		if expect != nil {
			if err := domain.CheckVersion(cur.Version, *expect); err != nil {
				return err
			}
		}
		if enabled && !cur.TeamOn() {
			return domain.Conflict("disabled_above", "the system is switched off at the "+string(cur.DisabledAt)+" level; an agent cannot switch it on",
				map[string]any{"disabledAt": string(cur.DisabledAt)})
		}
		oid, err := s.IDs.NewID(domain.PrefixExtSystemOptout)
		if err != nil {
			return err
		}
		if err := s.Repo.SetAgentOff(ctx, tx, teamID, agentID, systemID, oid.String(), !enabled); err != nil {
			return err
		}
		if out, err = s.Repo.GetAgent(ctx, tx, teamID, agentID, systemID); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("SetAgentExternalSystemState", kindState, systemID, teamID, auditState(agentID, cur, true), auditState(agentID, out, true)))
	})
	return out, err
}

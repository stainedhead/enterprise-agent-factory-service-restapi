// Package accounts serves the organization account mirror (M4-A2, TEAM-8): the accounts of the whole
// organization for service admins, and the ones a team's allowed-accounts filter lets its agents
// assume roles in for the team. The mirror itself is written by the reconciler (housekeeping/orgmirror).
package accounts

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repository reads the mirror.
type Repository interface {
	// ListOrg returns mirrored accounts with id greater than after, in id order.
	ListOrg(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.OrgAccount, error)
	// ListAllowed returns the active accounts the team's filter allows, with id greater than after.
	ListAllowed(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.OrgAccount, error)
	// TeamExists reports whether the team is visible.
	TeamExists(ctx context.Context, tx port.Tx, teamID string) (bool, error)
}

// Service is the account use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// ListOrg implements ListOrgAccounts (admin:accounts).
func (s *Service) ListOrg(ctx context.Context, c acting.Caller, p acting.Page) ([]domain.OrgAccount, string, error) {
	switch authz.Authorize(c.Principal, authz.AdminAccounts, authz.Resource{Scope: authz.ScopeService}).Outcome {
	case authz.Allow:
	default:
		return nil, "", s.Denied(ctx, c, "ListOrgAccounts", "", authz.AdminAccounts)
	}
	p = p.Normalized()
	var rows []domain.OrgAccount
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.ListOrg(ctx, tx, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(a domain.OrgAccount) string { return a.ID })
	return page, next, nil
}

// ListAllowed implements ListAllowedAccounts (identities:read on the team).
func (s *Service) ListAllowed(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.OrgAccount, string, error) {
	switch authz.Authorize(c.Principal, authz.IdentitiesRead, authz.Resource{Scope: authz.ScopeTeam, TeamID: teamID}).Outcome {
	case authz.Allow:
	case authz.NotFound:
		return nil, "", domain.NotFound("team")
	default:
		return nil, "", s.Denied(ctx, c, "ListAllowedAccounts", teamID, authz.IdentitiesRead)
	}
	p = p.Normalized()
	var rows []domain.OrgAccount
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		ok, err := s.Repo.TeamExists(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if !ok {
			return domain.NotFound("team")
		}
		rows, err = s.Repo.ListAllowed(ctx, tx, teamID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(a domain.OrgAccount) string { return a.ID })
	return page, next, nil
}

// Package secops implements ListSecurityOperators, AddSecurityOperator and RemoveSecurityOperator
// (API contract §4.3, M6-A1). A security operator is a service-level role binding (team_id NULL) of a
// user or an identity provider group. Only service admins manage them: the route requires
// admin:settings and the use case checks authz.CanGrant again, so a team-admin can never grant the
// role. Contract §7 has no event for them, so mutations write the row and an audit record but no
// outbox row (CD-7).
package secops

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kindBinding = "role_bindings"

// Operator is a security operator binding. It is a domain.Member with no team and the
// security_operator role.
type Operator = domain.Member

// RoleSecurityOperator is the role of every Operator.
const RoleSecurityOperator domain.TeamRole = domain.TeamRole(authz.RoleSecurityOperator)

// Repository is the store of service-level security operator bindings.
type Repository interface {
	// List returns the bindings with id greater than after, in id order.
	List(ctx context.Context, tx port.Tx, after string, limit int) ([]Operator, error)
	// Get returns one binding; NotFound when no security operator binding has the id. forUpdate locks it.
	Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (Operator, error)
	// ResolveUser returns the id of the user with this id or email.
	ResolveUser(ctx context.Context, tx port.Tx, userID, email string) (string, bool, error)
	// Insert stores a binding; a duplicate subject is a Conflict.
	Insert(ctx context.Context, tx port.Tx, m Operator) error
	// Delete removes the binding; PreconditionFailed when the version is not expect.
	Delete(ctx context.Context, tx port.Tx, id string, expect domain.Version) error
}

// Service is the security operator use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// Result is an operator with the replayed response of an idempotent add.
type Result struct {
	Operator Operator
	Replay   *port.StoredResponse
}

func audit(m Operator) map[string]any {
	return map[string]any{"role": m.Role, "subjectType": m.SubjectType, "userId": m.UserID,
		"groupName": m.GroupName, "identityProviderId": m.IdentityProviderID}
}

// guard refuses anyone but a service admin: the route's permission already did, this is the second line.
func (s *Service) guard(ctx context.Context, c acting.Caller, action string) error {
	if authz.CanGrant(c.Principal, authz.RoleSecurityOperator, "") {
		return nil
	}
	return s.Denied(ctx, c, action, "", authz.AdminSettings)
}

// List implements ListSecurityOperators.
func (s *Service) List(ctx context.Context, c acting.Caller, p acting.Page) ([]Operator, string, error) {
	if !authz.Authorize(c.Principal, authz.AdminSettings, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return nil, "", s.Denied(ctx, c, "ListSecurityOperators", "", authz.AdminSettings)
	}
	p = p.Normalized()
	var rows []Operator
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(m Operator) string { return m.ID })
	return page, next, nil
}

// Add implements AddSecurityOperator.
func (s *Service) Add(ctx context.Context, c acting.Caller, in domain.NewMemberParams, idem *acting.Idem) (Result, error) {
	if err := s.guard(ctx, c, "AddSecurityOperator"); err != nil {
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
		// The subject rules are those of a team member; the role is fixed, so a placeholder team role
		// stands in for it while the body is checked.
		check := in
		check.Role = domain.TeamRoleTeamViewer
		if err := check.Validate(); err != nil {
			return err
		}
		if in.SubjectType == domain.SubjectTypeUser {
			if in.IdentityProviderID != "" {
				return domain.Validation(domain.FieldError{Field: "identityProviderId", Reason: "is only for a group subject"})
			}
			id, ok, err := s.Repo.ResolveUser(ctx, tx, in.UserID, in.Email)
			if err != nil {
				return err
			}
			if !ok {
				return domain.Validation(domain.FieldError{Field: "userId", Reason: "no such user; they must sign in once first"})
			}
			in.UserID, in.Email = id, ""
		}
		id, err := s.IDs.NewID(domain.PrefixBinding)
		if err != nil {
			return err
		}
		m := Operator{ID: id.String(), Role: RoleSecurityOperator, SubjectType: in.SubjectType, UserID: in.UserID,
			GroupName: in.GroupName, IdentityProviderID: in.IdentityProviderID}
		if err := s.Repo.Insert(ctx, tx, m); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("AddSecurityOperator", kindBinding, m.ID, "", nil, audit(m))); err != nil {
			return err
		}
		if m, err = s.Repo.Get(ctx, tx, m.ID, false); err != nil {
			return err
		}
		res.Operator = m
		return idem.Complete(ctx, s.Idem, tx, m)
	})
	return res, err
}

// Remove implements RemoveSecurityOperator (If-Match required).
func (s *Service) Remove(ctx context.Context, c acting.Caller, id string, expect domain.Version) error {
	if err := s.guard(ctx, c, "RemoveSecurityOperator"); err != nil {
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
		if err := s.Repo.Delete(ctx, tx, id, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("RemoveSecurityOperator", kindBinding, id, "", audit(cur), nil))
	})
}

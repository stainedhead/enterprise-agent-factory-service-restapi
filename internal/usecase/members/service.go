// Package members implements ListMembers, AddMember, UpdateMember and RemoveMember (API contract
// §4.3). Team-admins grant team roles on their own team; service-level roles are never granted
// here (authz.CanGrant). Mutations write the row and an audit record; contract §7 has no member
// event type, so no outbox row is written (CD-7).
package members

import (
	"context"
	"errors"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const kindBinding = "role_bindings"

// Repository is the role binding store for team roles.
type Repository interface {
	// List returns the team's bindings with id greater than after, in id order.
	List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.Member, error)
	// Get returns one binding of the team; NotFound otherwise. forUpdate locks it.
	Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.Member, error)
	// ResolveUser returns the id of the user with this id or email.
	ResolveUser(ctx context.Context, tx port.Tx, userID, email string) (string, bool, error)
	// Insert stores a binding; a duplicate is a Conflict.
	Insert(ctx context.Context, tx port.Tx, m domain.Member) error
	// UpdateRole changes the role; PreconditionFailed when the version is not expect.
	UpdateRole(ctx context.Context, tx port.Tx, teamID, id string, role domain.TeamRole, expect domain.Version) (domain.Member, error)
	// Delete removes the binding; PreconditionFailed when the version is not expect.
	Delete(ctx context.Context, tx port.Tx, teamID, id string, expect domain.Version) error
}

// Service is the member use case set.
type Service struct {
	acting.Deps
	Repo Repository
}

// Result is a member with the replayed response of an idempotent add.
type Result struct {
	Member domain.Member
	Replay *port.StoredResponse
}

type errDenied struct{}

func (errDenied) Error() string { return "denied members:write" }

func (s *Service) finish(ctx context.Context, c acting.Caller, action, teamID string, err error) error {
	if errors.As(err, &errDenied{}) {
		return s.Denied(ctx, c, action, teamID, authz.MembersWrite)
	}
	return err
}

func audit(m domain.Member) map[string]any {
	return map[string]any{"role": m.Role, "subjectType": m.SubjectType, "userId": m.UserID,
		"groupName": m.GroupName, "identityProviderId": m.IdentityProviderID}
}

// List implements ListMembers.
func (s *Service) List(ctx context.Context, c acting.Caller, teamID string, p acting.Page) ([]domain.Member, string, error) {
	p = p.Normalized()
	var rows []domain.Member
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, teamID, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(m domain.Member) string { return m.ID })
	return page, next, nil
}

// Add implements AddMember.
func (s *Service) Add(ctx context.Context, c acting.Caller, teamID string, in domain.NewMemberParams, idem *acting.Idem) (Result, error) {
	var res Result
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		if err := in.Validate(); err != nil {
			return err
		}
		if !authz.CanGrant(c.Principal, authz.Role(in.Role), teamID) {
			return errDenied{}
		}
		if in.SubjectType == domain.SubjectTypeUser {
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
		m := domain.Member{ID: id.String(), TeamID: teamID, Role: in.Role, SubjectType: in.SubjectType, UserID: in.UserID,
			GroupName: in.GroupName, IdentityProviderID: in.IdentityProviderID}
		if err := s.Repo.Insert(ctx, tx, m); err != nil {
			return err
		}
		if err := s.Audit.Write(ctx, tx, c.Record("AddMember", kindBinding, m.ID, teamID, nil, audit(m))); err != nil {
			return err
		}
		if m, err = s.Repo.Get(ctx, tx, teamID, m.ID, false); err != nil {
			return err
		}
		res.Member = m
		return idem.Complete(ctx, s.Idem, tx, m)
	})
	return res, s.finish(ctx, c, "AddMember", teamID, err)
}

// Update implements UpdateMember (role only; If-Match required).
func (s *Service) Update(ctx context.Context, c acting.Caller, teamID, id string, role domain.TeamRole, expect domain.Version) (domain.Member, error) {
	var out domain.Member
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		if !role.Valid() {
			return domain.Validation(domain.FieldError{Field: "role", Reason: "must be team_admin or team_viewer"})
		}
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if !authz.CanGrant(c.Principal, authz.Role(role), teamID) || !authz.CanGrant(c.Principal, authz.Role(cur.Role), teamID) {
			return errDenied{}
		}
		if out, err = s.Repo.UpdateRole(ctx, tx, teamID, id, role, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("UpdateMember", kindBinding, id, teamID, audit(cur), audit(out)))
	})
	return out, s.finish(ctx, c, "UpdateMember", teamID, err)
}

// Remove implements RemoveMember (If-Match required).
func (s *Service) Remove(ctx context.Context, c acting.Caller, teamID, id string, expect domain.Version) error {
	err := s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		cur, err := s.Repo.Get(ctx, tx, teamID, id, true)
		if err != nil {
			return err
		}
		if err := domain.CheckVersion(cur.Version, expect); err != nil {
			return err
		}
		if !authz.CanGrant(c.Principal, authz.Role(cur.Role), teamID) {
			return errDenied{}
		}
		if err := s.Repo.Delete(ctx, tx, teamID, id, expect); err != nil {
			return err
		}
		return s.Audit.Write(ctx, tx, c.Record("RemoveMember", kindBinding, id, teamID, audit(cur), nil))
	})
	return s.finish(ctx, c, "RemoveMember", teamID, err)
}

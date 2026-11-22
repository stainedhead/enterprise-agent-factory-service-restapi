package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/members"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// MemberRepo implements members.Repository on afs.role_bindings.
type MemberRepo struct{}

// NewMemberRepo returns a MemberRepo.
func NewMemberRepo() *MemberRepo { return &MemberRepo{} }

var _ members.Repository = (*MemberRepo)(nil)

const memberSelect = `SELECT b.id, b.team_id, b.role, b.subject_type, coalesce(b.user_id, ''), coalesce(u.email, ''),
  coalesce(b.group_name, ''), coalesce(b.identity_provider_id, ''), b.version, b.created_at, b.created_by, b.updated_at
FROM afs.role_bindings b LEFT JOIN afs.users u ON u.id = b.user_id`

func scanMember(row pgx.Row) (domain.Member, error) {
	var m domain.Member
	var role, st string
	var v int64
	err := row.Scan(&m.ID, &m.TeamID, &role, &st, &m.UserID, &m.Email, &m.GroupName, &m.IdentityProviderID, &v,
		&m.CreatedAt, &m.CreatedBy, &m.UpdatedAt)
	m.Role, m.SubjectType, m.Version = domain.TeamRole(role), domain.SubjectType(st), domain.Version(v)
	return m, err
}

// List implements members.Repository.
func (*MemberRepo) List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.Member, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, memberSelect+` WHERE b.team_id=$1 AND b.id > $2 ORDER BY b.id LIMIT $3`, teamID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "member")
	}
	defer rows.Close()
	out := []domain.Member{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "member")
		}
		out = append(out, m)
	}
	return out, MapError(rows.Err(), OpRead, "member")
}

// Get implements members.Repository.
func (*MemberRepo) Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.Member, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.Member{}, err
	}
	q := memberSelect + ` WHERE b.team_id=$1 AND b.id=$2`
	if forUpdate {
		q += ` FOR UPDATE OF b`
	}
	m, err := scanMember(pt.QueryRow(ctx, q, teamID, id))
	return m, MapError(err, OpRead, "member")
}

// ResolveUser implements members.Repository.
func (*MemberRepo) ResolveUser(ctx context.Context, tx port.Tx, userID, email string) (string, bool, error) {
	return resolveUser(ctx, tx, userID, email)
}

// Insert implements members.Repository.
func (*MemberRepo) Insert(ctx context.Context, tx port.Tx, m domain.Member) error {
	return insertBinding(ctx, tx, m.ID, m.TeamID, domain.NewMemberParams{Role: m.Role, SubjectType: m.SubjectType,
		UserID: m.UserID, Email: m.Email, GroupName: m.GroupName, IdentityProviderID: m.IdentityProviderID})
}

// UpdateRole implements members.Repository.
func (r *MemberRepo) UpdateRole(ctx context.Context, tx port.Tx, teamID, id string, role domain.TeamRole, expect domain.Version) (domain.Member, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.Member{}, err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.role_bindings SET role=$3 WHERE team_id=$1 AND id=$2 AND version=$4`, teamID, id, string(role), int64(expect))
	if err != nil {
		return domain.Member{}, MapError(err, OpWrite, "member")
	}
	if tag.RowsAffected() == 0 {
		return domain.Member{}, r.missOrStale(ctx, pt, teamID, id)
	}
	return r.Get(ctx, tx, teamID, id, false)
}

// Delete implements members.Repository.
func (r *MemberRepo) Delete(ctx context.Context, tx port.Tx, teamID, id string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.role_bindings WHERE team_id=$1 AND id=$2 AND version=$3`, teamID, id, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "member")
	}
	if tag.RowsAffected() == 0 {
		return r.missOrStale(ctx, pt, teamID, id)
	}
	return nil
}

func (*MemberRepo) missOrStale(ctx context.Context, pt *Tx, teamID, id string) error {
	var one int
	err := pt.QueryRow(ctx, `SELECT 1 FROM afs.role_bindings WHERE team_id=$1 AND id=$2`, teamID, id).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound("member")
	}
	if err != nil {
		return MapError(err, OpRead, "member")
	}
	return domain.PreconditionFailed()
}

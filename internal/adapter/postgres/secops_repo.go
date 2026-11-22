package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secops"
)

// SecOpsRepo implements secops.Repository on the service-level rows of afs.role_bindings
// (role security_operator, team_id NULL). Row-level security lets only service admins write them.
type SecOpsRepo struct{}

// NewSecOpsRepo returns a SecOpsRepo.
func NewSecOpsRepo() *SecOpsRepo { return &SecOpsRepo{} }

var _ secops.Repository = (*SecOpsRepo)(nil)

// secOpSelect is memberSelect with the team coalesced: these bindings have none.
const secOpSelect = `SELECT b.id, coalesce(b.team_id, ''), b.role, b.subject_type, coalesce(b.user_id, ''), coalesce(u.email, ''),
  coalesce(b.group_name, ''), coalesce(b.identity_provider_id, ''), b.version, b.created_at, b.created_by, b.updated_at
FROM afs.role_bindings b LEFT JOIN afs.users u ON u.id = b.user_id
WHERE b.role = 'security_operator' AND b.team_id IS NULL`

// List implements secops.Repository.
func (*SecOpsRepo) List(ctx context.Context, tx port.Tx, after string, limit int) ([]secops.Operator, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, secOpSelect+` AND b.id > $1 ORDER BY b.id LIMIT $2`, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "security operator")
	}
	defer rows.Close()
	out := []secops.Operator{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "security operator")
		}
		out = append(out, m)
	}
	return out, MapError(rows.Err(), OpRead, "security operator")
}

// Get implements secops.Repository.
func (*SecOpsRepo) Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (secops.Operator, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return secops.Operator{}, err
	}
	q := secOpSelect + ` AND b.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF b`
	}
	m, err := scanMember(pt.QueryRow(ctx, q, id))
	return m, MapError(err, OpRead, "security operator")
}

// ResolveUser implements secops.Repository.
func (*SecOpsRepo) ResolveUser(ctx context.Context, tx port.Tx, userID, email string) (string, bool, error) {
	return resolveUser(ctx, tx, userID, email)
}

// Insert implements secops.Repository.
func (*SecOpsRepo) Insert(ctx context.Context, tx port.Tx, m secops.Operator) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.role_bindings (id, role, team_id, subject_type, user_id, group_name, identity_provider_id)
VALUES ($1, 'security_operator', NULL, $2, $3, $4, $5)`, m.ID, string(m.SubjectType),
		nullable(m.UserID), nullable(m.GroupName), nullable(m.IdentityProviderID))
	return MapError(err, OpWrite, "security operator")
}

// Delete implements secops.Repository.
func (*SecOpsRepo) Delete(ctx context.Context, tx port.Tx, id string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.role_bindings WHERE role = 'security_operator' AND team_id IS NULL AND id = $1 AND version = $2`,
		id, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "security operator")
	}
	if tag.RowsAffected() == 0 {
		var one int
		err := pt.QueryRow(ctx, `SELECT 1 FROM afs.role_bindings WHERE role = 'security_operator' AND team_id IS NULL AND id = $1`, id).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("security operator")
		}
		if err != nil {
			return MapError(err, OpRead, "security operator")
		}
		return domain.PreconditionFailed()
	}
	return nil
}

// ServiceAPIKeyRepo implements apikeys.ServiceRepository on the rows of afs.api_keys with no team
// (role security_operator). Only the SHA-256 of a key is stored.
type ServiceAPIKeyRepo struct{}

// NewServiceAPIKeyRepo returns a ServiceAPIKeyRepo.
func NewServiceAPIKeyRepo() *ServiceAPIKeyRepo { return &ServiceAPIKeyRepo{} }

var _ apikeys.ServiceRepository = (*ServiceAPIKeyRepo)(nil)

const serviceKeySelect = `id, coalesce(team_id, ''), name, role, key_prefix, expires_at, last_used_at, created_at, created_by` //nolint:gosec // a column list, not a credential

// List implements apikeys.ServiceRepository.
func (*ServiceAPIKeyRepo) List(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.APIKey, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT `+serviceKeySelect+` FROM afs.api_keys WHERE team_id IS NULL AND id > $1 ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "api key")
	}
	defer rows.Close()
	out := []domain.APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "api key")
		}
		out = append(out, k)
	}
	return out, MapError(rows.Err(), OpRead, "api key")
}

// Insert implements apikeys.ServiceRepository. UNIQUE (team_id, name) does not cover rows with no
// team (NULLs are distinct), so the name is checked under an advisory lock instead.
func (*ServiceAPIKeyRepo) Insert(ctx context.Context, tx port.Tx, k domain.APIKey, sha [32]byte) (domain.APIKey, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.APIKey{}, err
	}
	if _, err := pt.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('afs.service_api_key_name'))`); err != nil {
		return domain.APIKey{}, MapError(err, OpWrite, "api key")
	}
	var one int
	switch err := pt.QueryRow(ctx, `SELECT 1 FROM afs.api_keys WHERE team_id IS NULL AND name = $1`, k.Name).Scan(&one); {
	case err == nil:
		return domain.APIKey{}, domain.Conflict("duplicate", "a service API key with this name exists", nil)
	case !errors.Is(err, pgx.ErrNoRows):
		return domain.APIKey{}, MapError(err, OpRead, "api key")
	}
	out, err := scanAPIKey(pt.QueryRow(ctx, `INSERT INTO afs.api_keys (id, team_id, name, role, key_prefix, key_sha256, expires_at)
VALUES ($1, NULL, $2, $3, $4, $5, $6) RETURNING `+serviceKeySelect, k.ID, k.Name, string(k.Role), k.KeyPrefix, sha[:], k.ExpiresAt))
	return out, MapError(err, OpWrite, "api key")
}

// Delete implements apikeys.ServiceRepository.
func (*ServiceAPIKeyRepo) Delete(ctx context.Context, tx port.Tx, id string) (domain.APIKey, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.APIKey{}, err
	}
	k, err := scanAPIKey(pt.QueryRow(ctx, `DELETE FROM afs.api_keys WHERE team_id IS NULL AND id = $1 RETURNING `+serviceKeySelect, id))
	return k, MapError(err, OpDelete, "api key")
}

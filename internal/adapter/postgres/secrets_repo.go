package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
)

// SecretRepo implements secrets.Repository on afs.secrets. It stores references only: there is no
// column that could hold a value.
type SecretRepo struct{}

// NewSecretRepo returns a SecretRepo.
func NewSecretRepo() *SecretRepo { return &SecretRepo{} }

var _ secrets.Repository = (*SecretRepo)(nil)

const secretSelect = `SELECT s.id, s.team_id, coalesce(s.agent_id, ''), coalesce(s.pool_id, ''), s.name, s.description,
  coalesce(s.external_system_id, ''), s.path, s.secrets_manager_arn, coalesce(s.current_version_id, ''), s.last_changed_at,
  s.rotation_days, s.version, s.created_at, s.created_by, s.updated_at,
  coalesce((SELECT array_agg(b.agent_id ORDER BY b.agent_id) FROM afs.agent_secret_bindings b
            WHERE b.team_id = s.team_id AND b.secret_id = s.id), '{}')
FROM afs.secrets s`

func scanSecret(row pgx.Row) (domain.Secret, error) {
	var (
		s    domain.Secret
		days *int
		v    int64
	)
	err := row.Scan(&s.ID, &s.TeamID, &s.AgentID, &s.PoolID, &s.Name, &s.Description, &s.ExternalSystemID, &s.Path, &s.ARN,
		&s.VersionID, &s.LastChangedAt, &days, &v, &s.CreatedAt, &s.CreatedBy, &s.UpdatedAt, &s.BoundTo)
	if err != nil {
		return domain.Secret{}, err
	}
	s.Version, s.Scope = domain.Version(v), domain.ScopeOf(s.AgentID, s.PoolID)
	if days != nil {
		s.Rotation = domain.Rotation{Enabled: true, Days: *days}
	}
	if s.BoundTo == nil {
		s.BoundTo = []string{}
	}
	return s, nil
}

func rotationDays(r domain.Rotation) *int {
	if !r.Enabled {
		return nil
	}
	d := r.Days
	return &d
}

// List implements secrets.Repository.
func (*SecretRepo) List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.Secret, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, secretSelect+` WHERE s.team_id=$1 AND s.id > $2 ORDER BY s.id LIMIT $3`, teamID, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "secret")
	}
	defer rows.Close()
	out := []domain.Secret{}
	for rows.Next() {
		s, err := scanSecret(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "secret")
		}
		out = append(out, s)
	}
	return out, MapError(rows.Err(), OpRead, "secret")
}

// Get implements secrets.Repository.
func (*SecretRepo) Get(ctx context.Context, tx port.Tx, teamID, id string, forUpdate bool) (domain.Secret, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.Secret{}, err
	}
	q := secretSelect + ` WHERE s.team_id=$1 AND s.id=$2`
	if forUpdate {
		q += ` FOR UPDATE OF s`
	}
	s, err := scanSecret(pt.QueryRow(ctx, q, teamID, id))
	return s, MapError(err, OpRead, "secret")
}

// NameTaken implements secrets.Repository.
func (*SecretRepo) NameTaken(ctx context.Context, tx port.Tx, teamID, name string) (bool, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return false, err
	}
	var taken bool
	err = pt.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM afs.secrets WHERE team_id=$1 AND name=$2)`, teamID, name).Scan(&taken)
	return taken, MapError(err, OpRead, "secret")
}

// Path implements secrets.Repository: afs.secret_path names the Secrets Manager secret.
func (*SecretRepo) Path(ctx context.Context, tx port.Tx, teamID, agentID, poolID, name string) (string, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return "", err
	}
	var path string
	err = pt.QueryRow(ctx, `SELECT afs.secret_path($1, nullif($2, ''), nullif($3, ''), $4)`, teamID, agentID, poolID, name).Scan(&path)
	return path, MapError(err, OpRead, "secret")
}

// Insert implements secrets.Repository. The path column is set by the database trigger.
func (*SecretRepo) Insert(ctx context.Context, tx port.Tx, s domain.Secret) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.secrets (id, team_id, name, description, secrets_manager_arn, origin, rotation_days,
  current_version_id, last_changed_at, agent_id, pool_id, external_system_id)
VALUES ($1, $2, $3, $4, $5, 'managed', $6, nullif($7, ''), $8, nullif($9, ''), nullif($10, ''), nullif($11, ''))`,
		s.ID, s.TeamID, s.Name, s.Description, s.ARN, rotationDays(s.Rotation), s.VersionID, s.LastChangedAt, s.AgentID, s.PoolID, s.ExternalSystemID)
	return MapError(err, OpWrite, "secret")
}

// Update implements secrets.Repository.
func (*SecretRepo) Update(ctx context.Context, tx port.Tx, s domain.Secret, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.secrets SET description=$3, rotation_days=$4, external_system_id=nullif($5, '')
WHERE team_id=$1 AND id=$2 AND version=$6`, s.TeamID, s.ID, s.Description, rotationDays(s.Rotation), s.ExternalSystemID, int64(expect))
	if err != nil {
		return MapError(err, OpWrite, "secret")
	}
	if tag.RowsAffected() == 0 {
		return domain.PreconditionFailed()
	}
	return nil
}

// SetCurrentVersion implements secrets.Repository.
func (*SecretRepo) SetCurrentVersion(ctx context.Context, tx port.Tx, teamID, id, versionID string, at time.Time) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.secrets SET current_version_id=$3, last_changed_at=$4 WHERE team_id=$1 AND id=$2`, teamID, id, versionID, at)
	if err != nil {
		return MapError(err, OpWrite, "secret")
	}
	if tag.RowsAffected() == 0 {
		return domain.NotFound("secret")
	}
	return nil
}

// Delete implements secrets.Repository.
func (*SecretRepo) Delete(ctx context.Context, tx port.Tx, teamID, id string) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.secrets WHERE team_id=$1 AND id=$2`, teamID, id)
	if err != nil {
		return MapError(err, OpDelete, "secret")
	}
	if tag.RowsAffected() == 0 {
		return domain.NotFound("secret")
	}
	return nil
}

package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// APIKeyRepo implements apikeys.Repository on afs.api_keys. Only the SHA-256 of a key is stored.
type APIKeyRepo struct{}

// NewAPIKeyRepo returns an APIKeyRepo.
func NewAPIKeyRepo() *APIKeyRepo { return &APIKeyRepo{} }

var _ apikeys.Repository = (*APIKeyRepo)(nil)

const apiKeyCols = `id, team_id, name, role, key_prefix, expires_at, last_used_at, created_at, created_by`

func scanAPIKey(row pgx.Row) (domain.APIKey, error) {
	var k domain.APIKey
	var role string
	err := row.Scan(&k.ID, &k.TeamID, &k.Name, &role, &k.KeyPrefix, &k.ExpiresAt, &k.LastUsedAt, &k.CreatedAt, &k.CreatedBy)
	k.Role = domain.TeamRole(role)
	return k, err
}

// List implements apikeys.Repository.
func (*APIKeyRepo) List(ctx context.Context, tx port.Tx, teamID, after string, limit int) ([]domain.APIKey, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT `+apiKeyCols+` FROM afs.api_keys WHERE team_id=$1 AND id > $2 ORDER BY id LIMIT $3`, teamID, after, limit)
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

// Insert implements apikeys.Repository.
func (*APIKeyRepo) Insert(ctx context.Context, tx port.Tx, k domain.APIKey, sha [32]byte) (domain.APIKey, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.APIKey{}, err
	}
	out, err := scanAPIKey(pt.QueryRow(ctx, `INSERT INTO afs.api_keys (id, team_id, name, role, key_prefix, key_sha256, expires_at, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+apiKeyCols, k.ID, k.TeamID, k.Name, string(k.Role), k.KeyPrefix, sha[:], k.ExpiresAt, k.CreatedAt))
	return out, MapError(err, OpWrite, "api key")
}

// Delete implements apikeys.Repository.
func (*APIKeyRepo) Delete(ctx context.Context, tx port.Tx, teamID, id string) (domain.APIKey, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.APIKey{}, err
	}
	k, err := scanAPIKey(pt.QueryRow(ctx, `DELETE FROM afs.api_keys WHERE team_id=$1 AND id=$2 RETURNING `+apiKeyCols, teamID, id))
	return k, MapError(err, OpDelete, "api key")
}

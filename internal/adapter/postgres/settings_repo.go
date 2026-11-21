package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/settings"
)

// SettingsRepo implements settings.Repository on afs.service_settings.
type SettingsRepo struct{}

// NewSettingsRepo returns a SettingsRepo.
func NewSettingsRepo() *SettingsRepo { return &SettingsRepo{} }

var _ settings.Repository = (*SettingsRepo)(nil)

// All implements settings.Repository.
func (*SettingsRepo) All(ctx context.Context, tx port.Tx) (map[string]settings.Stored, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, `SELECT key, value, version, updated_at FROM afs.service_settings`)
	if err != nil {
		return nil, MapError(err, OpRead, "setting")
	}
	defer rows.Close()
	out := map[string]settings.Stored{}
	for rows.Next() {
		var k string
		var s settings.Stored
		var v int64
		if err := rows.Scan(&k, &s.Value, &v, &s.UpdatedAt); err != nil {
			return nil, MapError(err, OpRead, "setting")
		}
		s.Version = domain.Version(v)
		out[k] = s
	}
	return out, MapError(rows.Err(), OpRead, "setting")
}

// Put implements settings.Repository. Range checks live in the database (a 23514 becomes a
// ValidationError).
func (*SettingsRepo) Put(ctx context.Context, tx port.Tx, key string, value json.RawMessage, expect *domain.Version) (settings.Stored, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return settings.Stored{}, err
	}
	var s settings.Stored
	var v int64
	if expect != nil {
		err = pt.QueryRow(ctx, `UPDATE afs.service_settings SET value=$2::jsonb WHERE key=$1 AND version=$3
RETURNING value, version, updated_at`, key, []byte(value), int64(*expect)).Scan(&s.Value, &v, &s.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			var one int
			e := pt.QueryRow(ctx, `SELECT 1 FROM afs.service_settings WHERE key=$1`, key).Scan(&one)
			if errors.Is(e, pgx.ErrNoRows) {
				return settings.Stored{}, domain.NotFound("setting")
			}
			if e != nil {
				return settings.Stored{}, MapError(e, OpRead, "setting")
			}
			return settings.Stored{}, domain.PreconditionFailed()
		}
	} else {
		err = pt.QueryRow(ctx, `INSERT INTO afs.service_settings (key, value) VALUES ($1, $2::jsonb)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
RETURNING value, version, updated_at`, key, []byte(value)).Scan(&s.Value, &v, &s.UpdatedAt)
	}
	if err != nil {
		return settings.Stored{}, MapError(err, OpWrite, "setting")
	}
	s.Version = domain.Version(v)
	return s, nil
}

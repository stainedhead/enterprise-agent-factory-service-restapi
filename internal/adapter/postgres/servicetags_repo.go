package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/servicetags"
)

// ServiceTagRepo implements servicetags.Repository on afs.service_tags.
type ServiceTagRepo struct{}

// NewServiceTagRepo returns a ServiceTagRepo.
func NewServiceTagRepo() *ServiceTagRepo { return &ServiceTagRepo{} }

var _ servicetags.Repository = (*ServiceTagRepo)(nil)

const serviceTagSelect = `SELECT key, mode, value, default_value, allowed_values, description, version, updated_at FROM afs.service_tags`

func scanServiceTag(row pgx.Row) (domain.ServiceTag, error) {
	var t domain.ServiceTag
	var mode string
	var v int64
	err := row.Scan(&t.Key, &mode, &t.Value, &t.DefaultValue, &t.AllowedValues, &t.Description, &v, &t.UpdatedAt)
	t.Mode, t.Version = domain.ServiceTagMode(mode), domain.Version(v)
	return t, err
}

func listServiceTags(ctx context.Context, pt *Tx) ([]domain.ServiceTag, error) {
	return queryServiceTags(ctx, pt, serviceTagSelect+` ORDER BY key`)
}

func queryServiceTags(ctx context.Context, pt *Tx, q string, args ...any) ([]domain.ServiceTag, error) {
	rows, err := pt.Query(ctx, q, args...)
	if err != nil {
		return nil, MapError(err, OpRead, "service tag")
	}
	defer rows.Close()
	out := []domain.ServiceTag{}
	for rows.Next() {
		t, err := scanServiceTag(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "service tag")
		}
		out = append(out, t)
	}
	return out, MapError(rows.Err(), OpRead, "service tag")
}

// List implements servicetags.Repository.
func (*ServiceTagRepo) List(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.ServiceTag, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	return queryServiceTags(ctx, pt, serviceTagSelect+` WHERE key > $1 ORDER BY key LIMIT $2`, after, limit)
}

// Get implements servicetags.Repository.
func (*ServiceTagRepo) Get(ctx context.Context, tx port.Tx, key string, forUpdate bool) (domain.ServiceTag, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.ServiceTag{}, err
	}
	q := serviceTagSelect + ` WHERE key=$1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	t, err := scanServiceTag(pt.QueryRow(ctx, q, key))
	return t, MapError(err, OpRead, "service tag")
}

// Insert implements servicetags.Repository.
func (*ServiceTagRepo) Insert(ctx context.Context, tx port.Tx, t domain.ServiceTag) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.service_tags (key, mode, value, default_value, allowed_values, description)
VALUES ($1,$2,$3,$4,$5,$6)`, t.Key, string(t.Mode), t.Value, t.DefaultValue, t.AllowedValues, t.Description)
	return MapError(err, OpWrite, "service tag")
}

// Update implements servicetags.Repository.
func (r *ServiceTagRepo) Update(ctx context.Context, tx port.Tx, t domain.ServiceTag, expect domain.Version) (domain.ServiceTag, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.ServiceTag{}, err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.service_tags SET mode=$2, value=$3, default_value=$4, allowed_values=$5, description=$6
WHERE key=$1 AND version=$7`, t.Key, string(t.Mode), t.Value, t.DefaultValue, t.AllowedValues, t.Description, int64(expect))
	if err != nil {
		return domain.ServiceTag{}, MapError(err, OpWrite, "service tag")
	}
	if tag.RowsAffected() == 0 {
		return domain.ServiceTag{}, r.missOrStale(ctx, pt, t.Key)
	}
	return r.Get(ctx, tx, t.Key, false)
}

// Delete implements servicetags.Repository.
func (r *ServiceTagRepo) Delete(ctx context.Context, tx port.Tx, key string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.service_tags WHERE key=$1 AND version=$2`, key, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "service tag")
	}
	if tag.RowsAffected() == 0 {
		return r.missOrStale(ctx, pt, key)
	}
	return nil
}

func (*ServiceTagRepo) missOrStale(ctx context.Context, pt *Tx, key string) error {
	var one int
	err := pt.QueryRow(ctx, `SELECT 1 FROM afs.service_tags WHERE key=$1`, key).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NotFound("service tag")
	}
	if err != nil {
		return MapError(err, OpRead, "service tag")
	}
	return domain.PreconditionFailed()
}

package postgres

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/catalogsystems"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// CatalogSystemRepo implements catalogsystems.Repository: the service-level rows (team_id NULL) of
// afs.external_systems. Only service admins write them (policy app_insert/app_update/app_delete).
type CatalogSystemRepo struct{}

// NewCatalogSystemRepo returns a CatalogSystemRepo.
func NewCatalogSystemRepo() *CatalogSystemRepo { return &CatalogSystemRepo{} }

var _ catalogsystems.Repository = (*CatalogSystemRepo)(nil)

const catalogSelect = `SELECT ` + extSystemCols + ` FROM afs.external_systems s WHERE s.team_id IS NULL`

// List implements catalogsystems.Repository.
func (*CatalogSystemRepo) List(ctx context.Context, tx port.Tx, after string, limit int) ([]domain.ExternalSystem, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, catalogSelect+` AND s.id > $1 ORDER BY s.id LIMIT $2`, after, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "external system")
	}
	defer rows.Close()
	out := []domain.ExternalSystem{}
	for rows.Next() {
		x, err := scanExtSystemInto(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "external system")
		}
		out = append(out, x)
	}
	return out, MapError(rows.Err(), OpRead, "external system")
}

// Get implements catalogsystems.Repository.
func (*CatalogSystemRepo) Get(ctx context.Context, tx port.Tx, id string, forUpdate bool) (domain.ExternalSystem, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.ExternalSystem{}, err
	}
	q := catalogSelect + ` AND s.id = $1`
	if forUpdate {
		q += ` FOR UPDATE OF s`
	}
	x, err := scanExtSystemInto(pt.QueryRow(ctx, q, id))
	return x, MapError(err, OpRead, "external system")
}

// Insert implements catalogsystems.Repository.
func (*CatalogSystemRepo) Insert(ctx context.Context, tx port.Tx, s domain.ExternalSystem) error {
	return (&ExtSystemRepo{}).Insert(ctx, tx, s)
}

// Update implements catalogsystems.Repository.
func (r *CatalogSystemRepo) Update(ctx context.Context, tx port.Tx, s domain.ExternalSystem, expect domain.Version) (domain.ExternalSystem, error) {
	if err := (&ExtSystemRepo{}).Update(ctx, tx, s, expect); err != nil {
		return domain.ExternalSystem{}, err
	}
	return r.Get(ctx, tx, s.ID, false)
}

// Delete implements catalogsystems.Repository. A built-in entry is refused by the database guard
// (55000, a Conflict); the use case refuses it first with a clearer message.
func (*CatalogSystemRepo) Delete(ctx context.Context, tx port.Tx, id string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.external_systems WHERE id = $1 AND team_id IS NULL AND version = $2`, id, int64(expect))
	if err != nil {
		return MapError(err, OpDelete, "external system")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "external system", `SELECT 1 FROM afs.external_systems WHERE id = $1 AND team_id IS NULL`, id)
	}
	return nil
}

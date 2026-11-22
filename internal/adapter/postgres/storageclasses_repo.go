package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/storageclasses"
)

// StorageClassRepo implements storageclasses.Repository on afs.storage_classes. Row-level security
// (000013) lets every afs_app caller read and only service admins write.
type StorageClassRepo struct{}

// NewStorageClassRepo returns a StorageClassRepo.
func NewStorageClassRepo() *StorageClassRepo { return &StorageClassRepo{} }

var _ storageclasses.Repository = (*StorageClassRepo)(nil)

const storageClassSelect = `SELECT id, description, volume_type, enabled, version, created_at, updated_at FROM afs.storage_classes`

func scanStorageClass(row pgx.Row) (domain.StorageClass, error) {
	var c domain.StorageClass
	var vt string
	var v int64
	err := row.Scan(&c.Name, &c.Description, &vt, &c.Enabled, &v, &c.CreatedAt, &c.UpdatedAt)
	c.VolumeType, c.Version = domain.VolumeType(vt), domain.Version(v)
	return c, err
}

// List implements storageclasses.Repository.
func (*StorageClassRepo) List(ctx context.Context, tx port.Tx, enabledOnly bool, after string, limit int) ([]domain.StorageClass, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := pt.Query(ctx, storageClassSelect+` WHERE id > $1 AND (enabled OR NOT $2) ORDER BY id LIMIT $3`, after, enabledOnly, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "storage class")
	}
	defer rows.Close()
	out := []domain.StorageClass{}
	for rows.Next() {
		c, err := scanStorageClass(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "storage class")
		}
		out = append(out, c)
	}
	return out, MapError(rows.Err(), OpRead, "storage class")
}

// Get implements storageclasses.Repository.
func (*StorageClassRepo) Get(ctx context.Context, tx port.Tx, name string, forUpdate bool) (domain.StorageClass, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.StorageClass{}, err
	}
	q := storageClassSelect + ` WHERE id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	c, err := scanStorageClass(pt.QueryRow(ctx, q, name))
	return c, MapError(err, OpRead, "storage class")
}

// Insert implements storageclasses.Repository.
func (*StorageClassRepo) Insert(ctx context.Context, tx port.Tx, c domain.StorageClass) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	_, err = pt.Exec(ctx, `INSERT INTO afs.storage_classes (id, description, volume_type, enabled) VALUES ($1, $2, $3, $4)`,
		c.Name, c.Description, string(c.VolumeType), c.Enabled)
	return MapError(err, OpWrite, "storage class")
}

// Update implements storageclasses.Repository.
func (r *StorageClassRepo) Update(ctx context.Context, tx port.Tx, c domain.StorageClass, expect domain.Version) (domain.StorageClass, error) {
	pt, err := Unwrap(tx)
	if err != nil {
		return domain.StorageClass{}, err
	}
	tag, err := pt.Exec(ctx, `UPDATE afs.storage_classes SET description = $2, volume_type = $3, enabled = $4 WHERE id = $1 AND version = $5`,
		c.Name, c.Description, string(c.VolumeType), c.Enabled, int64(expect))
	if err != nil {
		return domain.StorageClass{}, MapError(err, OpWrite, "storage class")
	}
	if tag.RowsAffected() == 0 {
		return domain.StorageClass{}, catalogMissOrStale(ctx, pt, "storage class", `SELECT 1 FROM afs.storage_classes WHERE id = $1`, c.Name)
	}
	return r.Get(ctx, tx, c.Name, false)
}

// Delete implements storageclasses.Repository. An agent that uses the class makes the foreign key
// refuse the delete; that is a Conflict that says so.
func (*StorageClassRepo) Delete(ctx context.Context, tx port.Tx, name string, expect domain.Version) error {
	pt, err := Unwrap(tx)
	if err != nil {
		return err
	}
	tag, err := pt.Exec(ctx, `DELETE FROM afs.storage_classes WHERE id = $1 AND version = $2`, name, int64(expect))
	if err != nil {
		return catalogInUse(MapError(err, OpDelete, "storage class"), "storage class "+name+" is used by agents; disable it instead", "agents")
	}
	if tag.RowsAffected() == 0 {
		return catalogMissOrStale(ctx, pt, "storage class", `SELECT 1 FROM afs.storage_classes WHERE id = $1`, name)
	}
	return nil
}

// catalogInUse rewrites a dependents_exist Conflict with a message and the kind of dependents.
func catalogInUse(err error, message, dependents string) error {
	var de *domain.Error
	if errors.As(err, &de) && de.Kind == domain.KindConflict && de.Reason == "dependents_exist" {
		out := domain.Conflict("dependents_exist", message, map[string]any{"dependents": dependents})
		out.Cause = de.Cause
		return out
	}
	return err
}

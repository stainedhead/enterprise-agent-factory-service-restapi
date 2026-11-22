package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/exports"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// ExportRepo implements exports.Repository on afs.export_bundles (row-level security limits it to
// service admins, 000014).
type ExportRepo struct{}

// NewExportRepo returns an ExportRepo.
func NewExportRepo() *ExportRepo { return &ExportRepo{} }

var _ exports.Repository = (*ExportRepo)(nil)

// MissingTeams implements exports.Repository.
func (*ExportRepo) MissingTeams(ctx context.Context, tx port.Tx, ids []string) ([]string, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT x FROM unnest($1::text[]) AS x WHERE NOT EXISTS (SELECT 1 FROM afs.teams WHERE id = x) ORDER BY x`, ids)
	if err != nil {
		return nil, MapError(err, OpRead, "team")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, MapError(err, OpRead, "team")
		}
		out = append(out, id)
	}
	return out, MapError(rows.Err(), OpRead, "team")
}

// Insert implements exports.Repository.
func (*ExportRepo) Insert(ctx context.Context, tx port.Tx, e domain.Export) error {
	t, err := Unwrap(tx)
	if err != nil {
		return err
	}
	summary, _ := json.Marshal(map[string]any{"teamIds": e.TeamIDs})
	_, err = t.Exec(ctx, `INSERT INTO afs.export_bundles (id, direction, schema_version, status, summary)
VALUES ($1, 'export', $2, 'pending', $3)`, e.ID, e.SchemaVersion, summary)
	return MapError(err, OpWrite, "export")
}

const exportCols = `id, status, schema_version, created_at, created_by, finished_at, sha256, size_bytes, s3_uri, download_url,
  download_expires_at, summary`

func scanExport(row pgx.Row) (domain.Export, error) {
	var e domain.Export
	var status string
	var summary []byte
	if err := row.Scan(&e.ID, &status, &e.SchemaVersion, &e.CreatedAt, &e.CreatedBy, &e.FinishedAt, &e.SHA256, &e.SizeBytes,
		&e.S3URI, &e.DownloadURL, &e.DownloadExpiresAt, &summary); err != nil {
		return e, err
	}
	e.Status = domain.ExportStatus(status)
	var s struct {
		TeamIDs []string `json:"teamIds"`
		Error   *string  `json:"error"`
	}
	_ = json.Unmarshal(summary, &s)
	e.TeamIDs, e.Error = s.TeamIDs, s.Error
	return e, nil
}

// Get implements exports.Repository.
func (*ExportRepo) Get(ctx context.Context, tx port.Tx, id string) (domain.Export, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return domain.Export{}, err
	}
	e, err := scanExport(t.QueryRow(ctx, `SELECT `+exportCols+` FROM afs.export_bundles WHERE id = $1 AND direction = 'export'`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return e, domain.NotFound("export")
	}
	return e, MapError(err, OpRead, "export")
}

// List implements exports.Repository.
func (*ExportRepo) List(ctx context.Context, tx port.Tx, before string, limit int) ([]domain.Export, error) {
	t, err := Unwrap(tx)
	if err != nil {
		return nil, err
	}
	rows, err := t.Query(ctx, `SELECT `+exportCols+` FROM afs.export_bundles WHERE direction = 'export' AND ($1 = '' OR id < $1)
ORDER BY id DESC LIMIT $2`, before, limit)
	if err != nil {
		return nil, MapError(err, OpRead, "export")
	}
	defer rows.Close()
	out := []domain.Export{}
	for rows.Next() {
		e, err := scanExport(rows)
		if err != nil {
			return nil, MapError(err, OpRead, "export")
		}
		out = append(out, e)
	}
	return out, MapError(rows.Err(), OpRead, "export")
}

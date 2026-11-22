// Package exports implements CreateExport, GetExport and ListExports (RA-12). afs-api only records
// the request; the reconciler builds the bundle (RC-20) and afs-api never touches S3.
package exports

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Repository stores export requests.
type Repository interface {
	// MissingTeams returns the ids among ids that name no team.
	MissingTeams(ctx context.Context, tx port.Tx, ids []string) ([]string, error)
	// Insert stores a pending export.
	Insert(ctx context.Context, tx port.Tx, e domain.Export) error
	// Get returns one export; NotFound otherwise.
	Get(ctx context.Context, tx port.Tx, id string) (domain.Export, error)
	// List returns exports with an id below before ("" for the newest), newest first.
	List(ctx context.Context, tx port.Tx, before string, limit int) ([]domain.Export, error)
}

// Service is the export use case set. Access (admin:export, service admins only) is enforced by the
// route's permission; the use cases trust it.
type Service struct {
	acting.Deps
	Repo Repository
}

// Result is the outcome of Create: the new export or the replay of an earlier response.
type Result struct {
	Export domain.Export
	Replay *port.StoredResponse
}

// Create implements CreateExport: it records a pending export of teamIDs (empty: every team).
func (s *Service) Create(ctx context.Context, c acting.Caller, teamIDs []string, idem *acting.Idem) (Result, error) {
	ids, err := domain.ValidateExportTeams(teamIDs)
	if err != nil {
		return Result{}, err
	}
	var res Result
	err = s.Tx.InTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		res = Result{}
		replay, err := idem.Begin(ctx, s.Idem, tx)
		if err != nil || replay != nil {
			res.Replay = replay
			return err
		}
		missing, err := s.Repo.MissingTeams(ctx, tx, ids)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			return domain.Validation(domain.FieldError{Field: "teamIds", Reason: "unknown team " + missing[0]})
		}
		id, err := s.IDs.NewID(domain.PrefixExport)
		if err != nil {
			return err
		}
		e := domain.Export{ID: id.String(), Status: domain.ExportStatusPending, SchemaVersion: domain.BundleSchemaVersion,
			TeamIDs: ids, CreatedAt: s.Now(), CreatedBy: c.Session().Actor}
		if err := s.Repo.Insert(ctx, tx, e); err != nil {
			return err
		}
		// No outbox event: export.completed and export.failed are the reconciler's (API contract §7).
		if err := s.Audit.Write(ctx, tx, c.Record("CreateExport", "export_bundles", e.ID, "", nil,
			map[string]any{"teamIds": ids, "schemaVersion": e.SchemaVersion})); err != nil {
			return err
		}
		res.Export = e
		return idem.Complete(ctx, s.Idem, tx, e)
	})
	return res, err
}

// Get implements GetExport. After the download link expired the export stays readable without a
// link: a new export is needed (RC-US-17.2).
func (s *Service) Get(ctx context.Context, c acting.Caller, id string) (domain.Export, error) {
	if _, err := domain.ParseID(domain.PrefixExport, id); err != nil {
		return domain.Export{}, domain.NotFound("export")
	}
	var e domain.Export
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		e, err = s.Repo.Get(ctx, tx, id)
		return err
	})
	return s.withoutExpiredLink(e), err
}

func (s *Service) withoutExpiredLink(e domain.Export) domain.Export {
	if e.ExpiredAt(s.Now()) {
		e.DownloadURL = nil
	}
	return e
}

// List implements ListExports.
func (s *Service) List(ctx context.Context, c acting.Caller, p acting.Page) ([]domain.Export, string, error) {
	p = p.Normalized()
	var rows []domain.Export
	err := s.Tx.ReadTx(ctx, c.Session(), func(ctx context.Context, tx port.Tx) error {
		var err error
		rows, err = s.Repo.List(ctx, tx, p.Cursor, p.Limit+1)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	page, next := acting.Slice(rows, p.Limit, func(e domain.Export) string { return e.ID })
	for i := range page {
		page[i] = s.withoutExpiredLink(page[i])
	}
	return page, next, nil
}

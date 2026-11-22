package matrix

import (
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres/pgtest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Cases of M4-F (export and import). Every operation needs admin:export, which only service admins hold.
// CreateExport only records the request: export.completed and export.failed are the reconciler's events,
// so the request itself has no outbox event. CreateImport applies an empty bundle (dryRun=false) so the
// import's own audit record and import.applied are the whole effect.

const noExportEvent = "CreateExport records a pending request; export.completed and export.failed are published by the reconciler (API contract §7)"

func init() {
	export := func(t *testing.T, w *World) map[string]string {
		id := pgtest.NewID("exp")
		w.exec(t, `INSERT INTO afs.export_bundles (id, direction, schema_version, status) VALUES ($1, 'export', $2, 'pending')`, id, domain.BundleSchemaVersion)
		return map[string]string{"exportId": id}
	}
	Register(Case{Op: "CreateExport", Path: "/v1/exports", Body: `{}`, NoOutbox: noExportEvent})
	Register(Case{Op: "GetExport", Path: "/v1/exports/{exportId}", Prepare: export})
	Register(Case{Op: "ListExports", Path: "/v1/exports", Prepare: export})

	_, doc, err := domain.Bundle{SchemaVersion: domain.BundleSchemaVersion, ExportedAt: "2026-11-22T00:00:00Z",
		ExternalSystems: []domain.Row{}, Teams: []domain.TeamBundle{}}.Seal()
	if err != nil {
		panic(err)
	}
	Register(Case{Op: "CreateImport", Path: "/v1/imports?dryRun=false", Body: `{"bundle":` + string(doc) + `}`})
}

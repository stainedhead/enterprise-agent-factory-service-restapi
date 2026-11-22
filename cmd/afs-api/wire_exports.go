package main

import (
	crand "crypto/rand"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/exports"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/imports"
)

// wireExports builds the export and import use cases (M4-F) and binds their handlers. afs-api records
// export requests and applies imports; it makes no S3 call (RA-US-14.5).
func wireExports(c *components, log *slog.Logger) {
	ids := m1w2IDs{crand.Reader}
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: ids}
	httpapi.BindExports(&httpapi.ExportsAPI{
		Exports: &exports.Service{Deps: deps, Repo: postgres.NewExportRepo()},
		Imports: &imports.Service{Deps: deps, Repo: postgres.NewImportRepo(ids)},
		Pages:   httpapi.NewPageTokens(pageTokenKey(log), nil),
	})
}

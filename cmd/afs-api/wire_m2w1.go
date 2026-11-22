package main

import (
	crand "crypto/rand"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agenttemplates"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/catalogsystems"
	configuc "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/config"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/extsys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/models"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/storageclasses"
)

// wireM2W1 builds the use cases of M2-A (catalogs), M2-D (configuration values) and M2-E (external
// systems) on the database components and binds their handlers.
func wireM2W1(c *components, log *slog.Logger) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	pages := httpapi.NewPageTokens(pageTokenKey(log), nil)

	httpapi.BindCatalog(&httpapi.CatalogAPI{
		Models:         &models.Service{Deps: deps, Repo: postgres.NewModelRepo()},
		StorageClasses: &storageclasses.Service{Deps: deps, Repo: postgres.NewStorageClassRepo()},
		Templates:      &agenttemplates.Service{Deps: deps, Repo: postgres.NewAgentTemplateRepo()},
		Systems:        &catalogsystems.Service{Deps: deps, Repo: postgres.NewCatalogSystemRepo()},
		Pages:          pages,
	})
	httpapi.BindConfig(&httpapi.ConfigAPI{Config: &configuc.Service{Deps: deps, Repo: postgres.NewConfigRepo()}, Pages: pages})
	httpapi.BindExtSystems(&httpapi.ExtSystemsAPI{Systems: &extsys.Service{Deps: deps, Repo: postgres.NewExtSystemRepo()}, Pages: pages})
}

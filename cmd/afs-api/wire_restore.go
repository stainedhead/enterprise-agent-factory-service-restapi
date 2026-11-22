package main

import (
	crand "crypto/rand"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/restore"
)

// wireRestore builds the restore use case (M4-G) and binds its handler.
func wireRestore(c *components) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	httpapi.BindRestore(&httpapi.RestoreAPI{Svc: &restore.Service{Deps: deps, Repo: postgres.NewRestoreRepo()}})
}

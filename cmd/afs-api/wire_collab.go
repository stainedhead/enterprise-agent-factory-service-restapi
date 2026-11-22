package main

import (
	crand "crypto/rand"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/collab"
)

// wireCollab builds the Paperclip collaboration use cases (RA-17) on the database components and the
// Secrets Manager writer wireSecrets built, and binds their handlers.
func wireCollab(c *components) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	svc := &collab.Service{Deps: deps, Repo: postgres.NewCollabRepo(), Settings: c.Settings, Writer: c.SecretsWriter, Rand: crand.Reader}
	c.bind(httpapi.BindCollaboration(&httpapi.CollaborationAPI{Svc: svc, Pages: httpapi.NewPageTokens(c.PageKey, nil)}))
}

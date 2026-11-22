package main

import (
	crand "crypto/rand"
	"log/slog"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secops"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
)

// wireSecOps builds the security operator, service API key and team directory use cases (M6-A) and
// binds their handlers. The directory reads afs.team_directory() (X-DB-3) through the credential
// lookup, because row-level security hides every team from a security operator.
func wireSecOps(c *components, log *slog.Logger) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	httpapi.BindSecOps(&httpapi.SecOpsAPI{
		Operators: &secops.Service{Deps: deps, Repo: postgres.NewSecOpsRepo()},
		Keys:      &apikeys.ServiceKeys{Deps: deps, Repo: postgres.NewServiceAPIKeyRepo(), Entropy: crand.Reader},
		Directory: &teams.Directory{Source: c.Creds},
		Pages:     httpapi.NewPageTokens(pageTokenKey(log), nil),
	})
}

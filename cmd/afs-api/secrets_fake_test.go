package main

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets/secretstest"
)

// Every test in this package runs with an in-memory Secrets Manager: no test reaches AWS.
func init() {
	fake := secretstest.NewWriter()
	newSecretsWriter = func(context.Context, settings) (secrets.Writer, error) { return fake, nil }
}

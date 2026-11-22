package main

import (
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi/middleware"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/oidc"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/me"
)

// wireAuth installs bearer authentication (M1-A): identity provider tokens validated against their
// JWKS, API keys, the per-principal rate limit, and the handlers of GetMe and ListSignInProviders.
func wireAuth(c *components, cfg *httpapi.Config, p *pipeline, s settings) {
	principals := postgres.NewPrincipalRepo(c.Runner, nil)
	meRepo := postgres.NewMeRepo(c.Runner)
	svc := &auth.Service{
		Verifier:  oidc.NewVerifier(oidc.Options{HTTP: s.OIDCHTTP}),
		Providers: principals,
		Store:     principals,
		Keys:      c.Creds,
	}
	c.Auth = svc
	cfg.Auth = &middleware.Authn{Auth: svc, Limiter: p.Limiter}
	cfg.Registrars = append(cfg.Registrars, httpapi.MeRegistrar(&me.Service{Teams: meRepo}, svc))
}

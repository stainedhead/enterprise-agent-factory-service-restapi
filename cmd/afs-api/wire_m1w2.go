package main

import (
	"context"
	crand "crypto/rand"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/httpapi"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/postgres"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/apikeys"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/audit"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/idp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/members"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/revisions"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/servicetags"
	settingsuc "github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/settings"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
)

// m1w2Clock is the wall clock.
type m1w2Clock struct{}

func (m1w2Clock) Now() time.Time { return time.Now() }

// m1w2IDs generates prefixed ULIDs.
type m1w2IDs struct{ entropy io.Reader }

func (g m1w2IDs) NewID(p domain.IDPrefix) (domain.ID, error) {
	return domain.NewID(p, time.Now(), g.entropy)
}

// m1w2Teams answers the authorization middleware's team lookups with the teams use case, as the
// calling principal (so row-level security applies).
type m1w2Teams struct{ svc *teams.Service }

func (t m1w2Teams) caller(ctx context.Context) acting.Caller {
	p, _ := httpapi.PrincipalFrom(ctx)
	return acting.Caller{Principal: p, RequestID: httpapi.RequestIDFrom(ctx)}
}

func (t m1w2Teams) TeamFlags(ctx context.Context, teamID string) (authz.TeamFlags, bool, error) {
	f, _, ok, err := t.svc.Flags(ctx, t.caller(ctx), teamID)
	return f, ok, err
}

func (t m1w2Teams) TeamState(ctx context.Context, teamID string) (domain.TeamState, bool, error) {
	_, st, ok, err := t.svc.Flags(ctx, t.caller(ctx), teamID)
	return st, ok, err
}

// pageTokenKey is AFS_PAGE_TOKEN_KEY (at least 32 bytes, shared by every replica); without it a
// random key is drawn, so nextToken values do not survive a restart or cross replicas.
func pageTokenKey(log *slog.Logger) []byte {
	if k := os.Getenv("AFS_PAGE_TOKEN_KEY"); len(k) >= 32 {
		return []byte(k)
	}
	log.Warn("AFS_PAGE_TOKEN_KEY is unset or shorter than 32 bytes; using a random per-process key")
	k := make([]byte, 32)
	_, _ = crand.Read(k)
	return k
}

// wireM1W2 builds the use cases of M1-B, M1-C and M1-E on the database components and binds their
// handlers. It also sets the team lookups of the authorization middleware.
func wireM1W2(c *components, cfg *httpapi.Config, log *slog.Logger) {
	deps := acting.Deps{Tx: c.Runner, Audit: c.Audit, Outbox: c.Outbox, Idem: c.Idem, Clock: m1w2Clock{}, IDs: m1w2IDs{crand.Reader}}
	pages := httpapi.NewPageTokens(pageTokenKey(log), nil)
	setRepo := postgres.NewSettingsRepo()

	teamSvc := &teams.Service{Deps: deps, Repo: postgres.NewTeamRepo(), Settings: c.Settings}
	lookup := m1w2Teams{teamSvc}
	cfg.Teams, cfg.TeamState = lookup, lookup

	httpapi.BindTeams(&httpapi.TeamsAPI{Svc: teamSvc, Pages: pages, Now: time.Now})
	httpapi.BindMembers(&httpapi.MembersAPI{Svc: &members.Service{Deps: deps, Repo: postgres.NewMemberRepo()}, Pages: pages})
	httpapi.BindAPIKeys(&httpapi.APIKeysAPI{Svc: &apikeys.Service{Deps: deps, Repo: postgres.NewAPIKeyRepo(), Entropy: crand.Reader}, Pages: pages})
	httpapi.BindAdmin1(&httpapi.Admin1API{
		Settings:    &settingsuc.Service{Deps: deps, Repo: setRepo},
		IdPs:        &idp.Service{Deps: deps, Repo: postgres.NewIdPRepo()},
		ServiceTags: &servicetags.Service{Deps: deps, Repo: postgres.NewServiceTagRepo()},
		Pages:       pages,
	})
	httpapi.BindAudit(&httpapi.AuditAPI{Svc: &audit.Service{Deps: deps, Repo: postgres.NewAuditRepo()}, Pages: pages})
	httpapi.BindRevisions(&httpapi.RevisionsAPI{Svc: &revisions.Service{Deps: deps, Repo: postgres.NewRevisionRepo()}, Pages: pages})
}

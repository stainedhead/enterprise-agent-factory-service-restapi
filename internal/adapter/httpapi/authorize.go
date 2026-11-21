package httpapi

import (
	"context"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Authenticator turns a request's credential into a principal. It returns (nil, nil) when the
// request carries no credential and Unauthenticated for a credential that is invalid, expired or
// unknown.
type Authenticator interface {
	Authenticate(r *http.Request) (*authz.Principal, error)
}

// TeamLookup returns the flags of a team the caller is bound to; ok is false when the team does not
// exist (or is deleted).
type TeamLookup interface {
	TeamFlags(ctx context.Context, teamID string) (flags authz.TeamFlags, ok bool, err error)
}

// Denial describes a refused request for the audit hook.
type Denial struct {
	Principal          authz.Principal
	OperationID        string
	TeamID             string
	RequiredPermission authz.Permission
}

// Hooks observe authorization outcomes.
type Hooks struct {
	// Denied is called for every 403; it writes the denied audit record in its own transaction.
	Denied func(ctx context.Context, d Denial)
	// CrossTeamNotFound is called for every 404 caused by a missing binding (security metric).
	CrossTeamNotFound func(ctx context.Context, operationID string)
}

// PrincipalFrom returns the authenticated principal of the request, if any.
func PrincipalFrom(ctx context.Context) (authz.Principal, bool) {
	p, ok := ctx.Value(keyPrincipal).(*authz.Principal)
	if !ok || p == nil {
		return authz.Principal{}, false
	}
	return *p, true
}

// authorize applies PRD §3 in order: 401, then 404 for an unbound or unknown team, then 403 for a
// security operator on a team route, then 403 with requiredPermission. Work order routes are
// checked again by their use case once the work order is loaded.
func authorize(op *operation, auth Authenticator, teams TeamLookup, hooks Hooks, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if op.Scope == authz.ScopePublic {
			next.ServeHTTP(w, r)
			return
		}
		var p *authz.Principal
		var err error
		if auth != nil {
			p, err = auth.Authenticate(r)
		}
		if err != nil {
			if domain.KindOf(err) != domain.KindUnauthenticated && domain.KindOf(err) != domain.KindUnavailable {
				err = domain.Unauthenticated()
			}
			WriteError(w, r, err)
			return
		}
		if p == nil {
			WriteError(w, r, domain.Unauthenticated())
			return
		}
		ctx := context.WithValue(r.Context(), keyPrincipal, p)
		res := authz.Resource{Scope: op.Scope}
		if op.Scope == authz.ScopeTeam {
			res.TeamID = r.PathValue("teamId")
			if len(p.TeamRoles(res.TeamID)) > 0 && teams != nil {
				flags, ok, err := teams.TeamFlags(ctx, res.TeamID)
				if err != nil {
					WriteError(w, r, err)
					return
				}
				if !ok {
					notFound(w, r.WithContext(ctx), op, hooks)
					return
				}
				res.Flags = flags
			}
		}
		d := authz.Check(*p, op.requirement(), res)
		switch d.Outcome {
		case authz.Allow:
			next.ServeHTTP(w, r.WithContext(ctx))
		case authz.NotFound:
			notFound(w, r.WithContext(ctx), op, hooks)
		default:
			if hooks.Denied != nil {
				hooks.Denied(ctx, Denial{Principal: *p, OperationID: op.OperationID, TeamID: res.TeamID, RequiredPermission: d.RequiredPermission})
			}
			WriteError(w, r, domain.AccessDenied(string(d.RequiredPermission)))
		}
	})
}

func notFound(w http.ResponseWriter, r *http.Request, op *operation, hooks Hooks) {
	if hooks.CrossTeamNotFound != nil {
		hooks.CrossTeamNotFound(r.Context(), op.OperationID)
	}
	WriteError(w, r, domain.NotFound("team"))
}

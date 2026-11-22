package httpapi

import (
	"context"
	"net/http"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// TeamStateLookup returns the lifecycle state of a team as the caller sees it; ok is false when
// the team does not exist.
type TeamStateLookup interface {
	TeamState(ctx context.Context, teamID string) (state domain.TeamState, ok bool, err error)
}

// suspendedExempt are the team mutations that stay available on a suspended team (RA-US-16.1).
var suspendedExempt = map[string]bool{"ResumeTeam": true, "DeleteTeam": true}

// guardsSuspended reports whether op is refused on a suspended team: a mutation on a team route.
func guardsSuspended(op *operation) bool {
	return op.Mutation && op.Scope == authz.ScopeTeam && !suspendedExempt[op.OperationID]
}

// suspendedGuard wraps next, which runs after authorization: on a suspended team it answers 409
// with details.reason team_suspended. Reads are never wrapped.
func suspendedGuard(op *operation, lookup TeamStateLookup, next http.Handler) http.Handler {
	if lookup == nil || !guardsSuspended(op) {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state, ok, err := lookup.TeamState(r.Context(), r.PathValue("teamId"))
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if ok && state == domain.TeamStateSuspended {
			WriteError(w, r, domain.Conflict(domain.ReasonTeamSuspended, "the team is suspended; resume it first", nil))
			return
		}
		next.ServeHTTP(w, r)
	})
}

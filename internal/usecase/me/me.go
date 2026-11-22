// Package me implements GetMe (RA-3): who the caller is and what the API would let them do, per bound
// team. The answer is the API's own decision (authz.Effective); clients use it only to shape the UX.
package me

import (
	"context"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/auth"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// TeamInfo is what GetMe needs of a team: its slug and the flags that conditional grants read.
type TeamInfo struct {
	ID    string
	Slug  string
	Flags authz.TeamFlags
}

// TeamSource reads the teams a caller is bound to. It returns only teams that exist and are not
// deleted, in any order; ids it does not know are skipped.
type TeamSource interface {
	Teams(ctx context.Context, s port.Session, ids []string) ([]TeamInfo, error)
}

// WorkloadRole is the role reported for a workload principal on its own team.
const WorkloadRole = "workload"

// PrincipalInfo identifies the caller.
type PrincipalInfo struct {
	ID    string
	Type  authz.PrincipalType
	Email string
	Name  string
}

// TeamEntry is one bound team with the caller's role and effective permissions on it.
type TeamEntry struct {
	TeamID      string
	Slug        string
	Role        string
	Permissions []authz.Permission
}

// Result is the answer of GetMe.
type Result struct {
	Principal          PrincipalInfo
	ServiceAdmin       bool
	SecurityOperator   bool
	ServicePermissions []authz.Permission
	Teams              []TeamEntry
}

// Service implements GetMe.
type Service struct {
	Teams TeamSource
}

// Get returns the caller's identity and permissions (RA-US-3a).
func (s *Service) Get(ctx context.Context, p authz.Principal, requestID string) (Result, error) {
	res := Result{
		Principal:          PrincipalInfo{ID: p.ID, Type: p.Type, Email: p.Email, Name: p.Name},
		ServiceAdmin:       p.ServiceAdmin,
		SecurityOperator:   p.SecurityOperator,
		ServicePermissions: authz.ServicePermissions(p),
		Teams:              []TeamEntry{},
	}
	ids := p.TeamIDs()
	if p.Workload != nil {
		ids = []string{p.Workload.TeamID}
	}
	if len(ids) == 0 {
		return res, nil
	}
	sess := auth.SessionOf(p, requestID)
	sess.TeamIDs = ids
	infos, err := s.Teams.Teams(ctx, sess, ids)
	if err != nil {
		return Result{}, err
	}
	byID := make(map[string]TeamInfo, len(infos))
	for _, t := range infos {
		byID[t.ID] = t
	}
	for _, id := range ids {
		t, ok := byID[id]
		if !ok {
			continue
		}
		role, perms := roleAndPermissions(p, t)
		res.Teams = append(res.Teams, TeamEntry{TeamID: t.ID, Slug: t.Slug, Role: role, Permissions: perms})
	}
	return res, nil
}

func roleAndPermissions(p authz.Principal, t TeamInfo) (string, []authz.Permission) {
	if p.Workload != nil {
		// A workload holds nothing on team routes; its one grant (work-orders:report) is evaluated
		// where a work order is known, so it is listed from the any-scope decision.
		var perms []authz.Permission
		for _, perm := range authz.AllPermissions() {
			if authz.Authorize(p, perm, authz.Resource{Scope: authz.ScopeAny}).Allowed() {
				perms = append(perms, perm)
			}
		}
		return WorkloadRole, perms
	}
	role := authz.RoleTeamViewer
	for _, r := range p.TeamRoles(t.ID) {
		if r == authz.RoleTeamAdmin {
			role = r
		}
	}
	return string(role), authz.Effective(p, t.ID, t.Flags)
}

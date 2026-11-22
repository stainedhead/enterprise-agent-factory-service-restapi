package teams

import (
	"context"
	"sort"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

// Directory implements ListTeamDirectory (M6-A3): the id, slug and display name of every team that is
// not deleted, for service admins and security operators. Row-level security hides every team from a
// security operator, so the read goes through afs.team_directory() (X-DB-3), which refuses all other
// callers; the use case checks the permission as well, so the answer does not depend on the database
// alone.
type Directory struct {
	Source port.TeamDirectory
}

// List returns one page of the directory in slug order; after is the slug of the last row of the
// previous page ("" for the first).
func (d *Directory) List(ctx context.Context, c acting.Caller, p acting.Page) ([]port.TeamDirectoryEntry, string, error) {
	if !authz.Authorize(c.Principal, authz.TeamsReadDirectory, authz.Resource{Scope: authz.ScopeService}).Allowed() {
		return nil, "", domain.AccessDenied(string(authz.TeamsReadDirectory))
	}
	p = p.Normalized()
	all, err := d.Source.ListTeamDirectory(ctx, c.Session())
	if err != nil {
		return nil, "", err
	}
	rows := make([]port.TeamDirectoryEntry, 0, len(all))
	for _, e := range all {
		if p.Cursor == "" || e.Slug > p.Cursor {
			rows = append(rows, e)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Slug < rows[j].Slug })
	if len(rows) > p.Limit+1 {
		rows = rows[:p.Limit+1]
	}
	page, next := acting.Slice(rows, p.Limit, func(e port.TeamDirectoryEntry) string { return e.Slug })
	return page, next, nil
}

package teams_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/teams"
)

func TestDirectoryAllowsServiceAdminsAndSecurityOperatorsOnly(t *testing.T) {
	src := &fakes.Credentials{Teams: []port.TeamDirectoryEntry{{ID: "team_b", Slug: "b", DisplayName: "B"}, {ID: "team_a", Slug: "a", DisplayName: "A"}}}
	d := &teams.Directory{Source: src}
	const tid = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	for _, tc := range []struct {
		name string
		p    authz.Principal
		ok   bool
	}{
		{"service admin", authz.Principal{ID: "usr_1", ServiceAdmin: true}, true},
		{"security operator", authz.Principal{ID: "usr_2", SecurityOperator: true}, true},
		{"service key", authz.Principal{ID: "key_1", Type: authz.PrincipalAPIKey, SecurityOperator: true}, true},
		{"team admin", authz.Principal{ID: "usr_3", Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: tid}}}, false},
		{"team viewer", authz.Principal{ID: "usr_4", Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: tid}}}, false},
		{"workload", authz.Principal{ID: "pool_1", Type: authz.PrincipalWorkload, Workload: &authz.Workload{TeamID: tid, PoolID: "pool_1"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, _, err := d.List(t.Context(), acting.Caller{Principal: tc.p}, acting.Page{})
			if tc.ok {
				if err != nil || len(rows) != 2 || rows[0].Slug != "a" {
					t.Fatalf("rows=%v err=%v", rows, err)
				}
				return
			}
			if domain.KindOf(err) != domain.KindAccessDenied {
				t.Fatalf("want AccessDenied, got %v", err)
			}
		})
	}
}

func TestDirectoryPaginatesBySlug(t *testing.T) {
	var all []port.TeamDirectoryEntry
	for i := range 5 {
		all = append(all, port.TeamDirectoryEntry{ID: fmt.Sprintf("team_%d", i), Slug: fmt.Sprintf("s%d", i), DisplayName: "T"})
	}
	d := &teams.Directory{Source: &fakes.Credentials{Teams: all}}
	c := acting.Caller{Principal: authz.Principal{ID: "usr_1", ServiceAdmin: true}}
	var got []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		rows, next, err := d.List(t.Context(), c, acting.Page{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			got = append(got, r.Slug)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if fmt.Sprint(got) != "[s0 s1 s2 s3 s4]" {
		t.Fatalf("got %v", got)
	}
}

func TestDirectorySourceRefusalIsPassedOn(t *testing.T) {
	// The database function refuses callers the use case would have allowed only if the session lacks
	// the flag; the error is not swallowed.
	d := &teams.Directory{Source: refuse{}}
	_, _, err := d.List(t.Context(), acting.Caller{Principal: authz.Principal{ID: "usr_1", ServiceAdmin: true}}, acting.Page{})
	if domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("got %v", err)
	}
}

type refuse struct{}

func (refuse) ListTeamDirectory(_ context.Context, _ port.Session) ([]port.TeamDirectoryEntry, error) {
	return nil, domain.AccessDenied(string(authz.TeamsReadDirectory))
}

package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/audit"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

type repo struct {
	events []audit.Event // newest first
	teams  map[string]bool
	last   audit.Filter
}

func (r *repo) List(_ context.Context, _ port.Tx, f audit.Filter, before int64, limit int) ([]audit.Event, error) {
	r.last = f
	var out []audit.Event
	for _, e := range r.events {
		if (before == 0 || e.ID < before) && (f.TeamID == "" || e.TeamID == f.TeamID) {
			out = append(out, e)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) TeamExists(_ context.Context, _ port.Tx, id string) (bool, error) {
	return r.teams[id], nil
}

func rig() (*audit.Service, *repo, *fakes.Audit) {
	a := &fakes.Audit{}
	r := &repo{teams: map[string]bool{"team_1": true}}
	for i := int64(5); i >= 1; i-- {
		r.events = append(r.events, audit.Event{ID: i, TeamID: "team_1"})
	}
	return &audit.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Outbox: &fakes.Outbox{}, Clock: &fakes.Clock{}, IDs: &fakes.IDGen{}}, Repo: r}, r, a
}

var (
	svcAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true}}
	teamAdm  = acting.Caller{Principal: authz.Principal{ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: "team_1"}}}}
)

func TestListTeamPagesNewestFirstAndForcesTeam(t *testing.T) {
	s, r, _ := rig()
	rows, next, err := s.ListTeam(t.Context(), teamAdm, "team_1", audit.Filter{TeamID: "team_other"}, acting.Page{Limit: 2})
	if err != nil || len(rows) != 2 || rows[0].ID != 5 || next != "4" {
		t.Fatalf("page 1 = %v %q %v", rows, next, err)
	}
	if r.last.TeamID != "team_1" {
		t.Errorf("team filter = %q; the path team must win", r.last.TeamID)
	}
	rows, next, _ = s.ListTeam(t.Context(), teamAdm, "team_1", audit.Filter{}, acting.Page{Limit: 5, Cursor: "4"})
	if len(rows) != 3 || next != "" {
		t.Errorf("page 2 = %v %q", rows, next)
	}
	if _, _, err := s.ListTeam(t.Context(), teamAdm, "team_gone", audit.Filter{}, acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("missing team = %v", err)
	}
}

func TestFilterValidation(t *testing.T) {
	s, _, _ := rig()
	from, to := time.Now(), time.Now().Add(-time.Hour)
	for name, tc := range map[string]struct {
		f      audit.Filter
		cursor string
	}{
		"outcome":        {audit.Filter{Outcome: "maybe"}, ""},
		"to before from": {audit.Filter{From: &from, To: &to}, ""},
		"cursor":         {audit.Filter{}, "abc"},
	} {
		if _, _, err := s.ListService(t.Context(), svcAdmin, tc.f, acting.Page{Cursor: tc.cursor}); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s = %v", name, err)
		}
	}
}

func TestServiceListIsServiceAdminOnly(t *testing.T) {
	s, _, a := rig()
	if rows, _, err := s.ListService(t.Context(), svcAdmin, audit.Filter{}, acting.Page{}); err != nil || len(rows) != 5 {
		t.Fatalf("service admin = %d %v", len(rows), err)
	}
	if _, _, err := s.ListService(t.Context(), teamAdm, audit.Filter{}, acting.Page{}); domain.KindOf(err) != domain.KindAccessDenied {
		t.Errorf("team admin = %v", err)
	}
	if len(a.Entries) != 1 || a.Entries[0].Record.Outcome != port.OutcomeDenied {
		t.Errorf("denied audit = %+v", a.Entries)
	}
}

package accounts_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/accounts"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const team = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"

type repo struct {
	all      []domain.OrgAccount
	patterns []string
	known    bool
	// failOrg, failAllowed and failTeam make the method return the error.
	failOrg, failAllowed, failTeam error
}

func (r *repo) ListOrg(_ context.Context, _ port.Tx, after string, limit int) ([]domain.OrgAccount, error) {
	if r.failOrg != nil {
		return nil, r.failOrg
	}
	var out []domain.OrgAccount
	for _, a := range r.all {
		if a.ID > after && len(out) < limit {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *repo) ListAllowed(_ context.Context, _ port.Tx, _, after string, limit int) ([]domain.OrgAccount, error) {
	if r.failAllowed != nil {
		return nil, r.failAllowed
	}
	var out []domain.OrgAccount
	for _, a := range r.all {
		if a.ID > after && domain.AccountAllowed(r.patterns, a) && len(out) < limit {
			out = append(out, a)
		}
	}
	return out, nil
}

func (r *repo) TeamExists(context.Context, port.Tx, string) (bool, error) { return r.known, r.failTeam }

func newSvc(r *repo) (*accounts.Service, *fakes.Audit) {
	a := &fakes.Audit{}
	return &accounts.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: r}, a
}

func who(role authz.Role, tm string) acting.Caller {
	b := authz.Binding{Role: role, TeamID: tm}
	if role == authz.RoleServiceAdmin {
		b.TeamID = ""
	}
	return acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, ServiceAdmin: role == authz.RoleServiceAdmin, Bindings: []authz.Binding{b}}}
}

func TestAccounts(t *testing.T) {
	r := &repo{known: true, patterns: []string{"pay*"}, all: []domain.OrgAccount{
		{ID: "111122223333", Name: "payments", Status: domain.OrgAccountActive},
		{ID: "222233334444", Name: "sandbox", Status: domain.OrgAccountActive},
		{ID: "333344445555", Name: "payroll", Status: domain.OrgAccountSuspended},
	}}
	svc, audit := newSvc(r)
	rows, next, err := svc.ListOrg(t.Context(), who(authz.RoleServiceAdmin, ""), acting.Page{Limit: 2})
	if err != nil || len(rows) != 2 || next == "" {
		t.Fatalf("org: %v %q %v", rows, next, err)
	}
	if _, _, err := svc.ListOrg(t.Context(), who(authz.RoleTeamAdmin, team), acting.Page{}); domain.KindOf(err) != domain.KindAccessDenied || len(audit.Entries) != 1 {
		t.Fatalf("team admin org: %v", err)
	}
	rows, _, err = svc.ListAllowed(t.Context(), who(authz.RoleTeamViewer, team), team, acting.Page{})
	if err != nil || len(rows) != 1 || rows[0].ID != "111122223333" {
		t.Fatalf("allowed: %v %v", rows, err)
	}
	if _, _, err := svc.ListAllowed(t.Context(), who(authz.RoleTeamViewer, "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"), team, acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("other team: %v", err)
	}
	r.known = false
	if _, _, err := svc.ListAllowed(t.Context(), who(authz.RoleServiceAdmin, ""), team, acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("unknown team: %v", err)
	}
}

func TestRepositoryFailuresAndUnknownTeam(t *testing.T) {
	boom := errors.New("db down")
	admin := who(authz.RoleServiceAdmin, "")
	tadmin := who(authz.RoleTeamAdmin, team)
	tests := []struct {
		name string
		r    *repo
		call func(*accounts.Service) error
		want func(error) bool
	}{
		{"org list fails", &repo{failOrg: boom}, func(s *accounts.Service) error { _, _, err := s.ListOrg(t.Context(), admin, acting.Page{}); return err }, func(e error) bool { return errors.Is(e, boom) }},
		{"team lookup fails", &repo{failTeam: boom}, func(s *accounts.Service) error {
			_, _, err := s.ListAllowed(t.Context(), tadmin, team, acting.Page{})
			return err
		}, func(e error) bool { return errors.Is(e, boom) }},
		{"allowed list fails", &repo{known: true, failAllowed: boom}, func(s *accounts.Service) error {
			_, _, err := s.ListAllowed(t.Context(), tadmin, team, acting.Page{})
			return err
		}, func(e error) bool { return errors.Is(e, boom) }},
		{"team missing", &repo{known: false}, func(s *accounts.Service) error {
			_, _, err := s.ListAllowed(t.Context(), tadmin, team, acting.Page{})
			return err
		}, func(e error) bool { return domain.KindOf(e) == domain.KindNotFound }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := newSvc(tc.r)
			if err := tc.call(svc); !tc.want(err) {
				t.Errorf("err = %v", err)
			}
		})
	}
}

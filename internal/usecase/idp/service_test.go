package idp_test

import (
	"context"
	"slices"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/idp"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

type repo struct {
	rows  map[string]domain.IdentityProvider
	teams map[string][]idp.TeamRef
}

func (r *repo) List(_ context.Context, _ port.Tx, after string, limit int) ([]domain.IdentityProvider, error) {
	var out []domain.IdentityProvider
	for _, p := range r.rows {
		if p.ID > after {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b domain.IdentityProvider) int {
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) Get(_ context.Context, _ port.Tx, id string, _ bool) (domain.IdentityProvider, error) {
	p, ok := r.rows[id]
	if !ok {
		return domain.IdentityProvider{}, domain.NotFound("identity provider")
	}
	return p, nil
}

func (r *repo) TeamsUsing(_ context.Context, _ port.Tx, id string) ([]idp.TeamRef, error) {
	return r.teams[id], nil
}

func (r *repo) ClearDefault(_ context.Context, _ port.Tx, except string) error {
	for id, p := range r.rows {
		if id != except {
			p.IsDefault = false
			r.rows[id] = p
		}
	}
	return nil
}

func (r *repo) Insert(_ context.Context, _ port.Tx, p domain.IdentityProvider) error {
	p.Version = 1
	r.rows[p.ID] = p
	return nil
}

func (r *repo) Update(_ context.Context, _ port.Tx, p domain.IdentityProvider, expect domain.Version) (domain.IdentityProvider, error) {
	if r.rows[p.ID].Version != expect {
		return domain.IdentityProvider{}, domain.PreconditionFailed()
	}
	p.Version = expect + 1
	r.rows[p.ID] = p
	return p, nil
}

func (r *repo) Delete(_ context.Context, _ port.Tx, id string, expect domain.Version) error {
	if r.rows[id].Version != expect {
		return domain.PreconditionFailed()
	}
	delete(r.rows, id)
	return nil
}

func rig() (*idp.Service, *repo, *fakes.Audit) {
	a := &fakes.Audit{}
	r := &repo{rows: map[string]domain.IdentityProvider{}, teams: map[string][]idp.TeamRef{}}
	return &idp.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Outbox: &fakes.Outbox{}, Idem: &fakes.Idempotency{}, Clock: &fakes.Clock{}, IDs: &fakes.IDGen{}}, Repo: r}, r, a
}

var (
	svcAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true}}
	teamAdm  = acting.Caller{Principal: authz.Principal{ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: "team_1"}}}}
)

func provider(issuer string, def bool) domain.IdentityProvider {
	return domain.IdentityProvider{Kind: domain.IdPKindOkta, Name: "Okta", IssuerURL: issuer, ConsoleClientID: "console",
		CLIClientID: "cli", Enabled: true, IsDefault: def}
}

func TestCreateDefaultReplacesPrevious(t *testing.T) {
	s, r, audit := rig()
	a, _, err := s.Create(t.Context(), svcAdmin, provider("https://a.example.com", true), nil)
	if err != nil || !a.IsDefault || a.GroupsClaim != "groups" {
		t.Fatalf("a = %+v %v", a, err)
	}
	b, _, err := s.Create(t.Context(), svcAdmin, provider("https://b.example.com", true), nil)
	if err != nil || !b.IsDefault {
		t.Fatalf("b = %+v %v", b, err)
	}
	if r.rows[a.ID].IsDefault {
		t.Error("the previous default was not cleared")
	}
	if len(audit.Entries) != 2 {
		t.Errorf("audit = %d", len(audit.Entries))
	}
	if _, _, err := s.Create(t.Context(), svcAdmin, provider("http://insecure.example.com", false), nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("http issuer = %v", err)
	}
	rows, next, err := s.List(t.Context(), svcAdmin, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" {
		t.Errorf("list = %v %q %v", rows, next, err)
	}
}

func TestUpdateAndInUseRules(t *testing.T) {
	s, r, _ := rig()
	p, _, _ := s.Create(t.Context(), svcAdmin, provider("https://a.example.com", false), nil)
	name := "Renamed"
	out, err := s.Update(t.Context(), svcAdmin, p.ID, 1, idp.Patch{Name: &name})
	if err != nil || out.Name != name || out.Version != 2 {
		t.Fatalf("update = %+v %v", out, err)
	}
	if _, err := s.Update(t.Context(), svcAdmin, p.ID, 1, idp.Patch{Name: &name}); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale = %v", err)
	}
	arn := domain.OptionalString{Set: true}
	if out, err = s.Update(t.Context(), svcAdmin, p.ID, 2, idp.Patch{ClientSecretARN: &arn}); err != nil || out.ClientSecretARN != "" {
		t.Errorf("clear arn = %+v %v", out, err)
	}
	r.teams[p.ID] = []idp.TeamRef{{ID: "team_1", Slug: "a", DisplayName: "A"}}
	off := false
	err = func() error { _, e := s.Update(t.Context(), svcAdmin, p.ID, 3, idp.Patch{Enabled: &off}); return e }()
	var de *domain.Error
	if domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("disable in use = %v", err)
	}
	_ = de
	if err := s.Delete(t.Context(), svcAdmin, p.ID, 3); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("delete in use = %v", err)
	}
	got, teams, err := s.Get(t.Context(), svcAdmin, p.ID)
	if err != nil || got.ID != p.ID || len(teams) != 1 {
		t.Errorf("get = %+v %v %v", got, teams, err)
	}
	delete(r.teams, p.ID)
	if err := s.Delete(t.Context(), svcAdmin, p.ID, 3); err != nil {
		t.Errorf("delete = %v", err)
	}
	if _, _, err := s.Get(t.Context(), svcAdmin, p.ID); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("get after delete = %v", err)
	}
}

func TestDeniedForTeamAdmins(t *testing.T) {
	s, _, audit := rig()
	ctx := t.Context()
	checks := []error{
		func() error { _, _, e := s.List(ctx, teamAdm, acting.Page{}); return e }(),
		func() error { _, _, e := s.Get(ctx, teamAdm, "idp_1"); return e }(),
		func() error {
			_, _, e := s.Create(ctx, teamAdm, provider("https://a.example.com", false), nil)
			return e
		}(),
		func() error { _, e := s.Update(ctx, teamAdm, "idp_1", 1, idp.Patch{}); return e }(),
		s.Delete(ctx, teamAdm, "idp_1", 1),
	}
	for i, err := range checks {
		if domain.KindOf(err) != domain.KindAccessDenied {
			t.Errorf("call %d = %v", i, err)
		}
	}
	if len(audit.Entries) != len(checks) {
		t.Errorf("denied audit = %d", len(audit.Entries))
	}
}

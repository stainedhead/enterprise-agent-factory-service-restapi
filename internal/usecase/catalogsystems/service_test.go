package catalogsystems_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/catalogsystems"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

type repo struct {
	rows map[string]domain.ExternalSystem
}

func (r *repo) List(_ context.Context, _ port.Tx, after string, limit int) ([]domain.ExternalSystem, error) {
	var out []domain.ExternalSystem
	for _, x := range r.rows {
		if x.ID > after {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) Get(_ context.Context, _ port.Tx, id string, _ bool) (domain.ExternalSystem, error) {
	x, ok := r.rows[id]
	if !ok {
		return domain.ExternalSystem{}, domain.NotFound("external system")
	}
	return x, nil
}

func (r *repo) Insert(_ context.Context, _ port.Tx, s domain.ExternalSystem) error {
	for _, x := range r.rows {
		if x.Slug == s.Slug {
			return domain.Conflict("duplicate", "slug taken", nil)
		}
	}
	s.Version = 1
	r.rows[s.ID] = s
	return nil
}

func (r *repo) Update(_ context.Context, _ port.Tx, s domain.ExternalSystem, expect domain.Version) (domain.ExternalSystem, error) {
	s.Version = expect + 1
	r.rows[s.ID] = s
	return s, nil
}

func (r *repo) Delete(_ context.Context, _ port.Tx, id string, _ domain.Version) error {
	delete(r.rows, id)
	return nil
}

var (
	svcAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true}}
	teamAdm  = acting.Caller{Principal: authz.Principal{ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: "team_1"}}}}
	unbound  = acting.Caller{Principal: authz.Principal{ID: "usr_u", Type: authz.PrincipalUser}}
)

func rig() (*catalogsystems.Service, *repo, *fakes.Audit) {
	a := &fakes.Audit{}
	r := &repo{rows: map[string]domain.ExternalSystem{}}
	return &catalogsystems.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Outbox: &fakes.Outbox{}, Idem: &fakes.Idempotency{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: r}, r, a
}

func params(slug string) domain.NewCatalogSystemParams {
	return domain.NewCatalogSystemParams{Slug: slug, Name: "Sys", Category: domain.ExtOther, Endpoints: []string{"api.example.com"}}
}

func TestCreateGetUpdateDelete(t *testing.T) {
	s, r, audit := rig()
	ctx := t.Context()
	res, err := s.Create(ctx, svcAdmin, params("wiki"), nil)
	if err != nil || res.System.ID == "" || res.System.Version != 1 || !res.System.Enabled || res.System.TeamID != "" {
		t.Fatalf("create = %+v %v", res, err)
	}
	id := res.System.ID
	if _, err := s.Create(ctx, svcAdmin, params("wiki"), nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("duplicate = %v", err)
	}
	if _, err := s.Create(ctx, svcAdmin, domain.NewCatalogSystemParams{Slug: "Bad Slug"}, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("invalid = %v", err)
	}
	if got, err := s.Get(ctx, svcAdmin, id); err != nil || got.Slug != "wiki" {
		t.Fatalf("get = %+v %v", got, err)
	}
	if _, err := s.Get(ctx, svcAdmin, "ext_absent"); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("get absent = %v", err)
	}
	off, name := false, "Renamed"
	got, err := s.Update(ctx, svcAdmin, id, domain.SystemUpdate{Name: &name, Enabled: &off}, 1)
	if err != nil || got.Name != name || got.Enabled || got.Version != 2 {
		t.Fatalf("update = %+v %v", got, err)
	}
	if _, err := s.Update(ctx, svcAdmin, id, domain.SystemUpdate{Name: &name}, 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale update = %v", err)
	}
	if _, err := s.Update(ctx, svcAdmin, id, domain.SystemUpdate{}, 2); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("empty update = %v", err)
	}
	if _, err := s.Update(ctx, svcAdmin, "ext_absent", domain.SystemUpdate{Name: &name}, 1); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("update absent = %v", err)
	}
	if err := s.Delete(ctx, svcAdmin, id, 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale delete = %v", err)
	}
	if err := s.Delete(ctx, svcAdmin, id, 2); err != nil || len(r.rows) != 0 {
		t.Errorf("delete = %v, rows %d", err, len(r.rows))
	}
	if err := s.Delete(ctx, svcAdmin, id, 2); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("delete absent = %v", err)
	}
	if len(audit.Entries) != 3 {
		t.Errorf("audit = %d, want create, update, delete", len(audit.Entries))
	}
}

func TestBuiltinCannotBeDeleted(t *testing.T) {
	s, r, _ := rig()
	r.rows["ext_1"] = domain.ExternalSystem{ID: "ext_1", Slug: "github", Builtin: true, Enabled: true, Version: 3}
	if err := s.Delete(t.Context(), svcAdmin, "ext_1", 3); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("builtin = %v", err)
	}
	off := false
	if got, err := s.Update(t.Context(), svcAdmin, "ext_1", domain.SystemUpdate{Enabled: &off}, 3); err != nil || got.Enabled {
		t.Errorf("a built-in system can be switched off: %+v %v", got, err)
	}
}

func TestList(t *testing.T) {
	s, _, _ := rig()
	for _, slug := range []string{"one", "two", "three"} {
		if _, err := s.Create(t.Context(), svcAdmin, params(slug), nil); err != nil {
			t.Fatal(err)
		}
	}
	page, next, err := s.List(t.Context(), svcAdmin, acting.Page{Limit: 2})
	if err != nil || len(page) != 2 || next == "" {
		t.Fatalf("page 1 = %d %q %v", len(page), next, err)
	}
	if rest, next, err := s.List(t.Context(), svcAdmin, acting.Page{Limit: 2, Cursor: next}); err != nil || len(rest) != 1 || next != "" {
		t.Fatalf("page 2 = %d %q %v", len(rest), next, err)
	}
}

func TestDenied(t *testing.T) {
	s, r, audit := rig()
	ctx := t.Context()
	r.rows["ext_1"] = domain.ExternalSystem{ID: "ext_1", Slug: "x", Version: 1}
	name := "n"
	calls := map[string]func(acting.Caller) error{
		"List":   func(c acting.Caller) error { _, _, err := s.List(ctx, c, acting.Page{}); return err },
		"Get":    func(c acting.Caller) error { _, err := s.Get(ctx, c, "ext_1"); return err },
		"Create": func(c acting.Caller) error { _, err := s.Create(ctx, c, params("new"), nil); return err },
		"Update": func(c acting.Caller) error {
			_, err := s.Update(ctx, c, "ext_1", domain.SystemUpdate{Name: &name}, 1)
			return err
		},
		"Delete": func(c acting.Caller) error { return s.Delete(ctx, c, "ext_1", 1) },
	}
	for name, call := range calls {
		for who, c := range map[string]acting.Caller{"team admin": teamAdm, "unbound": unbound} {
			if err := call(c); domain.KindOf(err) != domain.KindAccessDenied {
				t.Errorf("%s as %s = %v", name, who, err)
			}
		}
	}
	if len(r.rows) != 1 || len(audit.Entries) != 10 {
		t.Errorf("rows %d, denied audit %d", len(r.rows), len(audit.Entries))
	}
}

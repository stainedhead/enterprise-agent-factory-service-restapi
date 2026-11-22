package storageclasses_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/storageclasses"
)

type repo struct {
	rows  map[string]domain.StorageClass
	inUse map[string]bool
}

func (r *repo) List(_ context.Context, _ port.Tx, enabledOnly bool, after string, limit int) ([]domain.StorageClass, error) {
	var out []domain.StorageClass
	for _, c := range r.rows {
		if c.Name > after && (!enabledOnly || c.Enabled) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) Get(_ context.Context, _ port.Tx, name string, _ bool) (domain.StorageClass, error) {
	c, ok := r.rows[name]
	if !ok {
		return domain.StorageClass{}, domain.NotFound("storage class")
	}
	return c, nil
}

func (r *repo) Insert(_ context.Context, _ port.Tx, c domain.StorageClass) error {
	c.Version = 1
	r.rows[c.Name] = c
	return nil
}

func (r *repo) Update(_ context.Context, _ port.Tx, c domain.StorageClass, expect domain.Version) (domain.StorageClass, error) {
	if r.rows[c.Name].Version != expect {
		return domain.StorageClass{}, domain.PreconditionFailed()
	}
	c.Version = expect + 1
	r.rows[c.Name] = c
	return c, nil
}

func (r *repo) Delete(_ context.Context, _ port.Tx, name string, _ domain.Version) error {
	if r.inUse[name] {
		return domain.Conflict("dependents_exist", "in use", nil)
	}
	delete(r.rows, name)
	return nil
}

var (
	svcAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true}}
	teamAdm  = acting.Caller{Principal: authz.Principal{ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: "team_1"}}}}
	unbound  = acting.Caller{Principal: authz.Principal{ID: "usr_u", Type: authz.PrincipalUser}}
)

func rig() (*storageclasses.Service, *repo, *fakes.Audit) {
	a := &fakes.Audit{}
	r := &repo{rows: map[string]domain.StorageClass{}, inUse: map[string]bool{}}
	return &storageclasses.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Outbox: &fakes.Outbox{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: r}, r, a
}

func class(name string) domain.StorageClass {
	return domain.StorageClass{Name: name, Description: "d", VolumeType: domain.VolumeTypeGP3, Enabled: true}
}

func ver(v domain.Version) *domain.Version { return &v }

func TestPutUpsertAndConcurrency(t *testing.T) {
	s, _, audit := rig()
	ctx := t.Context()
	got, err := s.Put(ctx, svcAdmin, class("fast"), nil)
	if err != nil || got.Version != 1 {
		t.Fatalf("create = %+v %v", got, err)
	}
	if _, err := s.Put(ctx, svcAdmin, class("slow"), ver(1)); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("If-Match on a new class = %v", err)
	}
	upd := class("fast")
	upd.VolumeType = domain.VolumeTypeIO2
	if got, err = s.Put(ctx, svcAdmin, upd, ver(1)); err != nil || got.Version != 2 || got.VolumeType != domain.VolumeTypeIO2 {
		t.Fatalf("update = %+v %v", got, err)
	}
	if _, err := s.Put(ctx, svcAdmin, upd, ver(1)); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale = %v", err)
	}
	if _, err := s.Put(ctx, svcAdmin, upd, nil); err != nil {
		t.Errorf("upsert without If-Match = %v", err)
	}
	bad := class("Bad Name")
	if _, err := s.Put(ctx, svcAdmin, bad, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("invalid = %v", err)
	}
	if len(audit.Entries) != 3 {
		t.Errorf("audit = %d, want 3", len(audit.Entries))
	}
}

func TestDelete(t *testing.T) {
	s, r, _ := rig()
	ctx := t.Context()
	for _, n := range []string{"a", "b"} {
		if _, err := s.Put(ctx, svcAdmin, class(n), nil); err != nil {
			t.Fatal(err)
		}
	}
	r.inUse["b"] = true
	for _, tc := range []struct {
		name  string
		class string
		v     domain.Version
		want  domain.ErrorKind
	}{
		{"stale", "a", 9, domain.KindPreconditionFailed},
		{"absent", "zz", 1, domain.KindNotFound},
		{"in use", "b", 1, domain.KindConflict},
		{"deleted", "a", 1, ""},
	} {
		if err := s.Delete(ctx, svcAdmin, tc.class, tc.v); domain.KindOf(err) != tc.want {
			t.Errorf("%s = %v, want %q", tc.name, err, tc.want)
		}
	}
	if _, ok := r.rows["a"]; ok {
		t.Error("a still stored")
	}
}

func TestLists(t *testing.T) {
	s, _, _ := rig()
	ctx := t.Context()
	off := class("c-off")
	off.Enabled = false
	for _, c := range []domain.StorageClass{class("a"), class("b"), off} {
		if _, err := s.Put(ctx, svcAdmin, c, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, err := s.ListEnabled(ctx, teamAdm, acting.Page{}); err != nil || len(got) != 2 {
		t.Fatalf("enabled = %d %v", len(got), err)
	}
	page, next, err := s.ListAll(ctx, svcAdmin, acting.Page{Limit: 2})
	if err != nil || len(page) != 2 || next == "" {
		t.Fatalf("page 1 = %d %q %v", len(page), next, err)
	}
	if rest, next, err := s.ListAll(ctx, svcAdmin, acting.Page{Limit: 2, Cursor: next}); err != nil || len(rest) != 1 || next != "" {
		t.Fatalf("page 2 = %d %q %v", len(rest), next, err)
	}
}

func TestDenied(t *testing.T) {
	s, r, audit := rig()
	ctx := t.Context()
	r.rows["a"] = class("a")
	calls := map[string]func(acting.Caller) error{
		"ListAll": func(c acting.Caller) error { _, _, err := s.ListAll(ctx, c, acting.Page{}); return err },
		"Put":     func(c acting.Caller) error { _, err := s.Put(ctx, c, class("x"), nil); return err },
		"Delete":  func(c acting.Caller) error { return s.Delete(ctx, c, "a", 1) },
	}
	for name, call := range calls {
		for who, c := range map[string]acting.Caller{"team admin": teamAdm, "unbound": unbound} {
			if err := call(c); domain.KindOf(err) != domain.KindAccessDenied {
				t.Errorf("%s as %s = %v", name, who, err)
			}
		}
	}
	if len(r.rows) != 1 || len(audit.Entries) != 6 {
		t.Errorf("rows %d, denied audit %d", len(r.rows), len(audit.Entries))
	}
}

func TestRepositoryFailures(t *testing.T) {
	ops := map[string]func(*storageclasses.Service) error{
		"ListEnabled": func(s *storageclasses.Service) error {
			_, _, err := s.ListEnabled(t.Context(), teamAdm, acting.Page{})
			return err
		},
		"ListAll": func(s *storageclasses.Service) error {
			_, _, err := s.ListAll(t.Context(), svcAdmin, acting.Page{})
			return err
		},
		"PutNew": func(s *storageclasses.Service) error {
			_, err := s.Put(t.Context(), svcAdmin, class("new"), nil)
			return err
		},
		"PutExisting": func(s *storageclasses.Service) error {
			_, err := s.Put(t.Context(), svcAdmin, class("fast"), ver(1))
			return err
		},
		"Delete": func(s *storageclasses.Service) error { return s.Delete(t.Context(), svcAdmin, "fast", 1) },
	}
	for name, op := range ops {
		for n := 1; n < 10; n++ {
			r := &repo{rows: map[string]domain.StorageClass{"fast": {Name: "fast", VolumeType: domain.VolumeTypeGP3, Enabled: true, Version: 1}}, inUse: map[string]bool{}}
			f, audit := &flaky{repo: r, failAt: n}, &fakes.Audit{}
			s := &storageclasses.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: audit, Outbox: &fakes.Outbox{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: f}
			err := op(s)
			if f.calls < n {
				break
			}
			if err == nil || len(audit.Entries) != 0 {
				t.Errorf("%s failing at repository call %d: err %v, audit %d", name, n, err, len(audit.Entries))
			}
		}
	}
}

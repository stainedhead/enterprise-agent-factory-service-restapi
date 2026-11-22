package agenttemplates_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agenttemplates"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	digA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type repo struct {
	rows  map[string]domain.AgentTemplate
	inUse map[string]bool
}

func (r *repo) List(_ context.Context, _ port.Tx, enabledOnly bool, after string, limit int) ([]domain.AgentTemplate, error) {
	var out []domain.AgentTemplate
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

func (r *repo) Get(_ context.Context, _ port.Tx, name string, _ bool) (domain.AgentTemplate, error) {
	c, ok := r.rows[name]
	if !ok {
		return domain.AgentTemplate{}, domain.NotFound("agent template")
	}
	return c, nil
}

func (r *repo) Insert(_ context.Context, _ port.Tx, t domain.AgentTemplate) error {
	t.Version = 1
	r.rows[t.Name] = t
	return nil
}

func (r *repo) Update(_ context.Context, _ port.Tx, t domain.AgentTemplate, expect domain.Version) (domain.AgentTemplate, error) {
	if r.rows[t.Name].Version != expect {
		return domain.AgentTemplate{}, domain.PreconditionFailed()
	}
	t.Version = expect + 1
	r.rows[t.Name] = t
	return t, nil
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

func rig() (*agenttemplates.Service, *repo, *fakes.Audit) {
	a := &fakes.Audit{}
	r := &repo{rows: map[string]domain.AgentTemplate{}, inUse: map[string]bool{}}
	return &agenttemplates.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Outbox: &fakes.Outbox{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: r}, r, a
}

func params(name string) domain.PutAgentTemplateParams {
	return domain.PutAgentTemplateParams{Name: name, Description: "d", Harness: "hermes", DefaultResources: domain.Resources{CPU: "500m", Memory: "1Gi"},
		AllowedDigests: []string{digA, digB}, CurrentDigest: digA}
}

func ver(v domain.Version) *domain.Version { return &v }

func TestPut(t *testing.T) {
	s, _, audit := rig()
	ctx := t.Context()
	got, err := s.Put(ctx, svcAdmin, params("omp"), nil)
	if err != nil || got.Version != 1 || got.ID == "" || !got.Enabled {
		t.Fatalf("create = %+v %v", got, err)
	}
	if _, err := s.Put(ctx, svcAdmin, params("other"), ver(1)); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("If-Match on a new template = %v", err)
	}
	next := params("omp")
	next.CurrentDigest = digB
	upd, err := s.Put(ctx, svcAdmin, next, ver(1))
	if err != nil || upd.Version != 2 || upd.CurrentDigest != digB || upd.ID != got.ID {
		t.Fatalf("update = %+v %v", upd, err)
	}
	if _, err := s.Put(ctx, svcAdmin, next, ver(1)); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale = %v", err)
	}
	outside := params("omp")
	outside.CurrentDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	bad := params("Bad Name")
	empty := params("omp")
	empty.AllowedDigests = nil
	for name, in := range map[string]domain.PutAgentTemplateParams{"current outside allowed": outside, "bad name": bad, "no digests": empty} {
		if _, err := s.Put(ctx, svcAdmin, in, nil); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s = %v", name, err)
		}
	}
	if len(audit.Entries) != 2 {
		t.Errorf("audit = %d, want 2", len(audit.Entries))
	}
}

func TestDelete(t *testing.T) {
	s, r, _ := rig()
	ctx := t.Context()
	for _, n := range []string{"aaa", "bbb"} {
		if _, err := s.Put(ctx, svcAdmin, params(n), nil); err != nil {
			t.Fatal(err)
		}
	}
	r.inUse["bbb"] = true
	for _, tc := range []struct {
		name string
		tpl  string
		v    domain.Version
		want domain.ErrorKind
	}{
		{"stale", "aaa", 7, domain.KindPreconditionFailed},
		{"absent", "zzz", 1, domain.KindNotFound},
		{"in use", "bbb", 1, domain.KindConflict},
		{"deleted", "aaa", 1, ""},
	} {
		if err := s.Delete(ctx, svcAdmin, tc.tpl, tc.v); domain.KindOf(err) != tc.want {
			t.Errorf("%s = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestLists(t *testing.T) {
	s, _, _ := rig()
	ctx := t.Context()
	off := false
	for _, n := range []string{"aaa", "bbb", "ccc"} {
		p := params(n)
		if n == "ccc" {
			p.Enabled = &off
		}
		if _, err := s.Put(ctx, svcAdmin, p, nil); err != nil {
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
	if _, err := s.Put(ctx, svcAdmin, params("aaa"), nil); err != nil {
		t.Fatal(err)
	}
	calls := map[string]func(acting.Caller) error{
		"ListAll": func(c acting.Caller) error { _, _, err := s.ListAll(ctx, c, acting.Page{}); return err },
		"Put":     func(c acting.Caller) error { _, err := s.Put(ctx, c, params("xxx"), nil); return err },
		"Delete":  func(c acting.Caller) error { return s.Delete(ctx, c, "aaa", 1) },
	}
	before := len(audit.Entries)
	for name, call := range calls {
		for who, c := range map[string]acting.Caller{"team admin": teamAdm, "unbound": unbound} {
			if err := call(c); domain.KindOf(err) != domain.KindAccessDenied {
				t.Errorf("%s as %s = %v", name, who, err)
			}
		}
	}
	if len(r.rows) != 1 || len(audit.Entries)-before != 6 {
		t.Errorf("rows %d, denied audit %d", len(r.rows), len(audit.Entries)-before)
	}
}

func TestRepositoryFailures(t *testing.T) {
	ops := map[string]func(*agenttemplates.Service) error{
		"ListEnabled": func(s *agenttemplates.Service) error {
			_, _, err := s.ListEnabled(t.Context(), teamAdm, acting.Page{})
			return err
		},
		"ListAll": func(s *agenttemplates.Service) error {
			_, _, err := s.ListAll(t.Context(), svcAdmin, acting.Page{})
			return err
		},
		"PutNew": func(s *agenttemplates.Service) error {
			_, err := s.Put(t.Context(), svcAdmin, params("new"), nil)
			return err
		},
		"PutExisting": func(s *agenttemplates.Service) error {
			_, err := s.Put(t.Context(), svcAdmin, params("omp"), ver(1))
			return err
		},
		"Delete": func(s *agenttemplates.Service) error { return s.Delete(t.Context(), svcAdmin, "omp", 1) },
	}
	for name, op := range ops {
		for n := 1; n < 10; n++ {
			r := &repo{rows: map[string]domain.AgentTemplate{"omp": {ID: "tpl_1", Name: "omp", Harness: "hermes", Enabled: true, Version: 1}}, inUse: map[string]bool{}}
			f, audit := &flaky{repo: r, failAt: n}, &fakes.Audit{}
			s := &agenttemplates.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: audit, Outbox: &fakes.Outbox{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: f}
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

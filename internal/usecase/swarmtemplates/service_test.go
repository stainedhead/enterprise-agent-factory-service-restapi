package swarmtemplates_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/swarmtemplates"
)

type repo struct {
	rows map[string]domain.SwarmTemplate
	open map[string]int
}

func (r *repo) List(_ context.Context, _ port.Tx, after string, limit int) ([]domain.SwarmTemplate, error) {
	var out []domain.SwarmTemplate
	for _, t := range r.rows {
		if string(t.Name) > after {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) Get(_ context.Context, _ port.Tx, id string, _ bool) (domain.SwarmTemplate, error) {
	t, ok := r.rows[id]
	if !ok {
		return domain.SwarmTemplate{}, domain.NotFound("swarm template")
	}
	return t, nil
}

func (r *repo) Insert(_ context.Context, _ port.Tx, t domain.SwarmTemplate) error {
	for _, o := range r.rows {
		if o.Name == t.Name {
			return domain.Conflict("duplicate", "name taken", nil)
		}
	}
	r.rows[t.ID] = t
	return nil
}

func (r *repo) Update(_ context.Context, _ port.Tx, t domain.SwarmTemplate, expect domain.Version) (domain.SwarmTemplate, error) {
	if r.rows[t.ID].Version != expect {
		return domain.SwarmTemplate{}, domain.PreconditionFailed()
	}
	t.Version = expect + 1
	r.rows[t.ID] = t
	return t, nil
}

func (r *repo) Delete(_ context.Context, _ port.Tx, id string, _ domain.Version) error {
	delete(r.rows, id)
	return nil
}

func (r *repo) OpenWorkOrders(_ context.Context, _ port.Tx, id string) (int, error) {
	return r.open[id], nil
}

var (
	svcAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true}}
	secOp    = acting.Caller{Principal: authz.Principal{ID: "key_o", Type: authz.PrincipalAPIKey, SecurityOperator: true}}
	teamAdm  = acting.Caller{Principal: authz.Principal{ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: "team_1"}}}}
)

func rig() (*swarmtemplates.Service, *repo, *fakes.Audit, *fakes.Outbox) {
	a, o := &fakes.Audit{}, &fakes.Outbox{}
	r := &repo{rows: map[string]domain.SwarmTemplate{}, open: map[string]int{}}
	return &swarmtemplates.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Outbox: o, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: r}, r, a, o
}

func params(name string) domain.NewSwarmTemplateParams {
	img := "registry.example.com/a@sha256:" + strings.Repeat("a", 64)
	return domain.NewSwarmTemplateParams{Name: name, Kind: "cve_remediation", BudgetUSD: "100.00", TTLHours: 24, TeardownRule: "at_work_complete",
		Spec: map[string]any{"stages": []any{}, "pools": []any{map[string]any{"name": "fixer", "template": "coder", "image": img, "modelId": "m"}}}}
}

func TestCreateAndRead(t *testing.T) {
	s, _, audit, outbox := rig()
	ctx := t.Context()
	got, err := s.Create(ctx, svcAdmin, params("cve-fix"), nil)
	if err != nil || got.Template.ID == "" || !strings.HasPrefix(got.Template.ID, "swt_") || got.Template.Version != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.Create(ctx, svcAdmin, params("cve-fix"), nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("duplicate name = %v", err)
	}
	if _, err := s.Create(ctx, svcAdmin, params("Bad"), nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("invalid = %v", err)
	}
	if len(audit.Entries) != 1 || audit.Entries[0].Record.Action != "CreateSwarmTemplate" || len(outbox.Events) != 0 {
		t.Errorf("audit %v outbox %v", audit.Entries, outbox.Events)
	}
	// Readers: service admin and security operator read; a team admin is denied.
	for name, c := range map[string]acting.Caller{"admin": svcAdmin, "secop": secOp} {
		if l, _, err := s.List(ctx, c, acting.Page{}); err != nil || len(l) != 1 {
			t.Errorf("%s list = %v %v", name, l, err)
		}
		if _, err := s.Get(ctx, c, got.Template.ID); err != nil {
			t.Errorf("%s get = %v", name, err)
		}
	}
	if _, err := s.Get(ctx, svcAdmin, "swt_nope"); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown = %v", err)
	}
	if _, _, err := s.List(ctx, teamAdm, acting.Page{}); domain.KindOf(err) != domain.KindAccessDenied {
		t.Errorf("team admin list = %v", err)
	}
}

func TestWritesAreServiceAdminOnly(t *testing.T) {
	s, _, audit, _ := rig()
	ctx := t.Context()
	got, _ := s.Create(ctx, svcAdmin, params("cve-fix"), nil)
	id := got.Template.ID
	for name, c := range map[string]acting.Caller{"secop": secOp, "team admin": teamAdm} {
		calls := map[string]func() error{
			"create": func() error { _, err := s.Create(ctx, c, params("other-one"), nil); return err },
			"update": func() error {
				d := "x"
				_, err := s.Update(ctx, c, id, 1, domain.SwarmTemplatePatch{Description: &d})
				return err
			},
			"enable":  func() error { _, err := s.Enable(ctx, c, id, nil); return err },
			"disable": func() error { _, err := s.Disable(ctx, c, id, nil); return err },
			"delete":  func() error { return s.Delete(ctx, c, id, 1) },
		}
		for op, f := range calls {
			if err := f(); domain.KindOf(err) != domain.KindAccessDenied {
				t.Errorf("%s %s = %v, want AccessDenied", name, op, err)
			}
		}
	}
	denied := 0
	for _, e := range audit.Entries {
		if e.Own {
			denied++
		}
	}
	if denied != 10 {
		t.Errorf("denied audit rows = %d, want 10", denied)
	}
}

func TestUpdateEnableDisableDelete(t *testing.T) {
	s, r, _, _ := rig()
	ctx := t.Context()
	got, _ := s.Create(ctx, svcAdmin, params("cve-fix"), nil)
	id := got.Template.ID
	d := "new"
	if _, err := s.Update(ctx, svcAdmin, id, 1, domain.SwarmTemplatePatch{}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("empty patch = %v", err)
	}
	u, err := s.Update(ctx, svcAdmin, id, 1, domain.SwarmTemplatePatch{Description: &d})
	if err != nil || u.Version != 2 || u.Description != "new" {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := s.Update(ctx, svcAdmin, id, 1, domain.SwarmTemplatePatch{Description: &d}); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale = %v", err)
	}
	bad := "-1"
	if _, err := s.Update(ctx, svcAdmin, id, 2, domain.SwarmTemplatePatch{BudgetUSD: &bad}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("bad budget = %v", err)
	}
	dis, err := s.Disable(ctx, svcAdmin, id, nil)
	if err != nil || dis.Template.Enabled || dis.Template.Version != 3 {
		t.Fatalf("disable %+v %v", dis, err)
	}
	if again, err := s.Disable(ctx, svcAdmin, id, nil); err != nil || again.Template.Version != 3 {
		t.Errorf("disable twice changes nothing: %+v %v", again, err)
	}
	if en, err := s.Enable(ctx, svcAdmin, id, nil); err != nil || !en.Template.Enabled {
		t.Errorf("enable %+v %v", en, err)
	}
	if _, err := s.Enable(ctx, svcAdmin, "swt_nope", nil); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("enable unknown = %v", err)
	}

	r.open[id] = 2
	if err := s.Delete(ctx, svcAdmin, id, 4); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("delete while open work orders = %v", err)
	}
	r.open[id] = 0
	if err := s.Delete(ctx, svcAdmin, id, 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("delete stale = %v", err)
	}
	if err := s.Delete(ctx, svcAdmin, id, 4); err != nil {
		t.Errorf("delete = %v", err)
	}
	if err := s.Delete(ctx, svcAdmin, id, 4); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("delete again = %v", err)
	}
}

func TestIdempotentVerb(t *testing.T) {
	s, _, audit, _ := rig()
	ctx := t.Context()
	got, _ := s.Create(ctx, svcAdmin, params("cve-fix"), nil)
	n := len(audit.Entries)
	mk := func(sum byte) *acting.Idem {
		return &acting.Idem{Key: port.IdempotencyKey{Key: "k1", Operation: "DisableSwarmTemplate", RequestSHA256: [32]byte{sum}},
			Render: func(v any) (int, json.RawMessage, error) { return 200, json.RawMessage(`{"ok":true}`), nil }}
	}
	if r, err := s.Disable(ctx, svcAdmin, got.Template.ID, mk(1)); err != nil || r.Replay != nil || r.Template.Enabled {
		t.Fatalf("first %+v %v", r, err)
	}
	r, err := s.Disable(ctx, svcAdmin, got.Template.ID, mk(1))
	if err != nil || r.Replay == nil {
		t.Fatalf("replay %+v %v", r, err)
	}
	if _, err := s.Disable(ctx, svcAdmin, got.Template.ID, mk(2)); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("mismatch = %v", err)
	}
	if len(audit.Entries) != n+1 {
		t.Errorf("a replay writes no audit row: %d", len(audit.Entries)-n)
	}
	// create replay
	ci := &acting.Idem{Key: port.IdempotencyKey{Key: "c1", Operation: "CreateSwarmTemplate"}, Render: func(any) (int, json.RawMessage, error) { return 201, json.RawMessage(`{}`), nil }}
	if _, err := s.Create(ctx, svcAdmin, params("second-one"), ci); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Create(ctx, svcAdmin, params("second-one"), ci); err != nil || r.Replay == nil {
		t.Errorf("create replay %+v %v", r, err)
	}
}

func TestPaging(t *testing.T) {
	s, _, _, _ := rig()
	for _, n := range []string{"aaa-one", "bbb-two", "ccc-three"} {
		if _, err := s.Create(t.Context(), svcAdmin, params(n), nil); err != nil {
			t.Fatal(err)
		}
	}
	p1, next, err := s.List(t.Context(), svcAdmin, acting.Page{Limit: 2})
	if err != nil || len(p1) != 2 || next != "bbb-two" {
		t.Fatalf("%v %q %v", p1, next, err)
	}
	p2, next, _ := s.List(t.Context(), svcAdmin, acting.Page{Limit: 2, Cursor: next})
	if len(p2) != 1 || next != "" {
		t.Errorf("%v %q", p2, next)
	}
}

// flaky fails one named repository method so each storage error path is exercised.
type flaky struct {
	swarmtemplates.Repository
	fail string
}

var errStore = errors.New("store down")

func (f flaky) Get(ctx context.Context, tx port.Tx, id string, up bool) (domain.SwarmTemplate, error) {
	if f.fail == "Get" {
		return domain.SwarmTemplate{}, errStore
	}
	return f.Repository.Get(ctx, tx, id, up)
}
func (f flaky) Insert(ctx context.Context, tx port.Tx, t domain.SwarmTemplate) error {
	if f.fail == "Insert" {
		return errStore
	}
	return f.Repository.Insert(ctx, tx, t)
}
func (f flaky) Update(ctx context.Context, tx port.Tx, t domain.SwarmTemplate, e domain.Version) (domain.SwarmTemplate, error) {
	if f.fail == "Update" {
		return t, errStore
	}
	return f.Repository.Update(ctx, tx, t, e)
}
func (f flaky) Delete(ctx context.Context, tx port.Tx, id string, e domain.Version) error {
	if f.fail == "Delete" {
		return errStore
	}
	return f.Repository.Delete(ctx, tx, id, e)
}
func (f flaky) OpenWorkOrders(ctx context.Context, tx port.Tx, id string) (int, error) {
	if f.fail == "OpenWorkOrders" {
		return 0, errStore
	}
	return f.Repository.OpenWorkOrders(ctx, tx, id)
}

func TestStorageErrorsSurface(t *testing.T) {
	d := "x"
	ops := map[string]func(s *swarmtemplates.Service, id string) error{
		"get": func(s *swarmtemplates.Service, id string) error { _, err := s.Get(t.Context(), secOp, id); return err },
		"create": func(s *swarmtemplates.Service, _ string) error {
			_, err := s.Create(t.Context(), svcAdmin, params("new-one"), nil)
			return err
		},
		"update": func(s *swarmtemplates.Service, id string) error {
			_, err := s.Update(t.Context(), svcAdmin, id, 1, domain.SwarmTemplatePatch{Description: &d})
			return err
		},
		"disable": func(s *swarmtemplates.Service, id string) error {
			_, err := s.Disable(t.Context(), svcAdmin, id, nil)
			return err
		},
		"delete": func(s *swarmtemplates.Service, id string) error { return s.Delete(t.Context(), svcAdmin, id, 1) },
	}
	for _, m := range []string{"Get", "Insert", "Update", "Delete", "OpenWorkOrders"} {
		for name, op := range ops {
			t.Run(m+"/"+name, func(t *testing.T) {
				s, r, _, _ := rig()
				got, err := s.Create(t.Context(), svcAdmin, params("cve-fix"), nil)
				if err != nil {
					t.Fatal(err)
				}
				_ = r
				s.Repo = flaky{Repository: s.Repo, fail: m}
				if err := op(s, got.Template.ID); err != nil && !errors.Is(err, errStore) && domain.KindOf(err) == "" {
					t.Errorf("%v", err)
				}
			})
		}
	}
}

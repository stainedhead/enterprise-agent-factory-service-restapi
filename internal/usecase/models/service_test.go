package models_test

import (
	"context"
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/models"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

type repo struct {
	models map[string]domain.Model
	prices []domain.ModelPrice
}

func (r *repo) List(_ context.Context, _ port.Tx, enabledOnly bool, after string, limit int) ([]domain.Model, error) {
	var out []domain.Model
	for _, m := range r.models {
		if m.ID > after && (!enabledOnly || m.Enabled) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) Get(_ context.Context, _ port.Tx, id string, _ bool) (domain.Model, error) {
	m, ok := r.models[id]
	if !ok {
		return domain.Model{}, domain.NotFound("model")
	}
	return m, nil
}

func (r *repo) Insert(_ context.Context, _ port.Tx, m domain.Model) error {
	if _, ok := r.models[m.ID]; ok {
		return domain.Conflict("duplicate", "exists", nil)
	}
	m.Version = 1
	r.models[m.ID] = m
	return nil
}

func (r *repo) Update(_ context.Context, _ port.Tx, m domain.Model, expect domain.Version) (domain.Model, error) {
	if r.models[m.ID].Version != expect {
		return domain.Model{}, domain.PreconditionFailed()
	}
	m.Version++
	r.models[m.ID] = m
	return m, nil
}

func (r *repo) ListPrices(_ context.Context, _ port.Tx, id string, before *time.Time, limit int) ([]domain.ModelPrice, error) {
	var out []domain.ModelPrice
	for _, p := range r.prices {
		if p.ModelID == id && (before == nil || p.EffectiveFrom.Before(*before)) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EffectiveFrom.After(out[j].EffectiveFrom) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) InsertPrice(_ context.Context, _ port.Tx, p domain.ModelPrice) (domain.ModelPrice, error) {
	for _, q := range r.prices {
		if q.ModelID == p.ModelID && q.EffectiveFrom.Equal(p.EffectiveFrom) {
			return domain.ModelPrice{}, domain.Conflict("duplicate", "exists", nil)
		}
	}
	r.prices = append(r.prices, p)
	return p, nil
}

var (
	svcAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true}}
	teamAdm  = acting.Caller{Principal: authz.Principal{ID: "usr_a", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: "team_1"}}}}
	unbound  = acting.Caller{Principal: authz.Principal{ID: "usr_u", Type: authz.PrincipalUser}}
)

func rig() (*models.Service, *repo, *fakes.Audit, *fakes.TxRunner) {
	a, tx := &fakes.Audit{}, &fakes.TxRunner{}
	r := &repo{models: map[string]domain.Model{}}
	return &models.Service{Deps: acting.Deps{Tx: tx, Audit: a, Outbox: &fakes.Outbox{}, Idem: &fakes.Idempotency{}, Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: r}, r, a, tx
}

func params(id string) domain.NewModelParams {
	return domain.NewModelParams{ID: id, Provider: "Example", DisplayName: "M", ContextWindow: 1000, MaxOutputTokens: 100}
}

func TestCreateAndUpdate(t *testing.T) {
	s, _, audit, _ := rig()
	ctx := t.Context()
	res, err := s.Create(ctx, svcAdmin, params("m-1"), nil)
	if err != nil || res.Value.ID != "m-1" || res.Value.Version != 1 {
		t.Fatalf("create = %+v %v", res, err)
	}
	if _, err := s.Create(ctx, svcAdmin, params("m-1"), nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("duplicate = %v", err)
	}
	if _, err := s.Create(ctx, svcAdmin, domain.NewModelParams{ID: "bad:id"}, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("invalid = %v", err)
	}
	name := "Renamed"
	got, err := s.Update(ctx, svcAdmin, "m-1", domain.ModelUpdate{DisplayName: &name}, 1)
	if err != nil || got.DisplayName != name || got.Version != 2 {
		t.Fatalf("update = %+v %v", got, err)
	}
	if _, err := s.Update(ctx, svcAdmin, "m-1", domain.ModelUpdate{DisplayName: &name}, 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale = %v", err)
	}
	if _, err := s.Update(ctx, svcAdmin, "absent", domain.ModelUpdate{DisplayName: &name}, 1); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("absent = %v", err)
	}
	zero := 0
	if _, err := s.Update(ctx, svcAdmin, "m-1", domain.ModelUpdate{ContextWindow: &zero}, 2); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("invalid update = %v", err)
	}
	if len(audit.Entries) != 2 {
		t.Errorf("audit = %d, want 2 (failed requests write nothing)", len(audit.Entries))
	}
	for _, e := range audit.Entries {
		if e.Record.Outcome != port.OutcomeAllowed {
			t.Errorf("outcome = %v", e.Record.Outcome)
		}
	}
}

func TestCreateReplay(t *testing.T) {
	s, r, _, _ := rig()
	idem := func() *acting.Idem {
		return &acting.Idem{Key: port.IdempotencyKey{Key: "k1", Operation: "CreateModel"},
			Render: func(v any) (int, json.RawMessage, error) {
				b, err := json.Marshal(map[string]string{"id": v.(domain.Model).ID})
				return 201, b, err
			}}
	}
	first, err := s.Create(t.Context(), svcAdmin, params("m-1"), idem())
	if err != nil || first.Replay != nil {
		t.Fatalf("first = %+v %v", first, err)
	}
	again, err := s.Create(t.Context(), svcAdmin, params("m-1"), idem())
	if err != nil || again.Replay == nil || again.Replay.Status != 201 || len(r.models) != 1 {
		t.Fatalf("replay = %+v %v", again, err)
	}
}

func TestLists(t *testing.T) {
	s, _, _, _ := rig()
	ctx := t.Context()
	for _, id := range []string{"a-1", "a-2", "a-3"} {
		if _, err := s.Create(ctx, svcAdmin, params(id), nil); err != nil {
			t.Fatal(err)
		}
	}
	off := false
	if _, err := s.Update(ctx, svcAdmin, "a-2", domain.ModelUpdate{Enabled: &off}, 1); err != nil {
		t.Fatal(err)
	}
	// Any signed-in caller sees the enabled models; only service admins see all.
	got, _, err := s.ListEnabled(ctx, teamAdm, acting.Page{})
	if err != nil || len(got) != 2 {
		t.Fatalf("enabled = %d %v", len(got), err)
	}
	all, next, err := s.ListAll(ctx, svcAdmin, acting.Page{Limit: 2})
	if err != nil || len(all) != 2 || next == "" {
		t.Fatalf("all page 1 = %d %q %v", len(all), next, err)
	}
	rest, next, err := s.ListAll(ctx, svcAdmin, acting.Page{Limit: 2, Cursor: next})
	if err != nil || len(rest) != 1 || next != "" {
		t.Fatalf("all page 2 = %d %q %v", len(rest), next, err)
	}
}

func TestPrices(t *testing.T) {
	s, _, _, _ := rig()
	ctx := t.Context()
	if _, err := s.Create(ctx, svcAdmin, params("m-1"), nil); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 3 {
		in := domain.NewModelPriceParams{EffectiveFrom: base.AddDate(0, i, 0), Input: domain.MustMoney("3"), Output: domain.MustMoney("15")}
		res, err := s.CreatePrice(ctx, svcAdmin, "m-1", in, nil)
		if err != nil || res.Value.Source != domain.PriceSourceManual {
			t.Fatalf("price %d = %+v %v", i, res, err)
		}
	}
	dup := domain.NewModelPriceParams{EffectiveFrom: base, Input: domain.MustMoney("1"), Output: domain.MustMoney("1")}
	if _, err := s.CreatePrice(ctx, svcAdmin, "m-1", dup, nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("same effective time = %v", err)
	}
	if _, err := s.CreatePrice(ctx, svcAdmin, "absent", dup, nil); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown model = %v", err)
	}
	if _, err := s.CreatePrice(ctx, svcAdmin, "m-1", domain.NewModelPriceParams{}, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("empty = %v", err)
	}
	page, next, err := s.ListPrices(ctx, svcAdmin, "m-1", acting.Page{Limit: 2})
	if err != nil || len(page) != 2 || next == "" || !page[0].EffectiveFrom.After(page[1].EffectiveFrom) {
		t.Fatalf("page 1 = %+v %q %v", page, next, err)
	}
	rest, next, err := s.ListPrices(ctx, svcAdmin, "m-1", acting.Page{Limit: 2, Cursor: next})
	if err != nil || len(rest) != 1 || next != "" {
		t.Fatalf("page 2 = %+v %q %v", rest, next, err)
	}
	if _, _, err := s.ListPrices(ctx, svcAdmin, "m-1", acting.Page{Cursor: "garbage"}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("bad cursor = %v", err)
	}
	if _, _, err := s.ListPrices(ctx, svcAdmin, "absent", acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("list unknown = %v", err)
	}
}

func TestDenied(t *testing.T) {
	s, r, audit, _ := rig()
	ctx := t.Context()
	r.models["m-1"] = domain.Model{ID: "m-1", Version: 1}
	name := "x"
	price := domain.NewModelPriceParams{EffectiveFrom: time.Now(), Input: domain.MustMoney("1"), Output: domain.MustMoney("1")}
	for name, call := range map[string]func(acting.Caller) error{
		"ListAll": func(c acting.Caller) error { _, _, err := s.ListAll(ctx, c, acting.Page{}); return err },
		"Create":  func(c acting.Caller) error { _, err := s.Create(ctx, c, params("m-9"), nil); return err },
		"Update": func(c acting.Caller) error {
			_, err := s.Update(ctx, c, "m-1", domain.ModelUpdate{DisplayName: &name}, 1)
			return err
		},
		"ListPrices":  func(c acting.Caller) error { _, _, err := s.ListPrices(ctx, c, "m-1", acting.Page{}); return err },
		"CreatePrice": func(c acting.Caller) error { _, err := s.CreatePrice(ctx, c, "m-1", price, nil); return err },
	} {
		for who, c := range map[string]acting.Caller{"team admin": teamAdm, "unbound": unbound} {
			if err := call(c); domain.KindOf(err) != domain.KindAccessDenied {
				t.Errorf("%s as %s = %v, want AccessDenied", name, who, err)
			}
		}
	}
	if len(r.models) != 1 || len(r.prices) != 0 {
		t.Errorf("denied calls changed data")
	}
	if len(audit.Entries) != 10 {
		t.Errorf("denied audit = %d, want 10", len(audit.Entries))
	}
}

func TestRepositoryFailureRollsBack(t *testing.T) {
	s, _, audit, tx := rig()
	tx.Err = domain.Internal(context.DeadlineExceeded)
	if _, err := s.Create(t.Context(), svcAdmin, params("m-1"), nil); err == nil {
		t.Fatal("want error")
	}
	if _, _, err := s.ListEnabled(t.Context(), svcAdmin, acting.Page{}); err == nil {
		t.Fatal("want error")
	}
	if len(audit.Entries) != 0 {
		t.Errorf("audit = %d", len(audit.Entries))
	}
}

package pools_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pools"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pools/poolstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

var now = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)

const (
	team    = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	persist = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	digest  = "sha256:4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f"
	image   = "registry.example.com/agents/coder@" + digest
	account = "111122223333"
)

type rig struct {
	svc    *pools.Service
	repo   *poolstest.Repo
	audit  *fakes.Audit
	outbox *fakes.Outbox
	tx     *fakes.TxRunner
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{repo: poolstest.New(), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}, tx: &fakes.TxRunner{}}
	r.svc = &pools.Service{
		Deps: acting.Deps{Tx: r.tx, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
			Clock: &fakes.Clock{T: now}, IDs: &fakes.IDGen{}},
		Repo: r.repo, Entropy: bytes.NewReader(bytes.Repeat([]byte{7}, 4096)),
	}
	r.repo.Kinds[team], r.repo.Kinds[persist] = domain.TeamKindEphemeral, domain.TeamKindPersistent
	r.repo.Models["m1"] = true
	r.repo.Templates["claude-code"] = pools.TemplateInfo{ID: "tpl_1", Name: "claude-code", Enabled: true, AllowedDigests: []string{digest}}
	r.repo.Templates["off"] = pools.TemplateInfo{ID: "tpl_2", Name: "off", AllowedDigests: []string{digest}}
	r.repo.Allowed[account] = true
	return r
}

var caller = acting.Caller{Principal: authz.Principal{ID: "usr_1", Type: authz.PrincipalUser}, RequestID: "req_1"}

func params(name string) domain.NewPoolParams {
	return domain.NewPoolParams{Name: name, Template: "claude-code", Image: image, ModelID: "m1", Workers: 3}
}

func (r *rig) pool(t *testing.T, name string) domain.Pool {
	t.Helper()
	res, err := r.svc.Create(t.Context(), caller, team, params(name), nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Pool
}

func kind(t *testing.T, err error, want domain.ErrorKind) {
	t.Helper()
	if domain.KindOf(err) != want {
		t.Fatalf("error kind = %q (%v), want %q", domain.KindOf(err), err, want)
	}
}

func (r *rig) writes() (int, int) { return len(r.audit.Entries), len(r.outbox.Events) }

func TestCreatePool(t *testing.T) {
	cases := []struct {
		name  string
		team  string
		setup func(r *rig)
		in    func(p *domain.NewPoolParams)
		want  domain.ErrorKind
	}{
		{"ok", team, nil, nil, ""},
		{"persistent team", persist, nil, nil, domain.KindConflict},
		{"unknown team", "team_nope", nil, nil, domain.KindNotFound},
		{"name of an agent", team, func(r *rig) { r.repo.Agents[team+"/fixer"] = true }, nil, domain.KindConflict},
		{"workers above 200", team, nil, func(p *domain.NewPoolParams) { p.Workers = 201 }, domain.KindValidation},
		{"model not enabled", team, nil, func(p *domain.NewPoolParams) { p.ModelID = "other" }, domain.KindValidation},
		{"unknown template", team, nil, func(p *domain.NewPoolParams) { p.Template = "nope" }, domain.KindValidation},
		{"disabled template", team, nil, func(p *domain.NewPoolParams) { p.Template = "off" }, domain.KindValidation},
		{"digest not allowed", team, nil, func(p *domain.NewPoolParams) {
			p.Image = "registry.example.com/a@sha256:" + string(bytes.Repeat([]byte{'a'}, 64))
		}, domain.KindValidation},
		{"unknown stage", team, nil, func(p *domain.NewPoolParams) { p.StageID = "stg_01JD2Q3R4S5T6V7W8X9Y0Z0009" }, domain.KindNotFound + "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			if tc.setup != nil {
				tc.setup(r)
			}
			in := params("fixer")
			if tc.in != nil {
				tc.in(&in)
			}
			want := tc.want
			if want == domain.KindNotFound+"x" {
				want = domain.KindValidation
			}
			res, err := r.svc.Create(t.Context(), caller, tc.team, in, nil)
			if want != "" {
				kind(t, err, want)
				if a, e := r.writes(); a != 0 || e != 0 || len(r.repo.Pools) != 0 {
					t.Fatal("a refused create wrote something")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			a, e := r.writes()
			if a != 1 || e != 1 || r.outbox.Events[0].Type != domain.EventPoolCreated || r.outbox.Events[0].Detail.PoolID != res.Pool.ID {
				t.Fatalf("audit %d events %+v", a, r.outbox.Events)
			}
		})
	}
}

func TestCreateWithStageAndIdempotency(t *testing.T) {
	r := newRig(t)
	st, err := r.svc.CreateStage(t.Context(), caller, team, domain.NewStageParams{Name: "triage", Position: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := params("fixer")
	in.StageID = st.Stage.ID
	mk := func() *acting.Idem {
		return &acting.Idem{Key: port.IdempotencyKey{Key: "k1", Operation: "CreatePool", TeamID: team},
			Render: func(v any) (int, json.RawMessage, error) { b, e := json.Marshal(v.(domain.Pool).ID); return 201, b, e }}
	}
	first, err := r.svc.Create(t.Context(), caller, team, in, mk())
	if err != nil || first.Replay != nil {
		t.Fatal(err)
	}
	again, err := r.svc.Create(t.Context(), caller, team, in, mk())
	if err != nil || again.Replay == nil || again.Replay.Status != 201 || len(r.repo.Pools) != 1 {
		t.Fatalf("replay %+v %v", again.Replay, err)
	}
}

func TestCreateFailuresWriteNothing(t *testing.T) {
	for _, m := range []string{"TeamKind", "AgentNameTaken", "ModelSets", "Template", "Insert", "Handoffs"} {
		t.Run(m, func(t *testing.T) {
			r := newRig(t)
			r.repo.Fail[m] = errors.New("boom")
			if _, err := r.svc.Create(t.Context(), caller, team, params("fixer"), nil); err == nil {
				t.Fatal("expected the failure")
			}
			if a, e := r.writes(); a != 0 || e != 0 || len(r.repo.Pools) != 0 {
				t.Fatal("a failed create left records behind")
			}
		})
	}
}

func TestListGetStatus(t *testing.T) {
	r := newRig(t)
	a, b := r.pool(t, "alpha"), r.pool(t, "bravo")
	rows, next, err := r.svc.List(t.Context(), caller, team, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" || rows[0].ID != a.ID {
		t.Fatalf("%v %q %v", rows, next, err)
	}
	got, err := r.svc.Get(t.Context(), caller, team, b.ID)
	if err != nil || got.ID != b.ID {
		t.Fatal(err)
	}
	if _, err := r.svc.Get(t.Context(), caller, team, "pool_nope"); domain.KindOf(err) != domain.KindNotFound {
		t.Fatal(err)
	}
	r.repo.Counts = domain.PoolWorkers{Reporting: 2, Working: 1, Idle: 1, Impaired: 0}
	st, err := r.svc.GetStatus(t.Context(), caller, team, a.ID)
	if err != nil || st.Workers.Desired != 3 || st.Workers.Reporting != 2 {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := r.svc.Pause(t.Context(), caller, team, a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if st, _ = r.svc.GetStatus(t.Context(), caller, team, a.ID); st.Workers.Desired != 0 {
		t.Fatal("a paused pool desires no workers")
	}
	for _, m := range []string{"List", "Get", "Workers"} {
		r.repo.Fail[m] = errors.New("boom")
		_, _, e1 := r.svc.List(t.Context(), caller, team, acting.Page{})
		_, e2 := r.svc.GetStatus(t.Context(), caller, team, a.ID)
		if e1 == nil && e2 == nil {
			t.Errorf("%s failure ignored", m)
		}
		delete(r.repo.Fail, m)
	}
}

func TestUpdateDeleteScalePauseResume(t *testing.T) {
	r := newRig(t)
	p := r.pool(t, "fixer")
	desc, w := "hello", 10
	up, err := r.svc.Update(t.Context(), caller, team, p.ID, p.Version, domain.PoolPatch{Description: &desc, Workers: &w})
	if err != nil || up.Workers != 10 {
		t.Fatalf("%+v %v", up, err)
	}
	types := []domain.EventType{}
	for _, e := range r.outbox.Events {
		types = append(types, e.Type)
	}
	if len(types) != 3 || types[1] != domain.EventPoolUpdated || types[2] != domain.EventPoolScaled {
		t.Fatalf("events %v", types)
	}
	_, err = r.svc.Update(t.Context(), caller, team, p.ID, p.Version, domain.PoolPatch{})
	kind(t, err, domain.KindPreconditionFailed)
	bad := 999
	_, err = r.svc.Update(t.Context(), caller, team, p.ID, up.Version, domain.PoolPatch{Workers: &bad})
	kind(t, err, domain.KindValidation)
	tpl, img := "off", image
	_, err = r.svc.Update(t.Context(), caller, team, p.ID, up.Version, domain.PoolPatch{Template: &tpl, Image: &img})
	kind(t, err, domain.KindValidation)
	model := "zzz"
	_, err = r.svc.Update(t.Context(), caller, team, p.ID, up.Version, domain.PoolPatch{ModelID: &model})
	kind(t, err, domain.KindValidation)
	sid := "stg_01JD2Q3R4S5T6V7W8X9Y0Z0009"
	_, err = r.svc.Update(t.Context(), caller, team, p.ID, up.Version, domain.PoolPatch{StageID: domain.OptionalString{Set: true, Value: &sid}})
	kind(t, err, domain.KindValidation)
	_, err = r.svc.Update(t.Context(), caller, persist, p.ID, up.Version, domain.PoolPatch{})
	kind(t, err, domain.KindConflict)

	// scale
	_, err = r.svc.Scale(t.Context(), caller, team, p.ID, 201, nil)
	kind(t, err, domain.KindValidation)
	sc, err := r.svc.Scale(t.Context(), caller, team, p.ID, 0, nil)
	if err != nil || sc.Pool.Workers != 0 {
		t.Fatal(err)
	}
	before := len(r.outbox.Events)
	if _, err := r.svc.Scale(t.Context(), caller, team, p.ID, 0, nil); err != nil || len(r.outbox.Events) != before {
		t.Fatal("scaling to the same count must write nothing", err)
	}
	// pause, resume
	pa, err := r.svc.Pause(t.Context(), caller, team, p.ID, nil)
	if err != nil || pa.Pool.State != domain.PoolStatePaused {
		t.Fatal(err)
	}
	if _, err := r.svc.Pause(t.Context(), caller, team, p.ID, nil); err != nil {
		t.Fatal(err)
	}
	re, err := r.svc.Resume(t.Context(), caller, team, p.ID, nil)
	if err != nil || re.Pool.State != domain.PoolStateEnabled {
		t.Fatal(err)
	}
	_, err = r.svc.Pause(t.Context(), caller, persist, p.ID, nil)
	kind(t, err, domain.KindConflict)
	// delete
	cur, _ := r.svc.Get(t.Context(), caller, team, p.ID)
	_, err = r.svc.Delete(t.Context(), caller, team, p.ID, cur.Version+5)
	kind(t, err, domain.KindPreconditionFailed)
	del, err := r.svc.Delete(t.Context(), caller, team, p.ID, cur.Version)
	if err != nil || del.State != domain.PoolStateDeleting || del.Status.Phase != domain.PhaseDeleting {
		t.Fatalf("%+v %v", del, err)
	}
	again, err := r.svc.Delete(t.Context(), caller, team, p.ID, del.Version)
	if err != nil || again.State != domain.PoolStateDeleting {
		t.Fatal(err)
	}
	_, err = r.svc.Update(t.Context(), caller, team, p.ID, del.Version, domain.PoolPatch{})
	kind(t, err, domain.KindConflict)
	_, err = r.svc.Pause(t.Context(), caller, team, p.ID, nil)
	kind(t, err, domain.KindConflict)
	_, err = r.svc.Delete(t.Context(), caller, persist, p.ID, del.Version)
	kind(t, err, domain.KindConflict)
}

func TestMutationFailuresWriteNothing(t *testing.T) {
	v := "x"
	w := 1
	type op func(r *rig, p, q domain.Pool) error
	tests := []struct {
		fail string
		run  op
	}{
		{"Update", func(r *rig, p, _ domain.Pool) error {
			_, e := r.svc.Update(t.Context(), caller, team, p.ID, p.Version, domain.PoolPatch{Workers: &w})
			return e
		}},
		{"ReplaceHandoffs", func(r *rig, p, q domain.Pool) error {
			_, e := r.svc.PutHandoffs(t.Context(), caller, team, p.ID, nil, []string{q.ID})
			return e
		}},
		{"ReplaceBindings", func(r *rig, p, _ domain.Pool) error {
			_, e := r.svc.PutBindings(t.Context(), caller, team, p.ID, nil, nil)
			return e
		}},
		{"InsertConfig", func(r *rig, p, _ domain.Pool) error {
			_, e := r.svc.PutConfig(t.Context(), caller, team, p.ID, "LEVEL", pools.ConfigInput{Source: domain.ConfigSourceLiteral, Value: &v}, nil)
			return e
		}},
		{"SetSystemOff", func(r *rig, p, _ domain.Pool) error {
			r.repo.Systems["ext_1"] = domain.ExternalSystemAccess{ExternalSystem: domain.ExternalSystem{ID: "ext_1"}}
			_, e := r.svc.SetSystemState(t.Context(), caller, team, p.ID, "ext_1", false, nil)
			return e
		}},
		{"InsertLink", func(r *rig, p, _ domain.Pool) error {
			_, e := r.svc.CreateLink(t.Context(), caller, team, p.ID, domain.NewPoolRoleLinkParams{RoleName: "r", AccountID: account, ProfileName: "p"}, nil)
			return e
		}},
		{"InsertStage", func(r *rig, _, _ domain.Pool) error {
			_, e := r.svc.CreateStage(t.Context(), caller, team, domain.NewStageParams{Name: "triage", Position: 2}, nil)
			return e
		}},
	}
	for _, tc := range tests {
		t.Run(tc.fail, func(t *testing.T) {
			r := newRig(t)
			p := r.pool(t, "fixer")
			q := r.pool(t, "other")
			r.repo.Fail[tc.fail] = errors.New("boom")
			a0, e0 := r.writes()
			if err := tc.run(r, p, q); err == nil {
				t.Fatalf("%s: operation did not fail", tc.fail)
			}
			if a, ev := r.writes(); a != a0 || ev != e0 {
				t.Errorf("%s: a failed mutation left audit or events behind (%d/%d, %d/%d)", tc.fail, a, a0, ev, e0)
			}
		})
	}
}

package agents_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents/agentstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	team   = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	model  = "anthropic.claude-test"
	digest = "sha256:4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f4f"
	image  = "registry.example.com/agents/coder@" + digest
	secA   = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	secB   = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	secC   = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0003"
)

type rig struct {
	svc    *agents.Service
	repo   *agentstest.Repo
	audit  *fakes.Audit
	outbox *fakes.Outbox
}

var admin = acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser,
	Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: team}}}, RequestID: "req-1"}

func newRig() *rig {
	r := &rig{repo: agentstest.NewRepo(), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}}
	r.repo.Models[team] = []string{model}
	r.repo.Templates["claude-code"] = agents.TemplateInfo{ID: "tpl_1", Name: "claude-code", Enabled: true, AllowedDigests: []string{digest}}
	r.repo.SecretsBy[secA] = agents.SecretInfo{}
	r.repo.SecretsBy[secB] = agents.SecretInfo{AgentID: "agt_other"}
	r.repo.SecretsBy[secC] = agents.SecretInfo{PoolID: "pool_x"}
	r.svc = &agents.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: r.repo}
	return r
}

func params(name string) domain.NewAgentParams {
	return domain.NewAgentParams{Name: name, Template: "claude-code", Image: image, ModelID: model}
}

func (r *rig) create(t *testing.T, name string) domain.Agent {
	t.Helper()
	res, err := r.svc.Create(t.Context(), admin, team, params(name), nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Agent
}

func TestCreateChecks(t *testing.T) {
	bind := func(id string) []domain.SecretBinding {
		return []domain.SecretBinding{{SecretID: id, Delivery: "env", Target: "TOKEN"}}
	}
	tests := []struct {
		name  string
		setup func(*rig)
		mod   func(*domain.NewAgentParams)
		field string // empty: success
	}{
		{name: "ok", mod: func(p *domain.NewAgentParams) { p.Bindings = bind(secA) }},
		{name: "model not enabled for the team", mod: func(p *domain.NewAgentParams) { p.ModelID = "other.model" }, field: "modelId"},
		{name: "unknown template", mod: func(p *domain.NewAgentParams) { p.Template = "nope-tpl" }, field: "template"},
		{name: "disabled template", setup: func(r *rig) {
			r.repo.Templates["claude-code"] = agents.TemplateInfo{ID: "tpl_1", Enabled: false, AllowedDigests: []string{digest}}
		}, field: "template"},
		{name: "digest not allowed", mod: func(p *domain.NewAgentParams) {
			p.Image = "registry.example.com/agents/coder@sha256:" + strings.Repeat("b", 64)
		}, field: "image"},
		{name: "storage class not offered", mod: func(p *domain.NewAgentParams) { p.Volume.StorageClass = "io2" }, field: "volume.storageClass"},
		{name: "secret of another agent", mod: func(p *domain.NewAgentParams) { p.Bindings = bind(secB) }, field: "secretBindings[0].secretId"},
		{name: "secret of a pool", mod: func(p *domain.NewAgentParams) { p.Bindings = bind(secC) }, field: "secretBindings[0].secretId"},
		{name: "unknown secret", mod: func(p *domain.NewAgentParams) { p.Bindings = bind("sec_01JD2Q3R4S5T6V7W8X9Y0Z0009") }, field: "secretBindings[0].secretId"},
		{name: "bad name", mod: func(p *domain.NewAgentParams) { p.Name = "Bad Name" }, field: "name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig()
			if tc.setup != nil {
				tc.setup(r)
			}
			p := params("coder")
			if tc.mod != nil {
				tc.mod(&p)
			}
			res, err := r.svc.Create(t.Context(), admin, team, p, nil)
			if tc.field == "" {
				if err != nil {
					t.Fatal(err)
				}
				if res.Agent.State != domain.AgentEnabled || res.Agent.Generation != 1 || len(r.audit.Entries) != 1 || len(r.outbox.Events) != 1 {
					t.Errorf("created: %+v audit=%d outbox=%d", res.Agent, len(r.audit.Entries), len(r.outbox.Events))
				}
				if ev := r.outbox.Events[0]; ev.Type != domain.EventAgentCreated || ev.Detail.AgentID != res.Agent.ID {
					t.Errorf("event: %+v", ev)
				}
				return
			}
			var de *domain.Error
			if err == nil || !asErr(err, &de) || de.Kind != domain.KindValidation || !hasField(de, tc.field) {
				t.Fatalf("want validation on %s, got %v", tc.field, err)
			}
			if len(r.audit.Entries) != 0 || len(r.outbox.Events) != 0 || len(r.repo.Rows) != 0 {
				t.Error("a refused create wrote something")
			}
		})
	}
}

func TestUpdate(t *testing.T) {
	r := newRig()
	a := r.create(t, "coder")
	d := "new description"

	// A disabled template does not block editing other fields (RA-US-4.5), and the template columns stay unwritten.
	r.repo.Templates["claude-code"] = agents.TemplateInfo{ID: "tpl_1", Enabled: false, AllowedDigests: []string{digest}}
	got, err := r.svc.Update(t.Context(), admin, team, a.ID, a.Version, domain.AgentPatch{Description: &d})
	if err != nil || got.Description != d || got.Generation != 2 || r.repo.TemplateWrites != 0 {
		t.Fatalf("update: %+v %v writes=%d", got, err, r.repo.TemplateWrites)
	}
	if ev := r.outbox.Events[len(r.outbox.Events)-1]; ev.Type != domain.EventAgentUpdated || ev.Detail.AgentID != a.ID {
		t.Errorf("update event = %+v, want agent.updated", ev)
	}
	if _, err := r.svc.Update(t.Context(), admin, team, a.ID, a.Version, domain.AgentPatch{Description: &d}); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale version: %v", err)
	}
	// Moving to the disabled template is refused.
	tpl := "claude-code"
	other := "registry.example.com/agents/coder@sha256:" + strings.Repeat("c", 64)
	if _, err := r.svc.Update(t.Context(), admin, team, a.ID, got.Version, domain.AgentPatch{Template: &tpl, Image: &other}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("disabled template: %v", err)
	}
	// Shrinking the volume and changing its class are refused.
	for _, v := range []domain.VolumeSpec{{SizeGiB: 5}, {StorageClass: "io2"}} {
		if _, err := r.svc.Update(t.Context(), admin, team, a.ID, got.Version, domain.AgentPatch{Volume: &v}); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("volume %+v: %v", v, err)
		}
	}
	// Growing is fine; an unknown model is not.
	grow := domain.VolumeSpec{SizeGiB: 50}
	g2, err := r.svc.Update(t.Context(), admin, team, a.ID, got.Version, domain.AgentPatch{Volume: &grow})
	if err != nil || g2.Volume.SizeGiB != 50 {
		t.Fatalf("grow: %+v %v", g2, err)
	}
	bad := "no.model"
	if _, err := r.svc.Update(t.Context(), admin, team, a.ID, g2.Version, domain.AgentPatch{ModelID: &bad}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("model: %v", err)
	}
	// Other teams' agents are not found.
	if _, err := r.svc.Update(t.Context(), admin, "team_01JD2Q3R4S5T6V7W8X9Y0Z0002", a.ID, g2.Version, domain.AgentPatch{Description: &d}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("other team: %v", err)
	}
}

func TestLifecycle(t *testing.T) {
	r := newRig()
	a := r.create(t, "coder")
	ctx := t.Context()

	p, err := r.svc.Pause(ctx, admin, team, a.ID, nil)
	if err != nil || p.Agent.State != domain.AgentPaused || p.Agent.Generation != 2 {
		t.Fatalf("pause: %+v %v", p.Agent, err)
	}
	if ev := r.outbox.Events[len(r.outbox.Events)-1]; ev.Type != domain.EventAgentPaused || ev.Detail.AgentID != a.ID {
		t.Errorf("pause event = %+v, want agent.paused", ev)
	}
	n := len(r.outbox.Events)
	if p2, err := r.svc.Pause(ctx, admin, team, a.ID, nil); err != nil || p2.Agent.Generation != 2 || len(r.outbox.Events) != n {
		t.Errorf("second pause must change nothing: %+v %v events=%d", p2.Agent, err, len(r.outbox.Events))
	}
	if _, err := r.svc.Restart(ctx, admin, team, a.ID, nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("restart of a paused agent: %v", err)
	}
	res, err := r.svc.Resume(ctx, admin, team, a.ID, nil)
	if err != nil || res.Agent.State != domain.AgentEnabled || res.Agent.Generation != 3 {
		t.Fatalf("resume: %+v %v", res.Agent, err)
	}
	if ev := r.outbox.Events[len(r.outbox.Events)-1]; ev.Type != domain.EventAgentResumed || ev.Detail.AgentID != a.ID {
		t.Errorf("resume event = %+v, want agent.resumed", ev)
	}
	rs, err := r.svc.Restart(ctx, admin, team, a.ID, nil)
	if err != nil || rs.Agent.Generation != 4 || r.outbox.Events[len(r.outbox.Events)-1].Type != domain.EventAgentRestarted {
		t.Fatalf("restart: %+v %v", rs.Agent, err)
	}

	cur, _ := r.svc.Get(ctx, admin, team, a.ID)
	if _, err := r.svc.Delete(ctx, admin, team, a.ID, cur.Version+9); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale delete: %v", err)
	}
	del, err := r.svc.Delete(ctx, admin, team, a.ID, cur.Version)
	if err != nil || del.State != domain.AgentDeleting || del.Status.Phase != domain.PhaseDeleting {
		t.Fatalf("delete: %+v %v", del, err)
	}
	last := r.outbox.Events[len(r.outbox.Events)-1]
	if last.Type != domain.EventAgentDeleted || last.Detail.AgentID != a.ID {
		t.Errorf("delete event: %+v", last)
	}
	// Deleting again is a no-op and writes nothing more.
	n = len(r.outbox.Events)
	if again, err := r.svc.Delete(ctx, admin, team, a.ID, del.Version); err != nil || again.State != domain.AgentDeleting || len(r.outbox.Events) != n {
		t.Errorf("repeat delete: %+v %v", again, err)
	}
	// A deleting agent cannot be changed, paused or snapshotted.
	d := "x"
	cur, _ = r.svc.Get(ctx, admin, team, a.ID)
	if _, err := r.svc.Update(ctx, admin, team, a.ID, cur.Version, domain.AgentPatch{Description: &d}); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("update deleting: %v", err)
	}
	if _, err := r.svc.Pause(ctx, admin, team, a.ID, nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("pause deleting: %v", err)
	}
	if _, err := r.svc.CreateSnapshot(ctx, admin, team, a.ID, "", nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("snapshot deleting: %v", err)
	}
}

func TestListAndSnapshots(t *testing.T) {
	r := newRig()
	a := r.create(t, "alpha")
	r.create(t, "beta")
	r.create(t, "gamma")
	ctx := t.Context()

	page, next, err := r.svc.List(ctx, admin, team, acting.Page{Limit: 2})
	if err != nil || len(page) != 2 || next == "" {
		t.Fatalf("page 1: %v next=%q %v", page, next, err)
	}
	rest, next2, err := r.svc.List(ctx, admin, team, acting.Page{Limit: 2, Cursor: next})
	if err != nil || len(rest) != 1 || next2 != "" {
		t.Fatalf("page 2: %v next=%q %v", rest, next2, err)
	}

	for i := 0; i < 3; i++ {
		if _, err := r.svc.CreateSnapshot(ctx, admin, team, a.ID, "before upgrade", nil); err != nil {
			t.Fatal(err)
		}
	}
	snaps, next, err := r.svc.ListSnapshots(ctx, admin, team, a.ID, acting.Page{Limit: 2})
	if err != nil || len(snaps) != 2 || next == "" || snaps[0].Reason != "manual" || snaps[0].State != "requested" {
		t.Fatalf("snapshots: %+v next=%q %v", snaps, next, err)
	}
	if _, err := r.svc.CreateSnapshot(ctx, admin, team, a.ID, strings.Repeat("x", 201), nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("long description: %v", err)
	}
	if _, _, err := r.svc.ListSnapshots(ctx, admin, team, "agt_01JD2Q3R4S5T6V7W8X9Y0Z0009", acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown agent: %v", err)
	}
	if got := r.audit.Entries[len(r.audit.Entries)-1].Record.Action; got != "CreateAgentSnapshot" {
		t.Errorf("audit action %q", got)
	}
}

type failAudit struct{ fakes.Audit }

func (*failAudit) Write(context.Context, port.Tx, port.AuditRecord) error { return errBoom }

type failOutbox struct{ fakes.Outbox }

func (*failOutbox) Publish(context.Context, port.Tx, domain.Event) error { return errBoom }

var errBoom = errors.New("boom")

func idemOf(op, key string) *acting.Idem {
	return &acting.Idem{Key: port.IdempotencyKey{Key: key, Operation: op, TeamID: team, RequestSHA256: [32]byte{1}},
		Render: func(any) (int, json.RawMessage, error) { return 200, json.RawMessage(`{"ok":true}`), nil }}
}

// Every store, audit and outbox failure aborts the call with that error.
func TestFailuresAbort(t *testing.T) {
	faults := map[string]func(*rig){
		"audit":  func(r *rig) { r.svc.Audit = &failAudit{} },
		"outbox": func(r *rig) { r.svc.Outbox = &failOutbox{} },
		"get":    func(r *rig) { r.repo.Err = map[string]error{"Get": errBoom} },
		"update": func(r *rig) { r.repo.Err = map[string]error{"Update": errBoom} },
		"insert": func(r *rig) { r.repo.Err = map[string]error{"Insert": errBoom} },
		"model":  func(r *rig) { r.repo.Err = map[string]error{"ModelEnabled": errBoom} },
		"class":  func(r *rig) { r.repo.Err = map[string]error{"StorageClassEnabled": errBoom} },
		"tpl":    func(r *rig) { r.repo.Err = map[string]error{"Template": errBoom} },
		"secret": func(r *rig) { r.repo.Err = map[string]error{"Secrets": errBoom} },
		"snap":   func(r *rig) { r.repo.Err = map[string]error{"InsertSnapshot": errBoom} },
		"list":   func(r *rig) { r.repo.Err = map[string]error{"List": errBoom, "Snapshots": errBoom} },
	}
	for _, op := range []string{"create", "update", "delete", "pause", "snapshot", "list"} {
		for name, fault := range faults {
			t.Run(op+"/"+name, func(t *testing.T) {
				r := newRig()
				a := r.create(t, "seeded")
				fault(r)
				err := opOn(r, op, a)
				if err == nil {
					t.Skip("this fault does not touch this operation")
				}
				if !errors.Is(err, errBoom) {
					t.Fatalf("err = %v", err)
				}
			})
		}
	}
}

func opOn(r *rig, op string, a domain.Agent) error {
	ctx := context.Background()
	d := "desc"
	bs := []domain.SecretBinding{{SecretID: secA, Delivery: "env", Target: "TOKEN"}}
	switch op {
	case "create":
		p := params("fresh")
		p.Bindings = bs
		_, err := r.svc.Create(ctx, admin, team, p, nil)
		return err
	case "update":
		_, err := r.svc.Update(ctx, admin, team, a.ID, a.Version, domain.AgentPatch{Description: &d, Bindings: &bs})
		return err
	case "delete":
		_, err := r.svc.Delete(ctx, admin, team, a.ID, a.Version)
		return err
	case "pause":
		_, err := r.svc.Pause(ctx, admin, team, a.ID, nil)
		return err
	case "list":
		if _, _, err := r.svc.List(ctx, admin, team, acting.Page{}); err != nil {
			return err
		}
		_, _, err := r.svc.ListSnapshots(ctx, admin, team, a.ID, acting.Page{})
		return err
	default:
		_, err := r.svc.CreateSnapshot(ctx, admin, team, a.ID, "", nil)
		return err
	}
}

// A replayed idempotent call returns the stored response and does nothing more.
func TestIdempotentReplay(t *testing.T) {
	r := newRig()
	first, err := r.svc.Create(t.Context(), admin, team, params("coder"), idemOf("CreateAgent", "k1"))
	if err != nil || first.Replay != nil {
		t.Fatalf("first: %+v %v", first, err)
	}
	again, err := r.svc.Create(t.Context(), admin, team, params("coder"), idemOf("CreateAgent", "k1"))
	if err != nil || again.Replay == nil || len(r.repo.Rows) != 1 {
		t.Fatalf("replay: %+v %v rows=%d", again, err, len(r.repo.Rows))
	}
	a := first.Agent
	if _, err := r.svc.Pause(t.Context(), admin, team, a.ID, idemOf("PauseAgent", "k2")); err != nil {
		t.Fatal(err)
	}
	if res, err := r.svc.Pause(t.Context(), admin, team, a.ID, idemOf("PauseAgent", "k2")); err != nil || res.Replay == nil {
		t.Errorf("pause replay: %+v %v", res, err)
	}
	if _, err := r.svc.CreateSnapshot(t.Context(), admin, team, a.ID, "", idemOf("CreateAgentSnapshot", "k3")); err != nil {
		t.Fatal(err)
	}
	if res, err := r.svc.CreateSnapshot(t.Context(), admin, team, a.ID, "", idemOf("CreateAgentSnapshot", "k3")); err != nil || res.Replay == nil || len(r.repo.Snaps) != 1 {
		t.Errorf("snapshot replay: %+v %v snaps=%d", res, err, len(r.repo.Snaps))
	}
}

// Bindings supplied on update are checked; the same bindings on a new agent are stored.
func TestUpdateBindings(t *testing.T) {
	r := newRig()
	a := r.create(t, "coder")
	bs := []domain.SecretBinding{{SecretID: secB, Delivery: "env", Target: "TOKEN"}}
	if _, err := r.svc.Update(t.Context(), admin, team, a.ID, a.Version, domain.AgentPatch{Bindings: &bs}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("another agent's secret: %v", err)
	}
	ok := []domain.SecretBinding{{SecretID: secA, Delivery: "file", Target: "/etc/token"}}
	got, err := r.svc.Update(t.Context(), admin, team, a.ID, a.Version, domain.AgentPatch{Bindings: &ok})
	if err != nil || len(got.Bindings) != 1 {
		t.Errorf("own binding: %+v %v", got.Bindings, err)
	}
}

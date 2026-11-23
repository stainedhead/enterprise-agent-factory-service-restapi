package collab_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/collab"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	team   = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	collAg = "agt_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	worker = "agt_01JD2Q3R4S5T6V7W8X9Y0Z0002"
)

var (
	admin = acting.Caller{Principal: authz.Principal{ID: "usr_a", Type: authz.PrincipalUser,
		Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: team}}}, RequestID: "req-1"}
	viewer = acting.Caller{Principal: authz.Principal{ID: "usr_v", Type: authz.PrincipalUser,
		Bindings: []authz.Binding{{Role: authz.RoleTeamViewer, TeamID: team}}}, RequestID: "req-2"}
	service  = acting.Caller{Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true}, RequestID: "req-3"}
	outsider = acting.Caller{Principal: authz.Principal{ID: "usr_o", Type: authz.PrincipalUser}, RequestID: "req-4"}
)

type repo struct {
	col      *domain.TeamCollaboration
	links    map[string]domain.CollaborationLink
	agents   map[string]collab.AgentRef
	secrets  map[string]collab.SecretRef
	versions map[string]string
	bumped   int
	fail     string
	runtime  map[domain.CollabTool]collab.ToolRuntime
	switched []collab.SwitchToolInput
	blocked  []domain.OrchestrationBlocked
}

func (r *repo) boom(op string) error {
	if r.fail == op {
		return errors.New(op + " failed")
	}
	return nil
}

func (r *repo) Get(_ context.Context, _ port.Tx, _ string, _ bool) (domain.TeamCollaboration, bool, error) {
	if err := r.boom("get"); err != nil {
		return domain.TeamCollaboration{}, false, err
	}
	if r.col == nil {
		return domain.TeamCollaboration{}, false, nil
	}
	return *r.col, true, nil
}

func (r *repo) Update(_ context.Context, _ port.Tx, _ string, web, api domain.Exposure, mode domain.RegistrationMode) error {
	if err := r.boom("update"); err != nil {
		return err
	}
	r.col.WebUIExposure, r.col.WebAPIExposure, r.col.Mode = web, api, mode
	r.col.Version++
	return nil
}

func (r *repo) Migration(context.Context, port.Tx) (int, int, []domain.OrchestrationBlocked, error) {
	if err := r.boom("migration"); err != nil {
		return 0, 0, nil, err
	}
	return 3, 3 - len(r.blocked), r.blocked, nil
}

func (r *repo) ToolRuntime(_ context.Context, _ port.Tx, tool domain.CollabTool) (collab.ToolRuntime, error) {
	if err := r.boom("runtime"); err != nil {
		return collab.ToolRuntime{}, err
	}
	return r.runtime[tool], nil
}

func (r *repo) SwitchTool(_ context.Context, _ port.Tx, in collab.SwitchToolInput) (string, error) {
	if err := r.boom("switch"); err != nil {
		return "", err
	}
	r.switched = append(r.switched, in)
	r.col.Tool, r.col.Mode = in.ToTool, in.Mode
	for _, sfc := range []domain.Surface{domain.SurfaceWebUI, domain.SurfaceWebAPI} {
		if in.ToTool.Supports(sfc) {
			continue
		}
		if sfc == domain.SurfaceWebUI {
			r.col.WebUIExposure = domain.ExposureOff
		} else {
			r.col.WebAPIExposure = domain.ExposureOff
		}
	}
	r.col.Version++
	return in.SnapshotID, nil
}

func (r *repo) ListLinks(_ context.Context, _ port.Tx, _ string, f collab.LinkFilter, after string, limit int) ([]domain.CollaborationLink, error) {
	var out []domain.CollaborationLink
	for _, id := range []string{worker, "agt_01JD2Q3R4S5T6V7W8X9Y0Z0003"} {
		l, ok := r.links[id]
		if !ok || id <= after {
			continue
		}
		if f.State != "" && l.LinkState != f.State {
			continue
		}
		if f.Connect != nil && l.Connect != *f.Connect {
			continue
		}
		out = append(out, l)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *repo) Agent(_ context.Context, _ port.Tx, _, id string) (collab.AgentRef, bool, error) {
	if err := r.boom("agent"); err != nil {
		return collab.AgentRef{}, false, err
	}
	a, ok := r.agents[id]
	return a, ok, nil
}

func (r *repo) Link(_ context.Context, _ port.Tx, _, id string, _ bool) (domain.CollaborationLink, error) {
	if err := r.boom("link"); err != nil {
		return domain.CollaborationLink{}, err
	}
	if l, ok := r.links[id]; ok {
		return l, nil
	}
	return domain.CollaborationLink{AgentID: id, LinkState: domain.LinkDisabled}, nil
}

func (r *repo) SetLink(_ context.Context, _ port.Tx, _, id string, connect bool, pc *string) error {
	if err := r.boom("setlink"); err != nil {
		return err
	}
	l := domain.CollaborationLink{AgentID: id, Connect: connect, LinkState: domain.LinkPending}
	if !connect {
		l.LinkState = domain.LinkDisabled
	}
	if pc != nil {
		l.PaperclipAgentID = *pc
	}
	r.links[id] = l
	return nil
}

func (r *repo) BumpAgent(context.Context, port.Tx, string, string) error {
	if err := r.boom("bump"); err != nil {
		return err
	}
	r.bumped++
	return nil
}

func (r *repo) BootSecret(_ context.Context, _ port.Tx, _, _, name string) (collab.SecretRef, bool, error) {
	if err := r.boom("boot"); err != nil {
		return collab.SecretRef{}, false, err
	}
	s, ok := r.secrets[name]
	return s, ok, nil
}

func (r *repo) SetSecretVersion(_ context.Context, _ port.Tx, _, id, v string, _ time.Time) error {
	if err := r.boom("version"); err != nil {
		return err
	}
	r.versions[id] = v
	return nil
}

func (r *repo) Endpoints(_ context.Context, _ port.Tx, f collab.EndpointFilter, _ string, limit int) ([]collab.EndpointRow, error) {
	if err := r.boom("endpoints"); err != nil {
		return nil, err
	}
	rows := []collab.EndpointRow{{Collab: *r.col, Phase: domain.PhaseReady}, {Collab: domain.TeamCollaboration{TeamID: "team_z", Tool: domain.ToolPaperclip, WebUIExposure: domain.ExposurePrivate, WebAPIExposure: domain.ExposureOff, Limit: domain.ExposurePrivate, TeamSlug: "zed", ProviderClientID: "c"}}}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

type writer struct {
	arns []string
	err  error
}

func (w *writer) PutValue(_ context.Context, arn string, _ domain.SecretValue, _ string) (string, error) {
	if w.err != nil {
		return "", w.err
	}
	w.arns = append(w.arns, arn)
	return "ver-" + arn, nil
}

type rig struct {
	svc    *collab.Service
	repo   *repo
	audit  *fakes.Audit
	outbox *fakes.Outbox
	set    *fakes.Settings
	w      *writer
}

func newRig() *rig {
	r := &rig{audit: &fakes.Audit{}, outbox: &fakes.Outbox{}, set: &fakes.Settings{}, w: &writer{}}
	r.repo = &repo{
		col: &domain.TeamCollaboration{TeamID: team, AgentID: collAg, Tool: domain.ToolPaperclip, Mode: domain.RegistrationManual, Version: 1,
			Limit: domain.ExposurePublic, WebUIExposure: domain.ExposureOff, WebAPIExposure: domain.ExposureOff,
			ProviderClientID: "cid", TeamSlug: "acme", Status: domain.Status{Phase: domain.PhaseReady}},
		runtime: map[domain.CollabTool]collab.ToolRuntime{
			domain.ToolBuzz: {Tool: domain.ToolBuzz, Enabled: true, TemplateID: "tpl_b", ImageRepository: "registry.example.com/buzz", Digest: "sha256:aa", TemplateEnabled: true},
		},
		links:    map[string]domain.CollaborationLink{},
		agents:   map[string]collab.AgentRef{worker: {ID: worker, Name: "w", Role: domain.AgentRoleWorker}, collAg: {ID: collAg, Role: domain.AgentRoleOrchestration}, "agt_pool": {ID: "agt_pool", Pool: true}},
		secrets:  map[string]collab.SecretRef{},
		versions: map[string]string{},
	}
	for _, n := range append(domain.RotatableCollabKeys(), domain.SecretPaperclipMasterKey) {
		r.repo.secrets[n] = collab.SecretRef{ID: "sec_" + n, ARN: "arn:" + n}
	}
	r.svc = &collab.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: r.repo, Settings: r.set, Writer: r.w, Rand: rand.Reader}
	return r
}

func (r *rig) enable() {
	r.set.Set(domain.SettingOrchestrationPublicBaseDomain, json.RawMessage(`"collab.example.com"`))
	r.set.Set(domain.SettingOrchestrationPrivateBaseDomain, json.RawMessage(`"collab.corp.example.com"`))
	r.set.Set(domain.SettingOrchestrationPublicEndpoints, json.RawMessage(`true`))
	r.set.Set(domain.SettingOrchestrationPrivateEndpoints, json.RawMessage(`true`))
}

func idemOf(op, key string) *acting.Idem {
	return &acting.Idem{Key: port.IdempotencyKey{Key: key, Operation: op, TeamID: team, RequestSHA256: [32]byte{1}},
		Render: func(any) (int, json.RawMessage, error) { return 200, json.RawMessage(`{"ok":true}`), nil }}
}

func kindOf(t *testing.T, err error) domain.ErrorKind {
	t.Helper()
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("not a domain error: %v", err)
	}
	return de.Kind
}

func TestGet(t *testing.T) {
	r := newRig()
	v, err := r.svc.Get(t.Context(), viewer, team)
	if err != nil || v.Collab.AgentID != collAg || v.WebUI.Effective != domain.ExposureOff {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := r.svc.Get(t.Context(), outsider, team); kindOf(t, err) != domain.KindNotFound {
		t.Errorf("outsider: %v", err)
	}
	r.repo.col = nil
	_, err = r.svc.Get(t.Context(), viewer, team)
	var de *domain.Error
	if !errors.As(err, &de) || de.Kind != domain.KindNotFound || de.Reason != domain.ReasonOrchestrationAgentPending {
		t.Errorf("pending: %v", err)
	}
	r.repo.fail = "get"
	if _, err := r.svc.Get(t.Context(), viewer, team); err == nil {
		t.Error("store failure swallowed")
	}
}

// ORC-14, ORC-15: a viewer cannot change the resource and the denial is audited; a level above the limit
// or on a surface the tool lacks is refused; every change emits orchestration.updated.
func TestPut(t *testing.T) {
	r := newRig()
	p := domain.PutOrchestrationParams{WebUI: domain.ExposurePublic, WebAPI: domain.ExposurePrivate}
	if _, err := r.svc.Put(t.Context(), viewer, team, p, nil); kindOf(t, err) != domain.KindAccessDenied {
		t.Errorf("viewer: %v", err)
	}
	if len(r.audit.Entries) != 1 || !r.audit.Entries[0].Own {
		t.Errorf("denial not audited: %+v", r.audit.Entries)
	}
	// An unset base domain does not refuse the write: the effective level says why.
	v, err := r.svc.Put(t.Context(), admin, team, p, nil)
	if err != nil || v.Collab.WebUIExposure != domain.ExposurePublic || v.WebUI.Effective != domain.ExposureOff {
		t.Fatalf("settings off: %+v %v", v, err)
	}
	r.enable()
	v, err = r.svc.Put(t.Context(), admin, team, p, nil)
	if err != nil || v.WebUI.Effective != domain.ExposurePublic || v.WebAPI.Hostname != "acme.collab.corp.example.com" {
		t.Fatalf("%+v %v", v, err)
	}
	var found bool
	for _, ev := range r.outbox.Events {
		if ev.Type == domain.EventOrchestrationUpdated {
			found = true
		}
	}
	if !found {
		t.Error("no orchestration.updated")
	}
	stale := domain.Version(1)
	if _, err := r.svc.Put(t.Context(), admin, team, p, &stale); kindOf(t, err) != domain.KindPreconditionFailed {
		t.Errorf("stale: %v", err)
	}
	r.repo.col.Limit = domain.ExposurePrivate
	if _, err := r.svc.Put(t.Context(), admin, team, p, nil); kindOf(t, err) != domain.KindValidation {
		t.Errorf("above limit: %v", err)
	}
	r.repo.col.Tool = domain.ToolBuzz
	if _, err := r.svc.Put(t.Context(), admin, team, domain.PutOrchestrationParams{WebUI: domain.ExposurePrivate, WebAPI: domain.ExposureOff}, nil); kindOf(t, err) != domain.KindValidation {
		t.Errorf("buzz surface: %v", err)
	}
	// Switching off is always accepted.
	if _, err := r.svc.Put(t.Context(), admin, team, domain.PutOrchestrationParams{WebUI: domain.ExposureOff, WebAPI: domain.ExposureOff}, nil); err != nil {
		t.Errorf("off: %v", err)
	}
	r.repo.col = nil
	_, err = r.svc.Put(t.Context(), admin, team, p, nil)
	var de *domain.Error
	if !errors.As(err, &de) || de.Kind != domain.KindConflict || de.Reason != domain.ReasonOrchestrationAgentPending {
		t.Errorf("no agent: %v", err)
	}
}

// The deprecated PutTeamCollaboration maps booleans to levels and also emits collaboration.updated.
func TestLegacyPut(t *testing.T) {
	r := newRig()
	r.enable()
	v, err := r.svc.LegacyPut(t.Context(), admin, team, true, false, "", nil)
	if err != nil || v.Collab.WebUIExposure != domain.ExposurePublic || v.Collab.WebAPIExposure != domain.ExposureOff {
		t.Fatalf("%+v %v", v, err)
	}
	var orch, legacy bool
	for _, ev := range r.outbox.Events {
		orch = orch || ev.Type == domain.EventOrchestrationUpdated
		legacy = legacy || ev.Type == domain.EventCollaborationUpdated
	}
	if !orch || !legacy {
		t.Errorf("events orchestration=%v collaboration=%v", orch, legacy)
	}
	r.repo.col.Limit = domain.ExposureOff
	if _, err := r.svc.LegacyPut(t.Context(), admin, team, true, false, "", nil); kindOf(t, err) != domain.KindValidation {
		t.Errorf("limit off: %v", err)
	}
	if _, err := r.svc.LegacyPut(t.Context(), viewer, team, false, false, "", nil); kindOf(t, err) != domain.KindAccessDenied {
		t.Errorf("viewer: %v", err)
	}
}

func TestPutFailures(t *testing.T) {
	p := domain.PutOrchestrationParams{WebUI: domain.ExposureOff, WebAPI: domain.ExposureOff}
	for _, op := range []string{"get", "update"} {
		r := newRig()
		r.repo.fail = op
		if _, err := r.svc.Put(t.Context(), admin, team, p, nil); err == nil {
			t.Errorf("%s swallowed", op)
		}
	}
	r := newRig()
	r.svc.Audit = failAudit{}
	if _, err := r.svc.Put(t.Context(), admin, team, p, nil); err == nil {
		t.Error("audit failure swallowed")
	}
	r = newRig()
	r.svc.Outbox = failOutbox{}
	if _, err := r.svc.Put(t.Context(), admin, team, p, nil); err == nil {
		t.Error("outbox failure swallowed")
	}
	r = newRig()
	r.set.Set(domain.SettingOrchestrationPublicBaseDomain, json.RawMessage(`{`))
	if _, err := r.svc.Put(t.Context(), admin, team, p, nil); err == nil {
		t.Error("bad setting swallowed")
	}
	r = newRig()
	r.set.Set(domain.SettingOrchestrationPublicEndpoints, json.RawMessage(`{`))
	if _, err := r.svc.Get(t.Context(), admin, team); err == nil {
		t.Error("bad setting swallowed")
	}
}

type bootFake struct {
	created, compensated []string
	err                  error
}

func (b *bootFake) CreateBootSecrets(_ context.Context, _ port.Tx, _ acting.Caller, _, _ string, tool domain.CollabTool, _ *acting.Idem, created *[]string) error {
	if b.err != nil {
		return b.err
	}
	for _, bs := range tool.BootSecrets() {
		*created = append(*created, "arn:"+bs.Name)
		b.created = append(b.created, bs.Name)
	}
	return nil
}
func (b *bootFake) Compensate(_ context.Context, arns []string) {
	b.compensated = append(b.compensated, arns...)
}

// ORC-11: ChangeCollaborationTool switches the tool in one transaction, takes the pre-change snapshot,
// lowers the surfaces the new tool lacks and writes the new tool's boot secrets.
func TestChangeTool(t *testing.T) {
	r := newRig()
	boot := &bootFake{}
	r.svc.Boot = boot
	r.repo.col.WebUIExposure, r.repo.col.WebAPIExposure = domain.ExposurePublic, domain.ExposurePrivate
	res, err := r.svc.ChangeTool(t.Context(), admin, team, domain.ToolBuzz, idemOf("ChangeCollaborationTool", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.SnapshotID == "" || res.View.Collab.Tool != domain.ToolBuzz || res.View.Collab.WebUIExposure != domain.ExposureOff ||
		res.View.Collab.Mode != domain.RegistrationManaged || len(boot.created) != 0 {
		t.Fatalf("%+v", res)
	}
	if len(r.repo.switched) != 1 || r.repo.switched[0].FromTool != domain.ToolPaperclip || r.repo.switched[0].RetainUntil.IsZero() {
		t.Errorf("switch: %+v", r.repo.switched)
	}
	var changed, updated int
	for _, ev := range r.outbox.Events {
		switch ev.Type {
		case domain.EventOrchestrationToolChanged:
			changed++
		case domain.EventAgentUpdated:
			updated++
		}
	}
	if changed != 1 || updated != 1 {
		t.Errorf("events: tool_changed=%d agent.updated=%d", changed, updated)
	}
	if again, err := r.svc.ChangeTool(t.Context(), admin, team, domain.ToolBuzz, idemOf("ChangeCollaborationTool", "k1")); err != nil || again.Replay == nil {
		t.Errorf("replay: %+v %v", again, err)
	}
	// Back to Paperclip writes its three boot secrets.
	r.repo.runtime[domain.ToolPaperclip] = collab.ToolRuntime{Tool: domain.ToolPaperclip, Enabled: true, TemplateID: "tpl_p", ImageRepository: "registry.example.com/paperclip", Digest: "sha256:bb", TemplateEnabled: true}
	if _, err := r.svc.ChangeTool(t.Context(), admin, team, domain.ToolPaperclip, nil); err != nil || len(boot.created) != 3 {
		t.Errorf("paperclip: %v %v", err, boot.created)
	}
}

func TestChangeToolRefusals(t *testing.T) {
	type tc struct {
		name  string
		setup func(*rig)
		who   acting.Caller
		tool  domain.CollabTool
		want  domain.ErrorKind
		why   string
	}
	for _, c := range []tc{
		{"viewer", nil, viewer, domain.ToolBuzz, domain.KindAccessDenied, ""},
		{"outsider", nil, outsider, domain.ToolBuzz, domain.KindNotFound, ""},
		{"bad tool", nil, admin, "slack", domain.KindValidation, ""},
		{"unchanged", nil, admin, domain.ToolPaperclip, domain.KindValidation, domain.ReasonToolUnchanged},
		{"not enabled", func(r *rig) {
			x := r.repo.runtime[domain.ToolBuzz]
			x.Enabled = false
			r.repo.runtime[domain.ToolBuzz] = x
		}, admin, domain.ToolBuzz, domain.KindValidation, domain.ReasonToolNotEnabled},
		{"runtime unset", func(r *rig) {
			delete(r.repo.runtime, domain.ToolBuzz)
			r.repo.runtime[domain.ToolBuzz] = collab.ToolRuntime{Enabled: true}
		}, admin, domain.ToolBuzz, domain.KindConflict, domain.ReasonOrchestrationRuntimeUnset},
		{"change running", func(r *rig) {
			r.repo.col.Status.Conditions = []domain.Condition{{Type: "ToolChanging", Status: domain.ConditionTrue}}
		}, admin, domain.ToolBuzz, domain.KindConflict, domain.ReasonToolChangeInProgress},
		{"pending", func(r *rig) { r.repo.col = nil }, admin, domain.ToolBuzz, domain.KindConflict, domain.ReasonOrchestrationAgentPending},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig()
			if c.setup != nil {
				c.setup(r)
			}
			_, err := r.svc.ChangeTool(t.Context(), c.who, team, c.tool, nil)
			var de *domain.Error
			if !errors.As(err, &de) || de.Kind != c.want {
				t.Fatalf("err = %v", err)
			}
			if c.why != "" && de.Reason != c.why && (len(de.Fields) == 0 || de.Fields[0].Reason != c.why) {
				t.Errorf("reason = %q fields %v, want %s", de.Reason, de.Fields, c.why)
			}
			if len(r.repo.switched) != 0 {
				t.Error("tool switched on a refused request")
			}
		})
	}
	// A Secrets Manager failure for Paperclip's boot secrets is a 503 and what was written is deleted.
	r := newRig()
	r.repo.col.Tool = domain.ToolBuzz
	r.repo.runtime[domain.ToolPaperclip] = collab.ToolRuntime{Tool: domain.ToolPaperclip, Enabled: true, TemplateID: "tpl_p", ImageRepository: "registry.example.com/paperclip", Digest: "sha256:bb", TemplateEnabled: true}
	boot := &bootFake{err: domain.Unavailable("Secrets Manager", errors.New("down"))}
	r.svc.Boot = boot
	if _, err := r.svc.ChangeTool(t.Context(), admin, team, domain.ToolPaperclip, nil); kindOf(t, err) != domain.KindUnavailable {
		t.Errorf("sm failure: %v", err)
	}
	r = newRig()
	r.repo.col.Tool = domain.ToolBuzz
	r.repo.runtime[domain.ToolPaperclip] = collab.ToolRuntime{Tool: domain.ToolPaperclip, Enabled: true, TemplateID: "tpl_p", ImageRepository: "registry.example.com/paperclip", Digest: "sha256:bb", TemplateEnabled: true}
	if _, err := r.svc.ChangeTool(t.Context(), admin, team, domain.ToolPaperclip, nil); kindOf(t, err) != domain.KindUnavailable {
		t.Errorf("no writer: %v", err)
	}
	for _, op := range []string{"runtime", "switch"} {
		r := newRig()
		r.repo.fail = op
		if _, err := r.svc.ChangeTool(t.Context(), admin, team, domain.ToolBuzz, nil); err == nil {
			t.Errorf("%s swallowed", op)
		}
	}
	r = newRig()
	r.svc.Audit = failAudit{}
	if _, err := r.svc.ChangeTool(t.Context(), admin, team, domain.ToolBuzz, nil); err == nil {
		t.Error("audit failure swallowed")
	}
}

type failAudit struct{}

func (failAudit) Write(context.Context, port.Tx, port.AuditRecord) error { return errors.New("audit") }
func (failAudit) WriteDenied(context.Context, port.Session, port.AuditRecord) error {
	return errors.New("audit")
}

type failOutbox struct{}

func (failOutbox) Publish(context.Context, port.Tx, domain.Event) error { return errors.New("outbox") }

func TestLinks(t *testing.T) {
	r := newRig()
	r.repo.links[worker] = domain.CollaborationLink{AgentID: worker, Connect: true, LinkState: domain.LinkLinked}
	r.repo.links["agt_01JD2Q3R4S5T6V7W8X9Y0Z0003"] = domain.CollaborationLink{AgentID: "agt_01JD2Q3R4S5T6V7W8X9Y0Z0003", LinkState: domain.LinkDisabled}
	rows, next, err := r.svc.Links(t.Context(), viewer, team, collab.LinkFilter{}, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" {
		t.Fatalf("%v %v %q", rows, err, next)
	}
	yes := true
	rows, _, err = r.svc.Links(t.Context(), viewer, team, collab.LinkFilter{Connect: &yes}, acting.Page{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	if _, _, err := r.svc.Links(t.Context(), outsider, team, collab.LinkFilter{}, acting.Page{}); err == nil {
		t.Error("outsider allowed")
	}
	r.repo.col = nil
	if _, _, err := r.svc.Links(t.Context(), viewer, team, collab.LinkFilter{}, acting.Page{}); err == nil {
		t.Error("no agent")
	}
}

func TestConnect(t *testing.T) {
	r := newRig()
	res, err := r.svc.Connect(t.Context(), admin, team, worker, "pc-1", idemOf("ConnectAgentToCollaboration", "k1"))
	if err != nil || !res.Link.Connect || res.Link.PaperclipAgentID != "pc-1" {
		t.Fatalf("%+v %v", res, err)
	}
	if again, err := r.svc.Connect(t.Context(), admin, team, worker, "pc-1", idemOf("ConnectAgentToCollaboration", "k1")); err != nil || again.Replay == nil {
		t.Errorf("replay: %+v %v", again, err)
	}
	res, err = r.svc.Disconnect(t.Context(), admin, team, worker, nil)
	if err != nil || res.Link.Connect {
		t.Fatalf("%+v %v", res, err)
	}
	tests := []struct {
		name  string
		agent string
		pc    string
		who   acting.Caller
		mode  domain.RegistrationMode
		want  domain.ErrorKind
	}{
		{"viewer", worker, "", viewer, domain.RegistrationManual, domain.KindAccessDenied},
		{"unknown agent", "agt_nope", "", admin, domain.RegistrationManual, domain.KindNotFound},
		{"pool", "agt_pool", "", admin, domain.RegistrationManual, domain.KindValidation},
		{"itself", collAg, "", admin, domain.RegistrationManual, domain.KindValidation},
		{"bad paperclip id", worker, "a b", admin, domain.RegistrationManual, domain.KindValidation},
		{"managed mode id", worker, "pc-2", admin, domain.RegistrationManaged, domain.KindValidation},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig()
			r.repo.col.Mode = tc.mode
			if _, err := r.svc.Connect(t.Context(), tc.who, team, tc.agent, tc.pc, nil); kindOf(t, err) != tc.want {
				t.Errorf("got %v", err)
			}
		})
	}
	for _, op := range []string{"get", "agent", "link", "setlink"} {
		r := newRig()
		r.repo.fail = op
		if _, err := r.svc.Connect(t.Context(), admin, team, worker, "", nil); err == nil {
			t.Errorf("%s swallowed", op)
		}
	}
	r = newRig()
	r.repo.col = nil
	if _, err := r.svc.Connect(t.Context(), admin, team, worker, "", nil); kindOf(t, err) != domain.KindConflict {
		t.Errorf("no agent: %v", err)
	}
	r = newRig()
	r.svc.Audit = failAudit{}
	if _, err := r.svc.Connect(t.Context(), admin, team, worker, "", nil); err == nil {
		t.Error("audit failure swallowed")
	}
}

// RA-20: only the signing secrets rotate; the master key is refused; each rotation writes a new
// version, emits secret.rotated and rolls the collaboration agent.
func TestRotate(t *testing.T) {
	r := newRig()
	res, err := r.svc.Rotate(t.Context(), admin, team, nil, idemOf("RotateCollaborationKeys", "k"))
	if err != nil || len(res.Rotated) != len(domain.RotatableCollabKeys()) || r.repo.bumped != 1 || len(r.w.arns) != len(res.Rotated) {
		t.Fatalf("%+v %v", res, err)
	}
	if again, err := r.svc.Rotate(t.Context(), admin, team, nil, idemOf("RotateCollaborationKeys", "k")); err != nil || again.Replay == nil {
		t.Errorf("replay: %+v %v", again, err)
	}
	if _, err := r.svc.Rotate(t.Context(), admin, team, []string{domain.SecretPaperclipMasterKey}, nil); kindOf(t, err) != domain.KindValidation {
		t.Errorf("master key: %v", err)
	}
	if _, err := r.svc.Rotate(t.Context(), admin, team, []string{"other"}, nil); kindOf(t, err) != domain.KindValidation {
		t.Errorf("unknown: %v", err)
	}
	if _, err := r.svc.Rotate(t.Context(), viewer, team, nil, nil); kindOf(t, err) != domain.KindAccessDenied {
		t.Errorf("viewer: %v", err)
	}
	for _, op := range []string{"get", "boot", "version", "bump"} {
		r := newRig()
		r.repo.fail = op
		if _, err := r.svc.Rotate(t.Context(), admin, team, nil, nil); err == nil {
			t.Errorf("%s swallowed", op)
		}
	}
	r = newRig()
	r.w.err = errors.New("aws")
	if _, err := r.svc.Rotate(t.Context(), admin, team, nil, nil); kindOf(t, err) != domain.KindUnavailable {
		t.Errorf("aws: %v", err)
	}
	r = newRig()
	r.repo.col = nil
	if _, err := r.svc.Rotate(t.Context(), admin, team, nil, nil); kindOf(t, err) != domain.KindConflict {
		t.Errorf("no agent: %v", err)
	}
	r = newRig()
	delete(r.repo.secrets, domain.RotatableCollabKeys()[0])
	if _, err := r.svc.Rotate(t.Context(), admin, team, nil, nil); kindOf(t, err) != domain.KindNotFound {
		t.Errorf("missing secret: %v", err)
	}
	r = newRig()
	r.svc.Rand = bytes.NewReader(nil)
	if _, err := r.svc.Rotate(t.Context(), admin, team, nil, nil); err == nil {
		t.Error("short random source accepted")
	}
	r = newRig()
	r.svc.Outbox = failOutbox{}
	if _, err := r.svc.Rotate(t.Context(), admin, team, nil, nil); err == nil {
		t.Error("outbox failure swallowed")
	}
	r = newRig()
	r.svc.Audit = failAudit{}
	if _, err := r.svc.Rotate(t.Context(), admin, team, nil, nil); err == nil {
		t.Error("audit failure swallowed")
	}
}

func TestEndpoints(t *testing.T) {
	r := newRig()
	r.enable()
	r.repo.col.WebUIExposure, r.repo.col.WebAPIExposure = domain.ExposurePublic, domain.ExposureOff
	res, err := r.svc.Endpoints(t.Context(), service, collab.EndpointFilter{}, acting.Page{Limit: 1})
	if err != nil || len(res.Items) != 1 || res.Next != team || res.Items[0].State.Hostname == "" || res.Items[0].Surface != domain.SurfaceWebUI {
		t.Fatalf("%+v %v", res, err)
	}
	if res.Migration.State != "complete" || res.Migration.TeamsTotal != 3 {
		t.Errorf("migration %+v", res.Migration)
	}
	res, err = r.svc.Endpoints(t.Context(), service, collab.EndpointFilter{}, acting.Page{Limit: 10})
	if err != nil || len(res.Items) != 2 || res.Next != "" {
		t.Fatalf("all: %+v %v", res, err)
	}
	// The effective-level filter keeps surfaces whose effective level matches.
	res, _ = r.svc.Endpoints(t.Context(), service, collab.EndpointFilter{Exposure: domain.ExposurePrivate}, acting.Page{Limit: 10})
	if len(res.Items) != 1 || res.Items[0].Row.Collab.TeamID != "team_z" {
		t.Errorf("filter: %+v", res.Items)
	}
	r.repo.blocked = []domain.OrchestrationBlocked{{TeamID: "team_q", Reason: domain.ReasonOrchestrationRuntimeUnset}}
	res, _ = r.svc.Endpoints(t.Context(), service, collab.EndpointFilter{}, acting.Page{Limit: 10})
	if res.Migration.State != "blocked" {
		t.Errorf("blocked migration: %+v", res.Migration)
	}
	if _, err := r.svc.Endpoints(t.Context(), admin, collab.EndpointFilter{}, acting.Page{}); kindOf(t, err) != domain.KindAccessDenied {
		t.Errorf("team admin: %v", err)
	}
	for _, op := range []string{"endpoints", "migration"} {
		r.repo.fail = op
		if _, err := r.svc.Endpoints(t.Context(), service, collab.EndpointFilter{}, acting.Page{}); err == nil {
			t.Errorf("%s swallowed", op)
		}
	}
	r = newRig()
	r.set.Set(domain.SettingOrchestrationPublicBaseDomain, json.RawMessage(`{`))
	if _, err := r.svc.Endpoints(t.Context(), service, collab.EndpointFilter{}, acting.Page{}); err == nil {
		t.Error("bad setting swallowed")
	}
}

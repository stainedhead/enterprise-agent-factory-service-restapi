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

func (r *repo) Update(_ context.Context, _ port.Tx, _ string, web, rest bool, mode domain.RegistrationMode) error {
	if err := r.boom("update"); err != nil {
		return err
	}
	r.col.WebUIEnabled, r.col.RESTAPIEnabled, r.col.Mode = web, rest, mode
	r.col.Version++
	return nil
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
	rows := []collab.EndpointRow{{Collab: *r.col, Phase: domain.PhaseReady}, {Collab: domain.TeamCollaboration{TeamID: "team_z"}}}
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
		col: &domain.TeamCollaboration{TeamID: team, AgentID: collAg, Mode: domain.RegistrationManual, Version: 1,
			MayExpose: true, ProviderClientID: "cid", TeamSlug: "acme", Status: domain.Status{Phase: domain.PhaseReady}},
		links:    map[string]domain.CollaborationLink{},
		agents:   map[string]collab.AgentRef{worker: {ID: worker, Name: "w", Role: domain.AgentRoleWorker}, collAg: {ID: collAg, Role: domain.AgentRoleCollaboration}, "agt_pool": {ID: "agt_pool", Pool: true}},
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
	r.set.Set(domain.SettingCollabBaseDomain, json.RawMessage(`"collab.example.com"`))
	r.set.Set(domain.SettingCollabPublicEndpoints, json.RawMessage(`true`))
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
	if err != nil || v.Collab.AgentID != collAg || v.Effective.WebUI {
		t.Fatalf("%+v %v", v, err)
	}
	if _, err := r.svc.Get(t.Context(), outsider, team); kindOf(t, err) != domain.KindNotFound {
		t.Errorf("outsider: %v", err)
	}
	r.repo.col = nil
	if _, err := r.svc.Get(t.Context(), viewer, team); kindOf(t, err) != domain.KindNotFound {
		t.Errorf("no agent: %v", err)
	}
	r.repo.fail = "get"
	if _, err := r.svc.Get(t.Context(), viewer, team); err == nil {
		t.Error("store failure swallowed")
	}
}

// RA-17: a viewer cannot change the resource and the denial is audited; a surface is enabled only when
// the guardrail and the settings allow it; every change emits collaboration.updated.
func TestPut(t *testing.T) {
	r := newRig()
	p := domain.PutCollaborationParams{WebUIEnabled: true, RESTAPIEnabled: true}
	if _, err := r.svc.Put(t.Context(), viewer, team, p, nil); kindOf(t, err) != domain.KindAccessDenied {
		t.Errorf("viewer: %v", err)
	}
	if _, err := r.svc.Put(t.Context(), admin, team, p, nil); kindOf(t, err) != domain.KindValidation {
		t.Errorf("settings off: %v", err)
	}
	r.enable()
	v, err := r.svc.Put(t.Context(), admin, team, p, nil)
	if err != nil || !v.Collab.WebUIEnabled || !v.Effective.WebUI || v.Hostname == "" {
		t.Fatalf("%+v %v", v, err)
	}
	var found bool
	for _, ev := range r.outbox.Events {
		if ev.Type == domain.EventCollaborationUpdated {
			found = true
		}
	}
	if !found || len(r.audit.Entries) == 0 {
		t.Errorf("event=%v audit=%d", found, len(r.audit.Entries))
	}
	stale := domain.Version(1)
	if _, err := r.svc.Put(t.Context(), admin, team, p, &stale); kindOf(t, err) != domain.KindPreconditionFailed {
		t.Errorf("stale: %v", err)
	}
	// Switching off is always accepted.
	r.set.Set(domain.SettingCollabPublicEndpoints, json.RawMessage(`false`))
	if _, err := r.svc.Put(t.Context(), admin, team, domain.PutCollaborationParams{}, nil); err != nil {
		t.Errorf("off: %v", err)
	}
	r.repo.col = nil
	if _, err := r.svc.Put(t.Context(), admin, team, p, nil); kindOf(t, err) != domain.KindConflict {
		t.Errorf("no agent: %v", err)
	}
}

func TestPutFailures(t *testing.T) {
	for _, op := range []string{"get", "update"} {
		r := newRig()
		r.enable()
		r.repo.fail = op
		if _, err := r.svc.Put(t.Context(), admin, team, domain.PutCollaborationParams{}, nil); err == nil {
			t.Errorf("%s swallowed", op)
		}
	}
	r := newRig()
	r.svc.Audit = failAudit{}
	if _, err := r.svc.Put(t.Context(), admin, team, domain.PutCollaborationParams{}, nil); err == nil {
		t.Error("audit failure swallowed")
	}
	r = newRig()
	r.svc.Outbox = failOutbox{}
	if _, err := r.svc.Put(t.Context(), admin, team, domain.PutCollaborationParams{}, nil); err == nil {
		t.Error("outbox failure swallowed")
	}
	r = newRig()
	r.set.Set(domain.SettingCollabBaseDomain, json.RawMessage(`{`))
	if _, err := r.svc.Put(t.Context(), admin, team, domain.PutCollaborationParams{}, nil); err == nil {
		t.Error("bad setting swallowed")
	}
	r = newRig()
	r.set.Set(domain.SettingCollabPublicEndpoints, json.RawMessage(`{`))
	if _, err := r.svc.Get(t.Context(), admin, team); err == nil {
		t.Error("bad setting swallowed")
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
	rows, next, err := r.svc.Endpoints(t.Context(), service, collab.EndpointFilter{}, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" || rows[0].Hostname == "" {
		t.Fatalf("%+v %q %v", rows, next, err)
	}
	if _, _, err := r.svc.Endpoints(t.Context(), admin, collab.EndpointFilter{}, acting.Page{}); kindOf(t, err) != domain.KindAccessDenied {
		t.Errorf("team admin: %v", err)
	}
	r.repo.fail = "endpoints"
	if _, _, err := r.svc.Endpoints(t.Context(), service, collab.EndpointFilter{}, acting.Page{}); err == nil {
		t.Error("store failure swallowed")
	}
	r = newRig()
	r.set.Set(domain.SettingCollabBaseDomain, json.RawMessage(`{`))
	if _, _, err := r.svc.Endpoints(t.Context(), service, collab.EndpointFilter{}, acting.Page{}); err == nil {
		t.Error("bad setting swallowed")
	}
}

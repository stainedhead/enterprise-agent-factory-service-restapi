package secrets_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/secrets/secretstest"
)

const (
	team  = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	agent = "agt_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	value = "s3cr3t-value-DO-NOT-LEAK"
)

type rig struct {
	svc    *secrets.Service
	repo   *secretstest.Repo
	w      *secretstest.Writer
	audit  *fakes.Audit
	outbox *fakes.Outbox
	tx     *fakes.TxRunner
	orphan int
}

func newRig() *rig {
	r := &rig{repo: secretstest.NewRepo(), w: secretstest.NewWriter(), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}, tx: &fakes.TxRunner{}}
	r.repo.Agents[agent] = "coder"
	r.svc = &secrets.Service{Deps: acting.Deps{Tx: r.tx, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: r.repo, Writer: r.w, OnOrphan: func() { r.orphan++ }}
	return r
}

var admin = acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser,
	Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: team}}}, RequestID: "req-1"}

func params(name string) domain.NewSecretParams {
	v, _ := domain.NewSecretValue("value", value)
	return domain.NewSecretParams{Scope: domain.SecretScopeTeam, Name: name, Value: v}
}

// noLeak fails if the value appears in anything the service wrote or returned.
func (r *rig) noLeak(t *testing.T, things ...any) {
	t.Helper()
	all := append([]any{}, things...)
	for _, a := range r.audit.Entries {
		all = append(all, a)
	}
	for _, e := range r.outbox.Events {
		all = append(all, e)
	}
	for _, x := range all {
		b, err := json.Marshal(x)
		if err != nil {
			b = []byte(fmt.Sprintf("%+v", x))
		}
		if strings.Contains(string(b), value) || strings.Contains(fmt.Sprintf("%+v", x), value) {
			t.Errorf("secret value leaked in %T: %s", x, b)
		}
	}
}

func TestCreateTeamAndAgentSecrets(t *testing.T) {
	r := newRig()
	res, err := r.svc.Create(t.Context(), admin, team, params("github-pat"), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Secret
	if s.Path != "afs/ns/_shared/github-pat" || !strings.Contains(s.ARN, s.Path) || s.Version != 1 || s.VersionID == "" {
		t.Errorf("secret: %+v", s)
	}
	if got := r.w.Tags[s.Path]; got["afs-team-id"] != team || got["afs-agent-id"] != "" {
		t.Errorf("tags: %v", got)
	}
	p := params("agent-key")
	p.Scope, p.AgentID = domain.SecretScopeAgent, agent
	a, err := r.svc.Create(t.Context(), admin, team, p, nil)
	if err != nil || a.Secret.Path != "afs/ns/coder/agent-key" || r.w.Tags[a.Secret.Path]["afs-agent-id"] != agent {
		t.Fatalf("agent secret: %+v %v tags=%v", a, err, r.w.Tags)
	}
	if len(r.audit.Entries) != 2 || len(r.outbox.Events) != 2 || r.outbox.Events[0].Type != domain.EventSecretCreated {
		t.Errorf("audit=%d outbox=%d", len(r.audit.Entries), len(r.outbox.Events))
	}
	r.noLeak(t, res, a)
}

func TestCreateRefusals(t *testing.T) {
	r := newRig()
	if _, err := r.svc.Create(t.Context(), admin, team, params("dup"), nil); err != nil {
		t.Fatal(err)
	}
	bad := params("Bad Name")
	unknownAgent := params("x")
	unknownAgent.Scope, unknownAgent.AgentID = domain.SecretScopeAgent, "agt_01JD2Q3R4S5T6V7W8X9Y0Z0009"
	tests := []struct {
		name string
		in   domain.NewSecretParams
		want domain.ErrorKind
	}{
		{"duplicate name", params("dup"), domain.KindConflict},
		{"invalid", bad, domain.KindValidation},
		{"unknown agent", unknownAgent, domain.KindValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(r.w.Calls)
			_, err := r.svc.Create(t.Context(), admin, team, tt.in, nil)
			if domain.KindOf(err) != tt.want {
				t.Fatalf("err = %v", err)
			}
			if tt.name == "duplicate name" && len(r.w.Calls) != before {
				t.Error("Secrets Manager must not be called for a taken name")
			}
		})
	}
	r.noLeak(t)
}

func TestCreateSecretsManagerFailureWritesNoRow(t *testing.T) {
	r := newRig()
	r.w.CreateErr = errors.New("connect: " + value) // even a misbehaving SDK error must not reach the client
	_, err := r.svc.Create(t.Context(), admin, team, params("x"), nil)
	var de *domain.Error
	if !errors.As(err, &de) || de.Kind != domain.KindUnavailable {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(de.Message, value) {
		t.Error("message carries the value")
	}
	if len(r.repo.Rows) != 0 || len(r.audit.Entries) != 0 || len(r.outbox.Events) != 0 {
		t.Error("a failed Secrets Manager call must write nothing")
	}
}

func TestCreateDatabaseFailureCompensates(t *testing.T) {
	tests := []struct {
		name             string
		forceErr, tagErr error
		wantCalls        []string
		wantOrphans      int
	}{
		{"deleted at once", nil, nil, []string{"create", "force-delete"}, 0},
		{"tagged when delete fails", errors.New("x"), nil, []string{"create", "force-delete", "tag-orphan"}, 1},
		{"counted even when tagging fails", errors.New("x"), errors.New("y"), []string{"create", "force-delete", "tag-orphan"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig()
			r.repo.InsertErr = errors.New("db down")
			r.w.ForceDeleteErr, r.w.TagErr = tt.forceErr, tt.tagErr
			if _, err := r.svc.Create(t.Context(), admin, team, params("x"), nil); err == nil {
				t.Fatal("expected error")
			}
			if strings.Join(r.w.Calls, ",") != strings.Join(tt.wantCalls, ",") || r.orphan != tt.wantOrphans {
				t.Errorf("calls=%v orphans=%d", r.w.Calls, r.orphan)
			}
			if tt.forceErr == nil && len(r.w.ByName) != 0 {
				t.Error("the new secret must be gone")
			}
			if tt.forceErr != nil && tt.tagErr == nil && len(r.w.Orphaned) != 1 {
				t.Error("the secret must be tagged")
			}
		})
	}
}

func TestCreateRetryDoesNotCreateSecondSecret(t *testing.T) {
	r := newRig()
	key := port.IdempotencyKey{Key: "k1", Operation: "CreateSecret", TeamID: team}
	mk := func() *acting.Idem {
		return &acting.Idem{Key: key, Render: func(any) (int, json.RawMessage, error) { return 201, json.RawMessage(`{"id":"x"}`), nil }}
	}
	// First attempt fails after Secrets Manager succeeded.
	r.repo.InsertErr = errors.New("db down")
	r.w.ForceDeleteErr = errors.New("cannot delete")
	if _, err := r.svc.Create(t.Context(), admin, team, params("x"), mk()); err == nil {
		t.Fatal("expected error")
	}
	// The retry (same key) gets the same secret back from Secrets Manager.
	r.repo.InsertErr, r.w.ForceDeleteErr = nil, nil
	res, err := r.svc.Create(t.Context(), admin, team, params("x"), mk())
	if err != nil || len(r.w.ByName) != 1 || res.Secret.ARN != r.w.ByName["afs/ns/_shared/x"].ARN {
		t.Fatalf("%+v %v names=%v", res, err, r.w.ByName)
	}
	// A third call with the same key is a replay and does not touch Secrets Manager.
	n := len(r.w.Calls)
	again, err := r.svc.Create(t.Context(), admin, team, params("x"), mk())
	if err != nil || again.Replay == nil || len(r.w.Calls) != n {
		t.Fatalf("replay: %+v %v", again, err)
	}
}

func TestPutValueAndRotate(t *testing.T) {
	r := newRig()
	c, _ := r.svc.Create(t.Context(), admin, team, params("tok"), nil)
	id := c.Secret.ID
	v2, _ := domain.NewSecretValue("value", value+"-2")
	if err := r.svc.PutValue(t.Context(), admin, team, id, nil, v2); err != nil {
		t.Fatal(err)
	}
	cur, _ := r.svc.Get(t.Context(), admin, team, id)
	if cur.VersionID == c.Secret.VersionID || cur.Version != 2 {
		t.Errorf("metadata not updated: %+v", cur)
	}
	stale := domain.Version(1)
	if err := r.svc.PutValue(t.Context(), admin, team, id, &stale, v2); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale: %v", err)
	}
	rot, err := r.svc.Rotate(t.Context(), admin, team, id, v2, nil)
	if err != nil || rot.Secret.VersionID == cur.VersionID {
		t.Fatalf("rotate: %+v %v", rot, err)
	}
	if last := r.outbox.Events[len(r.outbox.Events)-1]; last.Type != domain.EventSecretRotated {
		t.Errorf("event %s", last.Type)
	}
	if _, err := r.svc.Rotate(t.Context(), admin, team, "sec_01JD2Q3R4S5T6V7W8X9Y0Z0009", v2, nil); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown: %v", err)
	}
	if err := r.svc.PutValue(t.Context(), admin, team, id, nil, domain.SecretValue{}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("empty: %v", err)
	}
	r.noLeak(t, c, cur, rot)
}

func TestPutAndRotateSecretsManagerFailureLeavesMetadata(t *testing.T) {
	r := newRig()
	c, _ := r.svc.Create(t.Context(), admin, team, params("tok"), nil)
	r.w.PutErr = errors.New("down")
	v2, _ := domain.NewSecretValue("value", "other")
	before, auditN, outN := r.repo.Rows[c.Secret.ID], len(r.audit.Entries), len(r.outbox.Events)
	if err := r.svc.PutValue(t.Context(), admin, team, c.Secret.ID, nil, v2); domain.KindOf(err) != domain.KindUnavailable {
		t.Fatalf("put: %v", err)
	}
	if _, err := r.svc.Rotate(t.Context(), admin, team, c.Secret.ID, v2, nil); domain.KindOf(err) != domain.KindUnavailable {
		t.Fatalf("rotate: %v", err)
	}
	if r.repo.Rows[c.Secret.ID].VersionID != before.VersionID || len(r.audit.Entries) != auditN || len(r.outbox.Events) != outN {
		t.Error("metadata, audit or events changed")
	}
}

func TestUpdateListGetDelete(t *testing.T) {
	r := newRig()
	c, _ := r.svc.Create(t.Context(), admin, team, params("tok"), nil)
	id := c.Secret.ID
	d := "new description"
	u, err := r.svc.Update(t.Context(), admin, team, id, 1, domain.SecretPatch{Description: &d, Rotation: &domain.Rotation{Enabled: true, Days: 30}})
	if err != nil || u.Description != d || u.Version != 2 || u.Rotation.Days != 30 {
		t.Fatalf("%+v %v", u, err)
	}
	if _, err := r.svc.Update(t.Context(), admin, team, id, 1, domain.SecretPatch{Description: &d}); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale update: %v", err)
	}
	if _, err := r.svc.Update(t.Context(), admin, team, id, 2, domain.SecretPatch{}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("empty patch: %v", err)
	}
	rows, next, err := r.svc.List(t.Context(), admin, team, "", acting.Page{Limit: 10})
	if err != nil || len(rows) != 1 || next != "" {
		t.Fatalf("list %v %v %q", rows, err, next)
	}
	if _, err := r.svc.Get(t.Context(), admin, "team_01JD2Q3R4S5T6V7W8X9Y0Z0002", id); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("other team: %v", err)
	}
	// Bound secret: 409 listing the agents.
	s := r.repo.Rows[id]
	s.BoundTo = []string{agent}
	r.repo.Rows[id] = s
	err = r.svc.Delete(t.Context(), admin, team, id, s.Version)
	var de *domain.Error
	if !errors.As(err, &de) || de.Kind != domain.KindConflict || de.Details["agentIds"] == nil {
		t.Fatalf("bound delete: %v", err)
	}
	s.BoundTo = nil
	r.repo.Rows[id] = s
	if err := r.svc.Delete(t.Context(), admin, team, id, 99); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale delete: %v", err)
	}
	if err := r.svc.Delete(t.Context(), admin, team, id, s.Version); err != nil {
		t.Fatal(err)
	}
	if len(r.w.Scheduled) != 1 || len(r.repo.Rows) != 0 || r.outbox.Events[len(r.outbox.Events)-1].Type != domain.EventSecretDeleted {
		t.Errorf("scheduled=%v rows=%d", r.w.Scheduled, len(r.repo.Rows))
	}
	// A failed scheduling does not fail the delete (the sweep catches it).
	c2, _ := r.svc.Create(t.Context(), admin, team, params("two"), nil)
	r.w.ScheduleErr = errors.New("x")
	if err := r.svc.Delete(t.Context(), admin, team, c2.Secret.ID, 1); err != nil {
		t.Errorf("delete: %v", err)
	}
	r.noLeak(t, u)
}

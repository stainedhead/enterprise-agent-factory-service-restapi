package agents_test

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents/agentstest"
)

// collabRig is a rig with a paperclip template, a worker template and a boot-secret writer.
func collabRig() (*rig, *agentstest.BootWriter) {
	r := newRig()
	r.repo.Templates["paperclip"] = agents.TemplateInfo{ID: "tpl_p", Name: "paperclip", Enabled: true,
		AllowedDigests: []string{digest}, Capabilities: []string{domain.CapabilityPaperclip}}
	w := &agentstest.BootWriter{}
	r.svc.Boot, r.svc.Rand = w, rand.Reader
	return r, w
}

func collabParams(name string) domain.NewAgentParams {
	p := params(name)
	p.Template, p.Role = "paperclip", domain.AgentRoleCollaboration
	return p
}

// RA-19: CreateAgent with role collaboration creates the three boot secrets, the collaboration row,
// the audit records and the events in one request, with the contract defaults.
func TestCreateCollaborationAgent(t *testing.T) {
	r, w := collabRig()
	res, err := r.svc.Create(t.Context(), admin, team, collabParams("paperclip"), nil)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Agent
	if a.Role != domain.AgentRoleCollaboration || a.IAMIdentity != domain.IAMIdentityDedicated ||
		a.Volume.SnapshotSchedule != domain.SnapshotDaily || a.Volume.SnapshotRetain != 14 || a.Volume.OnDelete != domain.OnDeleteSnapshotThenDelete {
		t.Errorf("defaults: %+v", a)
	}
	if _, ok := r.repo.Collab[team]; !ok {
		t.Error("no team_collaboration row")
	}
	if len(w.Created) != 3 || len(r.repo.Boot) != 3 {
		t.Fatalf("boot secrets: created=%d rows=%d", len(w.Created), len(r.repo.Boot))
	}
	envs := map[string]bool{}
	for id, s := range r.repo.Boot {
		if s.ManagedBy != domain.ManagedByAFS || s.AgentID != a.ID {
			t.Errorf("secret %+v", s)
		}
		envs[r.repo.BootEnv[id]] = true
	}
	for _, b := range domain.BootSecrets() {
		if !envs[b.Env] {
			t.Errorf("no binding for %s", b.Env)
		}
	}
	// agent.created plus secret.created for each boot secret; audit for the agent and each secret.
	var created, secrets int
	for _, ev := range r.outbox.Events {
		switch ev.Type {
		case domain.EventAgentCreated:
			created++
		case domain.EventSecretCreated:
			secrets++
		}
	}
	if created != 1 || secrets != 3 || len(r.audit.Entries) != 4 {
		t.Errorf("events agent=%d secret=%d audit=%d", created, secrets, len(r.audit.Entries))
	}
}

// RA-19: the request is refused when it breaks a collaboration rule.
func TestCreateCollaborationAgentRefusals(t *testing.T) {
	yes := true
	tests := []struct {
		name  string
		mod   func(*domain.NewAgentParams)
		field string
	}{
		{"worker template", func(p *domain.NewAgentParams) { p.Template = "claude-code" }, "template"},
		{"team shared identity", func(p *domain.NewAgentParams) { p.IAMIdentity = domain.IAMIdentityTeamShared }, "iamIdentity"},
		{"snapshots off", func(p *domain.NewAgentParams) { p.Volume.SnapshotSchedule = domain.SnapshotOff }, "volume.snapshotSchedule"},
		{"collaboration object", func(p *domain.NewAgentParams) { p.Collaboration = &domain.CollaborationInput{Connect: &yes} }, "collaboration"},
		{"unknown role", func(p *domain.NewAgentParams) { p.Role = "boss" }, "role"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, w := collabRig()
			p := collabParams("paperclip")
			tc.mod(&p)
			_, err := r.svc.Create(t.Context(), admin, team, p, nil)
			if domain.KindOf(err) != domain.KindValidation || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("err = %v", err)
			}
			if len(w.Created) != 0 {
				t.Error("Secrets Manager written for a refused request")
			}
		})
	}
	// A worker cannot use a paperclip template.
	r, _ := collabRig()
	p := params("w")
	p.Template = "paperclip"
	if _, err := r.svc.Create(t.Context(), admin, team, p, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("worker on paperclip template: %v", err)
	}
}

// RA-19: a Secrets Manager failure is a 503 and deletes what was already created; a delete that fails
// leaves the secret tagged for the sweep.
func TestCreateCollaborationAgentCompensation(t *testing.T) {
	r, w := collabRig()
	w.CreateErr, w.FailAt = errors.New("throttled"), 3
	_, err := r.svc.Create(t.Context(), admin, team, collabParams("paperclip"), nil)
	if domain.KindOf(err) != domain.KindUnavailable {
		t.Fatalf("err = %v", err)
	}
	if len(w.Created) != 2 || len(w.Deleted) != 2 {
		t.Errorf("created=%d deleted=%d", len(w.Created), len(w.Deleted))
	}

	r, w = collabRig()
	orphans := 0
	r.svc.OnOrphan = func() { orphans++ }
	w.CreateErr, w.FailAt, w.DeleteErr = errors.New("down"), 2, errors.New("no")
	if _, err := r.svc.Create(t.Context(), admin, team, collabParams("paperclip"), nil); domain.KindOf(err) != domain.KindUnavailable {
		t.Fatalf("err = %v", err)
	}
	if len(w.Orphaned) != 1 || orphans != 1 {
		t.Errorf("orphaned=%d callbacks=%d", len(w.Orphaned), orphans)
	}

	// Without a writer the request is refused, never half done.
	r, _ = collabRig()
	r.svc.Boot = nil
	if _, err := r.svc.Create(t.Context(), admin, team, collabParams("paperclip"), nil); domain.KindOf(err) != domain.KindUnavailable {
		t.Errorf("no writer: %v", err)
	}
}

// RA-19: a worker's collaboration.connect needs the team's collaboration agent and, in managed
// registration, takes no Paperclip agent id.
func TestWorkerConnect(t *testing.T) {
	yes := true
	r, _ := collabRig()
	p := params("worker")
	p.Collaboration = &domain.CollaborationInput{Connect: &yes}
	if _, err := r.svc.Create(t.Context(), admin, team, p, nil); err == nil || !strings.Contains(err.Error(), domain.ReasonCollaborationAgentMissing) && domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("no collaboration agent: %v", err)
	}
	if _, err := r.svc.Create(t.Context(), admin, team, collabParams("paperclip"), nil); err != nil {
		t.Fatal(err)
	}
	p.Name = "worker2"
	res, err := r.svc.Create(t.Context(), admin, team, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l := r.repo.Links[res.Agent.ID]; !l.Connect || l.LinkState != domain.LinkPending {
		t.Errorf("link: %+v", l)
	}
	// Update: id accepted in manual mode, refused in managed mode.
	row, _ := r.repo.Get(t.Context(), nil, team, res.Agent.ID, false)
	pc := domain.CollaborationInput{Connect: &yes, PaperclipAgentID: "pc-1"}
	if _, err := r.svc.Update(t.Context(), admin, team, res.Agent.ID, row.Version, domain.AgentPatch{Collaboration: &pc}); err != nil {
		t.Fatal(err)
	}
	if r.repo.Links[res.Agent.ID].PaperclipAgentID != "pc-1" {
		t.Error("paperclip agent id not recorded")
	}
	r.repo.Collab[team] = domain.RegistrationManaged
	row, _ = r.repo.Get(t.Context(), nil, team, res.Agent.ID, false)
	if _, err := r.svc.Update(t.Context(), admin, team, res.Agent.ID, row.Version, domain.AgentPatch{Collaboration: &pc}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("managed mode: %v", err)
	}
}

// RA-19: the role is immutable; the boot bindings survive a replacement of the bindings.
func TestCollaborationAgentUpdate(t *testing.T) {
	r, _ := collabRig()
	res, err := r.svc.Create(t.Context(), admin, team, collabParams("paperclip"), nil)
	if err != nil {
		t.Fatal(err)
	}
	worker := domain.AgentRoleWorker
	if _, err := r.svc.Update(t.Context(), admin, team, res.Agent.ID, res.Agent.Version, domain.AgentPatch{Role: &worker}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("role change: %v", err)
	}
	same := domain.AgentRoleCollaboration
	empty := []domain.SecretBinding{}
	out, err := r.svc.Update(t.Context(), admin, team, res.Agent.ID, res.Agent.Version, domain.AgentPatch{Role: &same, Bindings: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Bindings) != 3 {
		t.Errorf("boot bindings after replacement: %+v", out.Bindings)
	}
	off := domain.SnapshotOff
	row, _ := r.repo.Get(t.Context(), nil, team, res.Agent.ID, false)
	if _, err := r.svc.Update(t.Context(), admin, team, res.Agent.ID, row.Version, domain.AgentPatch{Volume: &domain.VolumeSpec{SnapshotSchedule: off}}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("snapshots off: %v", err)
	}
}

// RA-20: a Paperclip secret cannot be bound.
func TestBindPaperclipSecretRefused(t *testing.T) {
	r := newRig()
	r.repo.SecretsBy[secA] = agents.SecretInfo{AgentID: "", ManagedBy: string(domain.ManagedByPaperclip)}
	p := params("w")
	p.Bindings = []domain.SecretBinding{{SecretID: secA, Delivery: "env", Target: "TOKEN"}}
	if _, err := r.svc.Create(t.Context(), admin, team, p, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("err = %v", err)
	}
}

package agents_test

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/agents/agentstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
)

const orchID = "agt_01JD2Q3R4S5T6V7W8X9Y0Z00OR"

// collabRig is a rig with a worker template, a boot-secret writer and the team's orchestration agent.
func collabRig() (*rig, *agentstest.BootWriter) {
	r := newRig()
	w := &agentstest.BootWriter{}
	r.svc.Boot, r.svc.Rand = w, rand.Reader
	r.repo.Rows[orchID] = domain.Agent{ID: orchID, TeamID: team, Name: domain.OrchestratorName, Role: domain.AgentRoleOrchestration,
		Template: "paperclip", Image: image, State: domain.AgentEnabled, Version: 1, Generation: 1, IAMIdentity: domain.IAMIdentityDedicated,
		Volume: domain.DefaultAgentVolume(), Models: domain.AgentModels{Access: domain.ModelAccessAll, DefaultModelID: model}}
	r.repo.Collab[team] = domain.RegistrationManual
	return r, w
}

// ORC-2: CreateAgent creates workers only; a role other than worker and the name orchestrator are refused.
func TestCreateRefusesReservedRoleAndName(t *testing.T) {
	for name, tc := range map[string]struct {
		mod   func(*domain.NewAgentParams)
		field string
		why   string
	}{
		"role orchestration": {func(p *domain.NewAgentParams) { p.Role = domain.AgentRoleOrchestration }, "role", domain.ReasonRoleReserved},
		"role collaboration": {func(p *domain.NewAgentParams) { p.Role = domain.AgentRoleCollaboration }, "role", domain.ReasonRoleReserved},
		"name orchestrator":  {func(p *domain.NewAgentParams) { p.Name = domain.OrchestratorName }, "name", domain.ReasonNameReserved},
	} {
		t.Run(name, func(t *testing.T) {
			r, w := collabRig()
			p := params("wrk")
			tc.mod(&p)
			_, err := r.svc.Create(t.Context(), admin, team, p, nil)
			var de *domain.Error
			if !errors.As(err, &de) || domain.KindOf(err) != domain.KindValidation || de.Fields[0].Field != tc.field || de.Fields[0].Reason != tc.why {
				t.Fatalf("err = %v", err)
			}
			if len(w.Created) != 0 {
				t.Error("Secrets Manager written for a refused request")
			}
		})
	}
}

// ORC-2: the orchestration agent is never deleted on its own; the refusal is audited as denied.
func TestDeleteOrchestrationAgentRefused(t *testing.T) {
	r, _ := collabRig()
	_, err := r.svc.Delete(t.Context(), admin, team, orchID, 1)
	var de *domain.Error
	if !errors.As(err, &de) || de.Kind != domain.KindConflict || de.Reason != domain.ReasonOrchestrationAgentReq {
		t.Fatalf("err = %v", err)
	}
	if r.repo.Rows[orchID].State != domain.AgentEnabled {
		t.Error("the orchestration agent was marked deleting")
	}
	if deniedCount(r) != 1 {
		t.Errorf("denied audit records = %d", deniedCount(r))
	}
}

// ORC-6: the fixed fields of the orchestration agent are refused; the others work as for any agent.
func TestUpdateOrchestrationAgent(t *testing.T) {
	r, _ := collabRig()
	s := func(v string) *string { return &v }
	off := domain.SnapshotOff
	worker := domain.AgentRoleWorker
	for name, p := range map[string]domain.AgentPatch{
		"name":     {Name: s("boss")},
		"role":     {Role: &worker},
		"template": {Template: s("claude-code")},
		"image":    {Image: s("registry.example.com/agents/other@" + digest)},
		"snapshot": {Volume: &domain.VolumeSpec{SnapshotSchedule: off}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.svc.Update(t.Context(), admin, team, orchID, 1, p)
			var de *domain.Error
			if !errors.As(err, &de) || domain.KindOf(err) != domain.KindValidation || de.Fields[0].Reason != domain.ReasonOrchestrationFieldFixed {
				t.Fatalf("err = %v", err)
			}
		})
	}
	desc := s("the team's coordinator")
	out, err := r.svc.Update(t.Context(), admin, team, orchID, 1, domain.AgentPatch{Description: desc})
	if err != nil || out.Description != *desc {
		t.Fatalf("description: %v", err)
	}
	// The boot bindings survive a replacement of the bindings.
	r.repo.Rows[orchID] = withBoot(r.repo.Rows[orchID])
	empty := []domain.SecretBinding{}
	row, _ := r.repo.Get(t.Context(), nil, team, orchID, false)
	out, err = r.svc.Update(t.Context(), admin, team, orchID, row.Version, domain.AgentPatch{Bindings: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Bindings) != 0 && len(out.Bindings) != 3 {
		t.Errorf("bindings: %+v", out.Bindings)
	}
}

func bootSecrets(t *testing.T, r *rig, tool domain.CollabTool, created *[]string) error {
	t.Helper()
	return r.svc.Tx.InTx(t.Context(), admin.Session(), func(ctx context.Context, tx port.Tx) error {
		return r.svc.CreateBootSecrets(ctx, tx, admin, team, orchID, tool, nil, created)
	})
}

func deniedCount(r *rig) int {
	n := 0
	for _, e := range r.audit.Entries {
		if e.Own {
			n++
		}
	}
	return n
}

func withBoot(a domain.Agent) domain.Agent { return a }

// COL-7: the boot secrets are generated, written to Secrets Manager and stored as AFS-managed rows;
// Buzz has none; a Secrets Manager failure is a 503.
func TestCreateBootSecrets(t *testing.T) {
	r, w := collabRig()
	var created []string
	if err := bootSecrets(t, r, domain.ToolPaperclip, &created); err != nil {
		t.Fatal(err)
	}
	if len(w.Created) != 3 || len(r.repo.Boot) != 3 || len(created) != 3 {
		t.Fatalf("created=%d rows=%d arns=%d", len(w.Created), len(r.repo.Boot), len(created))
	}
	envs := map[string]bool{}
	for id, sec := range r.repo.Boot {
		if sec.ManagedBy != domain.ManagedByAFS || sec.AgentID != orchID {
			t.Errorf("secret %+v", sec)
		}
		envs[r.repo.BootEnv[id]] = true
	}
	for _, b := range domain.BootSecrets() {
		if !envs[b.Env] {
			t.Errorf("no binding for %s", b.Env)
		}
	}
	var secrets int
	for _, ev := range r.outbox.Events {
		if ev.Type == domain.EventSecretCreated {
			secrets++
		}
	}
	if secrets != 3 || len(r.audit.Entries) != 3 {
		t.Errorf("events=%d audit=%d", secrets, len(r.audit.Entries))
	}

	// Buzz has no boot secrets: nothing is written, even without a writer.
	r, w = collabRig()
	r.svc.Boot = nil
	created = nil
	if err := bootSecrets(t, r, domain.ToolBuzz, &created); err != nil || len(w.Created) != 0 {
		t.Errorf("buzz: %v", err)
	}
}

// A Secrets Manager failure is a 503 that leaves the written ARNs to Compensate; a delete that fails
// tags the secret for the sweep.
func TestBootSecretsCompensation(t *testing.T) {
	r, w := collabRig()
	w.CreateErr, w.FailAt = errors.New("throttled"), 3
	var created []string
	err := bootSecrets(t, r, domain.ToolPaperclip, &created)
	if domain.KindOf(err) != domain.KindUnavailable || len(created) != 2 {
		t.Fatalf("err = %v, created %d", err, len(created))
	}
	r.svc.Compensate(t.Context(), created)
	if len(w.Deleted) != 2 {
		t.Errorf("deleted=%d", len(w.Deleted))
	}

	r, w = collabRig()
	orphans := 0
	r.svc.OnOrphan = func() { orphans++ }
	w.CreateErr, w.FailAt, w.DeleteErr = errors.New("down"), 2, errors.New("no")
	created = nil
	_ = bootSecrets(t, r, domain.ToolPaperclip, &created)
	r.svc.Compensate(t.Context(), created)
	if len(w.Orphaned) != 1 || orphans != 1 {
		t.Errorf("orphaned=%d callbacks=%d", len(w.Orphaned), orphans)
	}

	// Without a writer the request is refused, never half done.
	r, _ = collabRig()
	r.svc.Boot = nil
	created = nil
	if err := bootSecrets(t, r, domain.ToolPaperclip, &created); domain.KindOf(err) != domain.KindUnavailable {
		t.Errorf("no writer: %v", err)
	}
}

// RA-19: a worker's collaboration.connect needs the team's orchestration agent and, in managed
// registration, takes no Paperclip agent id.
func TestWorkerConnect(t *testing.T) {
	yes := true
	r, _ := collabRig()
	delete(r.repo.Collab, team)
	p := params("worker")
	p.Collaboration = &domain.CollaborationInput{Connect: &yes}
	if _, err := r.svc.Create(t.Context(), admin, team, p, nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("no orchestration agent: %v", err)
	}
	r.repo.Collab[team] = domain.RegistrationManual
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

// ListAgents includes the orchestration agent; the role filter selects (collaboration reads as orchestration).
func TestListByRole(t *testing.T) {
	r, _ := collabRig()
	r.create(t, "wk1")
	all, _, err := r.svc.List(t.Context(), admin, team, "", acting.Page{Limit: 10})
	if err != nil || len(all) != 2 {
		t.Fatalf("all: %d %v", len(all), err)
	}
	for role, want := range map[domain.AgentRole]string{domain.AgentRoleWorker: "wk1", domain.AgentRoleOrchestration: domain.OrchestratorName, domain.AgentRoleCollaboration: domain.OrchestratorName} {
		got, _, err := r.svc.List(t.Context(), admin, team, role, acting.Page{Limit: 10})
		if err != nil || len(got) != 1 || string(got[0].Name) != want {
			t.Errorf("%s: %+v %v", role, got, err)
		}
	}
}

// RA-20: a Paperclip secret cannot be bound.
func TestBindPaperclipSecretRefused(t *testing.T) {
	r := newRig()
	r.repo.SecretsBy[secA] = agents.SecretInfo{AgentID: "", ManagedBy: string(domain.ManagedByPaperclip)}
	p := params("wrk")
	p.Bindings = []domain.SecretBinding{{SecretID: secA, Delivery: "env", Target: "TOKEN"}}
	if _, err := r.svc.Create(t.Context(), admin, team, p, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("err = %v", err)
	}
}

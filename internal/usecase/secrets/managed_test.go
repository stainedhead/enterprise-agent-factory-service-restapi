package secrets_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
)

// seedManaged puts a managed secret of the collaboration agent into the fake repository.
func seedManaged(r *rig, id, name string, by domain.SecretManagedBy) domain.Secret {
	s := domain.Secret{ID: id, TeamID: team, AgentID: agent, Scope: domain.SecretScopeAgent, Name: name, ManagedBy: by,
		ARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/ns/coder/" + name + "-AbCdEf", Version: 1}
	r.repo.Rows[id] = s
	return s
}

// RA-20: a secret AFS or Paperclip manages cannot be changed, deleted or written through the API.
func TestManagedSecretsRefuseUserChanges(t *testing.T) {
	r := newRig()
	afs := seedManaged(r, "sec_01JD2Q3R4S5T6V7W8X9Y0Z0A01", domain.SecretPaperclipBetterAuth, domain.ManagedByAFS)
	pc := seedManaged(r, "sec_01JD2Q3R4S5T6V7W8X9Y0Z0A02", "paperclip-notion", domain.ManagedByPaperclip)
	desc := "x"
	v, _ := domain.NewSecretValue("value", value)
	for _, s := range []domain.Secret{afs, pc} {
		if _, err := r.svc.Update(t.Context(), admin, team, s.ID, 1, domain.SecretPatch{Description: &desc}); !isManaged(err) {
			t.Errorf("Update %s: %v", s.Name, err)
		}
		if err := r.svc.Delete(t.Context(), admin, team, s.ID, 1); !isManaged(err) {
			t.Errorf("Delete %s: %v", s.Name, err)
		}
		ver := domain.Version(1)
		if err := r.svc.PutValue(t.Context(), admin, team, s.ID, &ver, v); !isManaged(err) {
			t.Errorf("PutValue %s: %v", s.Name, err)
		}
	}
	if _, err := r.svc.Rotate(t.Context(), admin, team, pc.ID, v, nil); !isManaged(err) {
		t.Errorf("Rotate paperclip secret: %v", err)
	}
	r.noLeak(t)
}

func isManaged(err error) bool {
	var e *domain.Error
	ok := errors.As(err, &e)
	return ok && e.Kind == domain.KindConflict && e.Reason == domain.ReasonManagedSecret
}

// RA-20: RotateSecret on an AFS-managed secret takes no value, AFS generates it; the master key is refused.
func TestRotateAFSManagedSecretGeneratesValue(t *testing.T) {
	r := newRig()
	s := seedManaged(r, "sec_01JD2Q3R4S5T6V7W8X9Y0Z0A03", domain.SecretPaperclipToolSigning, domain.ManagedByAFS)
	res, err := r.svc.Rotate(t.Context(), admin, team, s.ID, domain.SecretValue{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Secret.VersionID == "" || len(r.outbox.Events) != 1 || r.outbox.Events[0].Type != domain.EventSecretRotated {
		t.Errorf("rotate: %+v events=%d", res.Secret, len(r.outbox.Events))
	}
	master := seedManaged(r, "sec_01JD2Q3R4S5T6V7W8X9Y0Z0A04", domain.SecretPaperclipMasterKey, domain.ManagedByAFS)
	_, err = r.svc.Rotate(t.Context(), admin, team, master.ID, domain.SecretValue{}, nil)
	var e *domain.Error
	ok := errors.As(err, &e)
	if !ok || e.Kind != domain.KindValidation || !strings.Contains(e.Message, domain.ReasonMasterKeyNotRotatable) {
		t.Errorf("master key: %v", err)
	}
	// A user's secret still needs a value.
	u, err := r.svc.Create(t.Context(), admin, team, params("mine"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Rotate(t.Context(), admin, team, u.Secret.ID, domain.SecretValue{}, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("user secret without value: %v", err)
	}
}

// RA-20: ListSecrets filters by managedBy; an unknown value is a ValidationError.
func TestListSecretsByManagedBy(t *testing.T) {
	r := newRig()
	seedManaged(r, "sec_01JD2Q3R4S5T6V7W8X9Y0Z0A05", domain.SecretPaperclipBetterAuth, domain.ManagedByAFS)
	if _, err := r.svc.Create(t.Context(), admin, team, params("mine"), nil); err != nil {
		t.Fatal(err)
	}
	afs, _, err := r.svc.List(t.Context(), admin, team, domain.ManagedByAFS, pageOf(10))
	if err != nil || len(afs) != 1 || afs[0].ManagedBy != domain.ManagedByAFS {
		t.Fatalf("afs: %v %v", afs, err)
	}
	user, _, err := r.svc.List(t.Context(), admin, team, domain.ManagedByUser, pageOf(10))
	if err != nil || len(user) != 1 {
		t.Fatalf("user: %v %v", user, err)
	}
	if _, _, err := r.svc.List(t.Context(), admin, team, "bogus", pageOf(10)); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("bogus: %v", err)
	}
}

// RA-20: reserved names.
func TestReservedSecretNames(t *testing.T) {
	r := newRig()
	for _, n := range []string{"paperclip", "paperclip-better-auth-secret", "paperclip-anything", "paperclip-agent-api-key"} {
		if _, err := r.svc.Create(t.Context(), admin, team, params(n), nil); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s at team scope: %v", n, err)
		}
	}
	p := params("paperclip-agent-api-key")
	p.Scope, p.AgentID = domain.SecretScopeAgent, agent
	if _, err := r.svc.Create(t.Context(), admin, team, p, nil); err != nil {
		t.Errorf("worker key secret: %v", err)
	}
}

func pageOf(n int) acting.Page { return acting.Page{Limit: n} }

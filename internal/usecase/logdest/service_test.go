package logdest_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/logdest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/logdest/logdesttest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	team   = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	other  = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	secret = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001"
)

type rig struct {
	svc    *logdest.Service
	repo   *logdesttest.Repo
	audit  *fakes.Audit
	outbox *fakes.Outbox
}

func newRig() *rig {
	r := &rig{repo: logdesttest.New(), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}}
	r.svc = &logdest.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}, IDs: &fakes.IDGen{}}, Repo: r.repo}
	r.repo.Secrets[secret] = team
	return r
}

func caller(role authz.Role, tm string) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: role, TeamID: tm}}}}
}

func cw(name string) domain.NewLogDestinationParams {
	return domain.NewLogDestinationParams{Name: name, Kind: domain.LogCloudWatch, Enabled: true,
		Config: map[string]string{"logGroup": "/afs/payments", "region": "us-east-1"}}
}

func TestCreateListGetUpdateDelete(t *testing.T) {
	r := newRig()
	admin := caller(authz.RoleTeamAdmin, team)
	res, err := r.svc.Create(t.Context(), admin, team, cw("cw"), nil)
	if err != nil || res.Destination.Version != 1 || res.Destination.Status.Phase != domain.PhasePending {
		t.Fatalf("%+v %v", res, err)
	}
	id := res.Destination.ID
	if len(r.audit.Entries) != 1 || len(r.outbox.Events) != 0 {
		t.Fatalf("audit=%d outbox=%d (CD-7: audit only)", len(r.audit.Entries), len(r.outbox.Events))
	}
	if _, err := r.svc.Create(t.Context(), admin, team, cw("cw"), nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("duplicate name: %v", err)
	}
	bad := cw("x")
	bad.Config["token"] = "abc"
	if _, err := r.svc.Create(t.Context(), admin, team, bad, nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("inline credential: %v", err)
	}
	dd := domain.NewLogDestinationParams{Name: "dd", Kind: domain.LogDatadog, Config: map[string]string{"site": "datadoghq.com"}, SecretID: "sec_01JD2Q3R4S5T6V7W8X9Y0Z0009"}
	if _, err := r.svc.Create(t.Context(), admin, team, dd, nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("unknown secret: %v", err)
	}
	dd.SecretID = secret
	if _, err := r.svc.Create(t.Context(), admin, team, dd, nil); err != nil {
		t.Fatalf("datadog with team secret: %v", err)
	}
	rows, next, err := r.svc.List(t.Context(), admin, team, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" {
		t.Fatalf("list page 1: %d %q %v", len(rows), next, err)
	}
	rows, next, err = r.svc.List(t.Context(), admin, team, acting.Page{Limit: 1, Cursor: next})
	if err != nil || len(rows) != 1 || next != "" {
		t.Fatalf("list page 2: %d %q %v", len(rows), next, err)
	}
	if _, err := r.svc.Get(t.Context(), admin, other, id); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("get through another team: %v", err)
	}
	off := false
	if _, err := r.svc.Update(t.Context(), admin, team, id, domain.LogDestinationPatch{Enabled: &off}, 7); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale update: %v", err)
	}
	up, err := r.svc.Update(t.Context(), admin, team, id, domain.LogDestinationPatch{Enabled: &off}, 1)
	if err != nil || up.Enabled || up.Version != 2 || up.Generation != 2 {
		t.Fatalf("update: %+v %v", up, err)
	}
	s := "sec_01JD2Q3R4S5T6V7W8X9Y0Z0009"
	if _, err := r.svc.Update(t.Context(), admin, team, id, domain.LogDestinationPatch{SecretID: &s}, 2); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("secret on cloudwatch: %v", err)
	}
	if _, err := r.svc.Delete(t.Context(), admin, team, id, 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale delete: %v", err)
	}
	gone, err := r.svc.Delete(t.Context(), admin, team, id, 2)
	if err != nil || gone.Status.Phase != domain.PhaseDeleting || gone.ID != id {
		t.Fatalf("delete: %+v %v", gone, err)
	}
	if _, err := r.svc.Get(t.Context(), admin, team, id); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("get after delete: %v", err)
	}
	if len(r.audit.Entries) != 4 { // create cw, create dd, update, delete
		t.Fatalf("audit = %d", len(r.audit.Entries))
	}
}

func TestViewerAndForeignCallersDenied(t *testing.T) {
	r := newRig()
	admin := caller(authz.RoleTeamAdmin, team)
	res, err := r.svc.Create(t.Context(), admin, team, cw("cw"), nil)
	if err != nil {
		t.Fatal(err)
	}
	id := res.Destination.ID
	r.audit.Entries = nil
	viewer := caller(authz.RoleTeamViewer, team)
	off := false
	// The viewer reads, never writes: one denied audit record per refused write.
	if _, err := r.svc.Get(t.Context(), viewer, team, id); err != nil {
		t.Fatalf("viewer get: %v", err)
	}
	if _, _, err := r.svc.List(t.Context(), viewer, team, acting.Page{}); err != nil {
		t.Fatalf("viewer list: %v", err)
	}
	if _, err := r.svc.Create(t.Context(), viewer, team, cw("v"), nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("viewer create: %v", err)
	}
	if _, err := r.svc.Update(t.Context(), viewer, team, id, domain.LogDestinationPatch{Enabled: &off}, 1); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("viewer update: %v", err)
	}
	if _, err := r.svc.Delete(t.Context(), viewer, team, id, 1); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("viewer delete: %v", err)
	}
	if len(r.audit.Entries) != 3 {
		t.Fatalf("denied audits = %d, want 3", len(r.audit.Entries))
	}
	for _, e := range r.audit.Entries {
		if !e.Own {
			t.Fatal("a denied record is written in its own transaction")
		}
	}
	// A caller bound only to another team sees nothing of this one.
	foreign := caller(authz.RoleTeamAdmin, other)
	if _, err := r.svc.Create(t.Context(), foreign, team, cw("f"), nil); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("foreign create: %v", err)
	}
	if len(r.repo.Rows) != 1 || !r.repo.Rows[id].Enabled {
		t.Fatal("refused calls changed destinations")
	}
}

func TestCreateReplay(t *testing.T) {
	r := newRig()
	admin := caller(authz.RoleTeamAdmin, team)
	idem := func() *acting.Idem {
		return &acting.Idem{Key: port.IdempotencyKey{Key: "k", Operation: "CreateLogDestination", RequestSHA256: [32]byte{1}},
			Render: func(any) (int, json.RawMessage, error) { return 201, json.RawMessage(`{"ok":true}`), nil }}
	}
	if _, err := r.svc.Create(t.Context(), admin, team, cw("cw"), idem()); err != nil {
		t.Fatal(err)
	}
	res, err := r.svc.Create(t.Context(), admin, team, cw("cw"), idem())
	if err != nil || res.Replay == nil || len(r.repo.Rows) != 1 || len(r.audit.Entries) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

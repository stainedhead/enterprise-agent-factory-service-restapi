package members_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/members"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/members/memberstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const team = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"

type rig struct {
	svc    *members.Service
	repo   *memberstest.Repo
	audit  *fakes.Audit
	outbox *fakes.Outbox
}

func newRig() *rig {
	r := &rig{repo: memberstest.New(), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}}
	r.svc = &members.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: r.repo}
	r.repo.Users["a@example.com"] = "usr_01JD2Q3R4S5T6V7W8X9Y0Z0009"
	return r
}

func caller(role authz.Role, tm string) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: role, TeamID: tm}}}}
}

var svcAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_a", ServiceAdmin: true}}

func add(email string, role domain.TeamRole) domain.NewMemberParams {
	return domain.NewMemberParams{Role: role, SubjectType: domain.SubjectTypeUser, Email: email}
}

func TestAddListUpdateRemove(t *testing.T) {
	r := newRig()
	admin := caller(authz.RoleTeamAdmin, team)
	res, err := r.svc.Add(t.Context(), admin, team, add("a@example.com", domain.TeamRoleTeamViewer), nil)
	if err != nil || res.Member.UserID == "" || res.Member.Version != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(r.audit.Entries) != 1 || len(r.outbox.Events) != 0 {
		t.Fatalf("audit=%d outbox=%d (CD-7: audit only)", len(r.audit.Entries), len(r.outbox.Events))
	}
	if _, err := r.svc.Add(t.Context(), admin, team, add("a@example.com", domain.TeamRoleTeamViewer), nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := r.svc.Add(t.Context(), admin, team, add("ghost@example.com", domain.TeamRoleTeamViewer), nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("unknown user: %v", err)
	}
	g, err := r.svc.Add(t.Context(), admin, team, domain.NewMemberParams{Role: domain.TeamRoleTeamAdmin, SubjectType: domain.SubjectTypeGroup, GroupName: "eng"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rows, next, err := r.svc.List(t.Context(), admin, team, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" {
		t.Fatalf("%v %q %v", rows, next, err)
	}
	// update: stale version, then good.
	if _, err := r.svc.Update(t.Context(), admin, team, res.Member.ID, domain.TeamRoleTeamAdmin, 9); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale: %v", err)
	}
	if _, err := r.svc.Update(t.Context(), admin, team, res.Member.ID, "root", 1); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("bad role: %v", err)
	}
	up, err := r.svc.Update(t.Context(), admin, team, res.Member.ID, domain.TeamRoleTeamAdmin, 1)
	if err != nil || up.Role != domain.TeamRoleTeamAdmin || up.Version != 2 {
		t.Fatalf("%+v %v", up, err)
	}
	if _, err := r.svc.Update(t.Context(), admin, team, "rb_01JD2Q3R4S5T6V7W8X9Y0Z0099", domain.TeamRoleTeamAdmin, 1); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("missing: %v", err)
	}
	if err := r.svc.Remove(t.Context(), admin, team, g.Member.ID, 5); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale remove: %v", err)
	}
	if err := r.svc.Remove(t.Context(), admin, team, g.Member.ID, 1); err != nil {
		t.Fatal(err)
	}
	if len(r.audit.Entries) != 4 {
		t.Fatalf("audit = %d", len(r.audit.Entries))
	}
	// a binding of another team is invisible.
	if err := r.svc.Remove(t.Context(), svcAdmin, "team_01JD2Q3R4S5T6V7W8X9Y0Z0002", res.Member.ID, 2); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("other team: %v", err)
	}
}

func TestViewerAndForeignCallersDenied(t *testing.T) {
	r := newRig()
	if _, err := r.svc.Add(t.Context(), svcAdmin, team, add("a@example.com", domain.TeamRoleTeamViewer), nil); err != nil {
		t.Fatal(err)
	}
	var id string
	for k := range r.repo.Rows {
		id = k
	}
	viewer := caller(authz.RoleTeamViewer, team)
	other := caller(authz.RoleTeamAdmin, "team_01JD2Q3R4S5T6V7W8X9Y0Z0002")
	for name, c := range map[string]acting.Caller{"viewer": viewer, "admin of another team": other} {
		r.audit.Entries = nil
		if _, err := r.svc.Add(t.Context(), c, team, add("a@example.com", domain.TeamRoleTeamAdmin), nil); domain.KindOf(err) != domain.KindAccessDenied {
			t.Fatalf("%s add: %v", name, err)
		}
		if _, err := r.svc.Update(t.Context(), c, team, id, domain.TeamRoleTeamAdmin, 1); domain.KindOf(err) != domain.KindAccessDenied {
			t.Fatalf("%s update: %v", name, err)
		}
		if err := r.svc.Remove(t.Context(), c, team, id, 1); domain.KindOf(err) != domain.KindAccessDenied {
			t.Fatalf("%s remove: %v", name, err)
		}
		if len(r.audit.Entries) != 3 {
			t.Fatalf("%s: denied audits = %d", name, len(r.audit.Entries))
		}
	}
	if len(r.repo.Rows) != 1 || r.repo.Rows[id].Role != domain.TeamRoleTeamViewer {
		t.Fatal("denied calls changed bindings")
	}
}

func TestAddReplay(t *testing.T) {
	r := newRig()
	in := add("a@example.com", domain.TeamRoleTeamViewer)
	idem := func() *acting.Idem {
		return &acting.Idem{Key: port.IdempotencyKey{Key: "k", Operation: "AddMember", RequestSHA256: [32]byte{1}},
			Render: func(any) (int, json.RawMessage, error) { return 201, json.RawMessage(`{"ok":true}`), nil }}
	}
	if _, err := r.svc.Add(t.Context(), svcAdmin, team, in, idem()); err != nil {
		t.Fatal(err)
	}
	res, err := r.svc.Add(t.Context(), svcAdmin, team, in, idem())
	if err != nil || res.Replay == nil || len(r.repo.Rows) != 1 || len(r.audit.Entries) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

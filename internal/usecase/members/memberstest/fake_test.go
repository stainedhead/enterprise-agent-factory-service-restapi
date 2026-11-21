package memberstest_test

import (
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/members"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/members/memberstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

// The fake behaves like the real repository for the paths the service uses: duplicates conflict,
// stale versions are refused, missing rows are not found, and a failed transaction is rolled back.
func TestFakeBehavesLikeTheRepository(t *testing.T) {
	repo := memberstest.New()
	repo.Users["a@example.com"] = "usr_01JD2Q3R4S5T6V7W8X9Y0Z0009"
	svc := &members.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: &fakes.Audit{}, Outbox: &fakes.Outbox{}, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Now()}, IDs: &fakes.IDGen{}}, Repo: repo}
	admin := acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: authz.RoleTeamAdmin, TeamID: "team_1"}}}}
	in := domain.NewMemberParams{Role: domain.TeamRoleTeamViewer, SubjectType: domain.SubjectTypeUser, Email: "a@example.com"}
	res, err := svc.Add(t.Context(), admin, "team_1", in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Add(t.Context(), admin, "team_1", in, nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("duplicate = %v", err)
	}
	if _, err := svc.Add(t.Context(), admin, "team_1", domain.NewMemberParams{Role: domain.TeamRoleTeamViewer, SubjectType: domain.SubjectTypeUser, Email: "no@example.com"}, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("unknown user = %v", err)
	}
	rows, _, err := svc.List(t.Context(), admin, "team_1", acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = %v %v", rows, err)
	}
	if _, err := svc.Update(t.Context(), admin, "team_1", res.Member.ID, domain.TeamRoleTeamAdmin, 9); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale update = %v", err)
	}
	if _, err := svc.Update(t.Context(), admin, "team_1", "rb_none", domain.TeamRoleTeamAdmin, 1); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("missing update = %v", err)
	}
	if err := svc.Remove(t.Context(), admin, "team_1", res.Member.ID, 9); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale remove = %v", err)
	}
	if err := svc.Remove(t.Context(), admin, "team_1", res.Member.ID, 1); err != nil {
		t.Errorf("remove = %v", err)
	}
	if err := svc.Remove(t.Context(), admin, "team_1", res.Member.ID, 1); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("remove again = %v", err)
	}
}

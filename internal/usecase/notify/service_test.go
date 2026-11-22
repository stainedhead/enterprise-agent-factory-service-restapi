package notify_test

import (
	"context"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/notify"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/notify/notifytest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	team   = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	other  = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	secret = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0001" //nolint:gosec // fixture id
)

type rig struct {
	svc    *notify.Service
	repo   *notifytest.Repo
	audit  *fakes.Audit
	outbox *fakes.Outbox
}

func newRig() *rig {
	r := &rig{repo: notifytest.New(), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}}
	r.svc = &notify.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}, IDs: &fakes.IDGen{}}, Repo: r.repo,
		TestWait: time.Second, Poll: time.Second, Sleep: func(context.Context, time.Duration) {}}
	r.repo.Secrets[secret] = team
	return r
}

func caller(role authz.Role, tm string) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: role, TeamID: tm}}}}
}

func admin() acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_sa", Type: authz.PrincipalUser, ServiceAdmin: true}}
}

func slack(name string) domain.NewChannelParams {
	return domain.NewChannelParams{Name: name, Kind: domain.ChannelSlack, Enabled: true, SecretID: secret,
		Config: map[string]any{"channel": "#payments-alerts"}, EventTypes: []string{"budget.*"}}
}

func email(name string) domain.NewChannelParams {
	return domain.NewChannelParams{Name: name, Kind: domain.ChannelEmail, Enabled: true, Config: map[string]any{"to": []any{"ops@example.com"}}}
}

func TestTeamChannelLifecycle(t *testing.T) {
	r := newRig()
	ta := caller(authz.RoleTeamAdmin, team)
	res, err := r.svc.CreateTeam(t.Context(), ta, team, slack("s"), nil)
	if err != nil || res.Channel.Version != 1 || res.Channel.Status.Phase != domain.PhasePending {
		t.Fatalf("%+v %v", res, err)
	}
	id := res.Channel.ID
	if len(r.audit.Entries) != 1 || len(r.outbox.Events) != 0 {
		t.Fatalf("audit=%d outbox=%d", len(r.audit.Entries), len(r.outbox.Events))
	}
	if _, err := r.svc.CreateTeam(t.Context(), ta, team, slack("s"), nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("duplicate: %v", err)
	}
	bad := slack("x")
	bad.SecretID = "sec_01JD2Q3R4S5T6V7W8X9Y0Z0009" //nolint:gosec // fixture id
	if _, err := r.svc.CreateTeam(t.Context(), ta, team, bad, nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("foreign secret: %v", err)
	}
	if _, err := r.svc.CreateTeam(t.Context(), ta, team, email("e"), nil); err != nil {
		t.Fatal(err)
	}
	rows, next, err := r.svc.ListTeam(t.Context(), ta, team, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" {
		t.Fatalf("page 1: %d %q %v", len(rows), next, err)
	}
	if rows, next, err = r.svc.ListTeam(t.Context(), ta, team, acting.Page{Limit: 1, Cursor: next}); err != nil || len(rows) != 1 || next != "" {
		t.Fatalf("page 2: %d %q %v", len(rows), next, err)
	}
	if _, err := r.svc.GetTeam(t.Context(), ta, other, id); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("get through another team: %v", err)
	}
	off := false
	if _, err := r.svc.UpdateTeam(t.Context(), ta, team, id, domain.ChannelPatch{Enabled: &off}, 9); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale update: %v", err)
	}
	up, err := r.svc.UpdateTeam(t.Context(), ta, team, id, domain.ChannelPatch{Enabled: &off}, 1)
	if err != nil || up.Enabled || up.Version != 2 {
		t.Fatalf("update: %+v %v", up, err)
	}
	if _, err := r.svc.DeleteTeam(t.Context(), ta, team, id, 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale delete: %v", err)
	}
	gone, err := r.svc.DeleteTeam(t.Context(), ta, team, id, 2)
	if err != nil || gone.Status.Phase != domain.PhaseDeleting {
		t.Fatalf("delete: %+v %v", gone, err)
	}
	if _, err := r.svc.GetTeam(t.Context(), ta, team, id); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestViewerReadsButCannotWrite(t *testing.T) {
	r := newRig()
	res, err := r.svc.CreateTeam(t.Context(), caller(authz.RoleTeamAdmin, team), team, slack("s"), nil)
	if err != nil {
		t.Fatal(err)
	}
	v := caller(authz.RoleTeamViewer, team)
	if _, err := r.svc.CreateTeam(t.Context(), v, team, slack("v"), nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("viewer create: %v", err)
	}
	denied := 0
	for _, e := range r.audit.Entries {
		if e.Record.Outcome == "denied" && e.Record.Action == "CreateNotificationChannel" {
			denied++
		}
	}
	if denied != 1 {
		t.Fatalf("denied audit records = %d", denied)
	}
	if _, _, err := r.svc.ListTeam(t.Context(), v, team, acting.Page{}); err != nil {
		t.Fatalf("viewer list: %v", err)
	}
	if _, err := r.svc.GetTeam(t.Context(), v, team, res.Channel.ID); err != nil {
		t.Fatalf("viewer get: %v", err)
	}
	if _, err := r.svc.UpdateTeam(t.Context(), v, team, res.Channel.ID, domain.ChannelPatch{}, 1); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("viewer update: %v", err)
	}
	if _, err := r.svc.DeleteTeam(t.Context(), v, team, res.Channel.ID, 1); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("viewer delete: %v", err)
	}
	if _, err := r.svc.TestChannel(t.Context(), v, team, res.Channel.ID, nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("viewer test: %v", err)
	}
	// A caller of another team learns nothing: 404, not 403.
	if _, err := r.svc.CreateTeam(t.Context(), caller(authz.RoleTeamAdmin, other), team, slack("z"), nil); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("foreign team admin: %v", err)
	}
	if _, _, err := r.svc.ListTeam(t.Context(), caller(authz.RoleTeamAdmin, other), team, acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("foreign list: %v", err)
	}
}

func TestServiceChannels(t *testing.T) {
	r := newRig()
	sa := admin()
	res, err := r.svc.CreateService(t.Context(), sa, email("svc"), nil)
	if err != nil || res.Channel.TeamID != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := r.svc.CreateService(t.Context(), sa, slack("bad"), nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("service slack: %v", err)
	}
	rows, _, err := r.svc.ListService(t.Context(), sa, acting.Page{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("%v %v", rows, err)
	}
	// Team admins and team callers have no admin:settings.
	ta := caller(authz.RoleTeamAdmin, team)
	if _, err := r.svc.CreateService(t.Context(), ta, email("x"), nil); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("team admin create service: %v", err)
	}
	if _, _, err := r.svc.ListService(t.Context(), ta, acting.Page{}); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("team admin list service: %v", err)
	}
	if _, err := r.svc.UpdateService(t.Context(), ta, res.Channel.ID, domain.ChannelPatch{}, 1); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("team admin update service: %v", err)
	}
	// A service channel is invisible on the team routes.
	if _, err := r.svc.GetTeam(t.Context(), ta, team, res.Channel.ID); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("service channel via team route: %v", err)
	}
	off := false
	if up, err := r.svc.UpdateService(t.Context(), sa, res.Channel.ID, domain.ChannelPatch{Enabled: &off}, 1); err != nil || up.Enabled {
		t.Fatalf("%+v %v", up, err)
	}
	// The channel the work order setting names cannot be deleted.
	r.repo.WorkOrderChannel = res.Channel.ID
	if _, err := r.svc.DeleteService(t.Context(), sa, res.Channel.ID, 2); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("delete named channel: %v", err)
	}
	r.repo.WorkOrderChannel = ""
	if _, err := r.svc.DeleteService(t.Context(), ta, res.Channel.ID, 2); domain.KindOf(err) != domain.KindAccessDenied {
		t.Fatalf("team admin delete: %v", err)
	}
	if _, err := r.svc.DeleteService(t.Context(), sa, res.Channel.ID, 2); err != nil {
		t.Fatal(err)
	}
}

func TestTestChannel(t *testing.T) {
	r := newRig()
	ta := caller(authz.RoleTeamAdmin, team)
	res, _ := r.svc.CreateTeam(t.Context(), ta, team, slack("s"), nil)
	id := res.Channel.ID

	// No report within the wait: pending, and one outbox event with the test id.
	out, err := r.svc.TestChannel(t.Context(), ta, team, id, nil)
	if err != nil || !out.Result.Pending || out.Result.TestID == "" {
		t.Fatalf("%+v %v", out, err)
	}
	if len(r.outbox.Events) != 1 || r.outbox.Events[0].Type != domain.EventNotificationTestRequested ||
		r.outbox.Events[0].Detail.Data["testId"] != out.Result.TestID || r.outbox.Events[0].Detail.Data["channelId"] != id {
		t.Fatalf("events: %+v", r.outbox.Events)
	}

	// The reconciler reports a failure while the API waits: returned and not pending.
	r.svc.Sleep = func(context.Context, time.Duration) {
		n := len(r.outbox.Events)
		tid := r.outbox.Events[n-1].Detail.Data["testId"].(string)
		f := false
		r.repo.SetStatus(id, []domain.Condition{domain.DeliveryDetail{LastTestID: tid, TestDelivered: &f, TestError: "slack: 404 no_service"}.Condition(time.Now())})
	}
	r.svc.Poll = 10 * time.Millisecond
	out, err = r.svc.TestChannel(t.Context(), ta, team, id, nil)
	if err != nil || out.Result.Pending || out.Result.Delivered || out.Result.Error != "slack: 404 no_service" {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := r.svc.TestChannel(t.Context(), ta, team, "nch_01JD2Q3R4S5T6V7W8X9Y0Z0099", nil); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("unknown channel: %v", err)
	}
}

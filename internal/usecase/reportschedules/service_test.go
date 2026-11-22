package reportschedules_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reportschedules"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reportschedules/schedtest"
)

const (
	team     = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	other    = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	chanOwn  = "nch_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	chanOwn2 = "nch_01JD2Q3R4S5T6V7W8X9Y0Z0004"
	chanSvc  = "nch_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	chanOth  = "nch_01JD2Q3R4S5T6V7W8X9Y0Z0003"
)

type rig struct {
	svc    *reportschedules.Service
	repo   *schedtest.Repo
	audit  *fakes.Audit
	outbox *fakes.Outbox
}

func newRig() *rig {
	r := &rig{repo: schedtest.New(), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}}
	r.svc = &reportschedules.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}, IDs: &fakes.IDGen{}}, Repo: r.repo}
	r.repo.Teams[team], r.repo.Teams[other] = true, true
	r.repo.Channels[chanOwn], r.repo.Channels[chanOwn2], r.repo.Channels[chanSvc], r.repo.Channels[chanOth] = team, team, "", other
	return r
}

func caller(role authz.Role, tm string) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: role, TeamID: tm}}}}
}

func admin() acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_sa", Type: authz.PrincipalUser, ServiceAdmin: true}}
}

func weekly(name, ch string) domain.NewScheduleParams {
	return domain.NewScheduleParams{Name: name, Report: domain.ReportKindUsage, Cadence: domain.CadenceWeekly, ChannelID: ch,
		Query: domain.ScheduleQuery{Range: "7d", GroupBy: []string{"agent"}}}
}

func TestScheduleLifecycle(t *testing.T) {
	r := newRig()
	ta := caller(authz.RoleTeamAdmin, team)
	res, err := r.svc.Create(t.Context(), ta, team, weekly("w", chanOwn), nil)
	if err != nil || res.Schedule.Version != 1 || !res.Schedule.NextRunAt.After(time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v %v", res, err)
	}
	id := res.Schedule.ID
	if len(r.audit.Entries) != 1 || len(r.outbox.Events) != 0 {
		t.Fatalf("a schedule change writes an audit record and no event: audit=%d outbox=%d", len(r.audit.Entries), len(r.outbox.Events))
	}
	if _, err := r.svc.Create(t.Context(), ta, team, weekly("w", chanOwn), nil); domain.KindOf(err) != domain.KindConflict {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := r.svc.Create(t.Context(), ta, team, weekly("w2", chanOwn2), nil); err != nil {
		t.Fatal(err)
	}
	rows, next, err := r.svc.List(t.Context(), ta, team, acting.Page{Limit: 1})
	if err != nil || len(rows) != 1 || next == "" {
		t.Fatalf("page 1: %d %q %v", len(rows), next, err)
	}
	if rows, next, err = r.svc.List(t.Context(), ta, team, acting.Page{Limit: 1, Cursor: next}); err != nil || len(rows) != 1 || next != "" {
		t.Fatalf("page 2: %d %q %v", len(rows), next, err)
	}
	off := false
	if _, err := r.svc.Update(t.Context(), ta, team, id, domain.SchedulePatch{Enabled: &off}, 9); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale update: %v", err)
	}
	up, err := r.svc.Update(t.Context(), ta, team, id, domain.SchedulePatch{Enabled: &off}, 1)
	if err != nil || up.Enabled || up.Version != 2 {
		t.Fatalf("update: %+v %v", up, err)
	}
	if err := r.svc.Delete(t.Context(), ta, team, id, 1); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Fatalf("stale delete: %v", err)
	}
	if err := r.svc.Delete(t.Context(), ta, team, id, 2); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Delete(t.Context(), ta, team, id, 2); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("second delete: %v", err)
	}
	if len(r.audit.Entries) != 4 {
		t.Fatalf("audit entries = %d, want create, create, update, delete", len(r.audit.Entries))
	}
}

// RA-US-7a.1, RA-US-15.6: only the team's own channel.
func TestScheduleChannelMustBeTheTeamsOwn(t *testing.T) {
	cases := []struct {
		name    string
		channel string
		want    domain.ErrorKind
	}{
		{"own channel", chanOwn, ""},
		{"service-level channel", chanSvc, domain.KindValidation},
		{"another team's channel", chanOth, domain.KindValidation},
		{"unknown channel", "nch_01JD2Q3R4S5T6V7W8X9Y0Z0999", domain.KindValidation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig()
			_, err := r.svc.Create(t.Context(), caller(authz.RoleTeamAdmin, team), team, weekly("w", c.channel), nil)
			if domain.KindOf(err) != c.want {
				t.Fatalf("create: %v, want %q", err, c.want)
			}
			if c.want != "" && len(r.repo.Rows) != 0 {
				t.Error("a refused create wrote a row")
			}
			// The same rule on update.
			ok, err := r.svc.Create(t.Context(), caller(authz.RoleTeamAdmin, team), team, weekly("ok", chanOwn), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.svc.Update(t.Context(), caller(authz.RoleTeamAdmin, team), team, ok.Schedule.ID, domain.SchedulePatch{ChannelID: &c.channel}, 1)
			if domain.KindOf(err) != c.want {
				t.Fatalf("update: %v, want %q", err, c.want)
			}
		})
	}
}

// RA-US-7a.2: a viewer reads but cannot change; the refusal is audited; every caller is covered.
func TestSchedulePermissions(t *testing.T) {
	r := newRig()
	made, err := r.svc.Create(t.Context(), admin(), team, weekly("w", chanOwn), nil)
	if err != nil {
		t.Fatal(err)
	}
	id, off := made.Schedule.ID, false
	cases := []struct {
		name        string
		c           acting.Caller
		wantRead    domain.ErrorKind
		wantWrite   domain.ErrorKind
		deniedAudit bool
	}{
		{"team admin", caller(authz.RoleTeamAdmin, team), "", "", false},
		{"service admin", admin(), "", "", false},
		{"viewer", caller(authz.RoleTeamViewer, team), "", domain.KindAccessDenied, true},
		{"admin of another team", caller(authz.RoleTeamAdmin, other), domain.KindNotFound, domain.KindNotFound, false},
		{"viewer of another team", caller(authz.RoleTeamViewer, other), domain.KindNotFound, domain.KindNotFound, false},
		{"unbound user", acting.Caller{Principal: authz.Principal{ID: "usr_x", Type: authz.PrincipalUser}}, domain.KindNotFound, domain.KindNotFound, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(r.audit.Entries)
			if _, _, err := r.svc.List(t.Context(), c.c, team, acting.Page{}); domain.KindOf(err) != c.wantRead {
				t.Errorf("list: %v, want %q", err, c.wantRead)
			}
			if _, err := r.svc.Create(t.Context(), c.c, team, weekly("n-"+c.name, chanOwn2), nil); domain.KindOf(err) != c.wantWrite {
				t.Errorf("create: %v, want %q", err, c.wantWrite)
			}
			if _, err := r.svc.Update(t.Context(), c.c, team, id, domain.SchedulePatch{Enabled: &off}, domain.Version(1)); c.wantWrite != "" && domain.KindOf(err) != c.wantWrite {
				t.Errorf("update: %v, want %q", err, c.wantWrite)
			}
			if err := r.svc.Delete(t.Context(), c.c, team, id, 99); c.wantWrite != "" && domain.KindOf(err) != c.wantWrite {
				t.Errorf("delete: %v, want %q", err, c.wantWrite)
			}
			denied := 0
			for _, e := range r.audit.Entries[before:] {
				if e.Record.Outcome == port.OutcomeDenied {
					denied++
				}
			}
			if c.deniedAudit && denied != 3 {
				t.Errorf("denied audit rows = %d, want 3", denied)
			}
			if !c.deniedAudit && c.wantWrite != "" && denied != 0 {
				t.Errorf("a hidden team's refusal must not be audited as a denial against it: %d", denied)
			}
		})
	}
}

func TestScheduleOtherTeamsRowIsNotFound(t *testing.T) {
	r := newRig()
	made, err := r.svc.Create(t.Context(), admin(), team, weekly("w", chanOwn), nil)
	if err != nil {
		t.Fatal(err)
	}
	off := false
	// An admin of `other` addressing the schedule through their own team path.
	ob := caller(authz.RoleTeamAdmin, other)
	if _, err := r.svc.Update(t.Context(), ob, other, made.Schedule.ID, domain.SchedulePatch{Enabled: &off}, 1); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("update: %v", err)
	}
	if err := r.svc.Delete(t.Context(), ob, other, made.Schedule.ID, 1); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("delete: %v", err)
	}
}

func TestScheduleUnknownTeam(t *testing.T) {
	r := newRig()
	const ghost = "team_01JD2Q3R4S5T6V7W8X9Y0Z0999"
	if _, _, err := r.svc.List(t.Context(), admin(), ghost, acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("list: %v", err)
	}
	if _, err := r.svc.Create(t.Context(), admin(), ghost, weekly("w", chanOwn), nil); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("create: %v", err)
	}
}

func TestScheduleInvalidQueryIsRejected(t *testing.T) {
	r := newRig()
	p := weekly("w", chanOwn)
	p.Query = domain.ScheduleQuery{GroupBy: []string{"bogus"}}
	if _, err := r.svc.Create(t.Context(), admin(), team, p, nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("%v", err)
	}
	p.Query = domain.ScheduleQuery{Filters: map[string]string{"teamId": other}}
	if _, err := r.svc.Create(t.Context(), admin(), team, p, nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("a schedule on another team's data: %v", err)
	}
}

func TestScheduleIdempotentCreate(t *testing.T) {
	r := newRig()
	idem := func() *acting.Idem {
		return &acting.Idem{Key: port.IdempotencyKey{Key: "k1", Operation: "CreateReportSchedule", TeamID: team, RequestSHA256: [32]byte{1}},
			Render: func(v any) (int, json.RawMessage, error) {
				b, err := json.Marshal(map[string]any{"id": v.(domain.ReportSchedule).ID, "version": 1})
				return 201, b, err
			}}
	}
	first, err := r.svc.Create(t.Context(), admin(), team, weekly("w", chanOwn), idem())
	if err != nil || first.Replay != nil {
		t.Fatalf("%+v %v", first, err)
	}
	again, err := r.svc.Create(t.Context(), admin(), team, weekly("w", chanOwn), idem())
	if err != nil || again.Replay == nil || again.Replay.Status != 201 {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if len(r.repo.Rows) != 1 || len(r.audit.Entries) != 1 {
		t.Fatalf("rows=%d audit=%d: a replay must not write again", len(r.repo.Rows), len(r.audit.Entries))
	}
}

func TestScheduleStoreFailuresAreReturnedAndWriteNothing(t *testing.T) {
	boom := errors.New("store down")
	off := false
	ops := map[string]func(r *rig, id string) error{
		"create": func(r *rig, _ string) error {
			_, err := r.svc.Create(t.Context(), admin(), team, weekly("w2", chanOwn2), nil)
			return err
		},
		"update": func(r *rig, id string) error {
			_, err := r.svc.Update(t.Context(), admin(), team, id, domain.SchedulePatch{ChannelID: ptr(chanOwn2), Enabled: &off}, 1)
			return err
		},
		"delete": func(r *rig, id string) error { return r.svc.Delete(t.Context(), admin(), team, id, 1) },
		"list": func(r *rig, _ string) error {
			_, _, err := r.svc.List(t.Context(), admin(), team, acting.Page{})
			return err
		},
	}
	cases := map[string][]string{
		"create": {"TeamExists", "ChannelOwnedByTeam", "Insert", "Get"},
		"update": {"Get", "ChannelOwnedByTeam", "Update"},
		"delete": {"Get", "Delete"},
		"list":   {"TeamExists", "List"},
	}
	for op, methods := range cases {
		for _, method := range methods {
			t.Run(op+"/"+method, func(t *testing.T) {
				r := newRig()
				made, err := r.svc.Create(t.Context(), admin(), team, weekly("w", chanOwn), nil)
				if err != nil {
					t.Fatal(err)
				}
				r.repo.Fail[method] = boom
				if err := ops[op](r, made.Schedule.ID); !errors.Is(err, boom) {
					t.Fatalf("err = %v, want the store error", err)
				}
				if len(r.audit.Entries) != 1 {
					t.Errorf("audit entries = %d: a failed call must leave none", len(r.audit.Entries))
				}
			})
		}
	}
}

func ptr[T any](v T) *T { return &v }

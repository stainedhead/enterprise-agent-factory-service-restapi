package restore_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/restore"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/restore/restoretest"
)

const team = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"

type rig struct {
	svc    *restore.Service
	repo   *restoretest.Repo
	audit  *fakes.Audit
	outbox *fakes.Outbox
}

func newRig() *rig {
	r := &rig{repo: restoretest.New(team), audit: &fakes.Audit{}, outbox: &fakes.Outbox{}}
	r.svc = &restore.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: r.audit, Outbox: r.outbox, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)}, IDs: &fakes.IDGen{}}, Repo: r.repo}
	r.repo.Targets = domain.RestoreState{
		"agents":    {"agt_1": {"id": "agt_1", "state": "enabled", "persona": "old"}},
		"team_tags": {team + "/env": {"id": team + "/env", "value": "prod"}},
	}
	r.repo.Now = domain.RestoreState{
		"agents":    {"agt_1": {"id": "agt_1", "state": "enabled", "persona": "new"}, "agt_2": {"id": "agt_2", "state": "enabled"}},
		"team_tags": {team + "/env": {"id": team + "/env", "value": "dev"}},
	}
	return r
}

func caller(role authz.Role) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: role, TeamID: team}}}}
}

func TestApplyRestoresInOneTransaction(t *testing.T) {
	r := newRig()
	res, err := r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, false, nil, nil)
	if err != nil || res.DryRun || len(res.Changes) != 2 || len(res.Skipped) != 1 || res.Skipped[0].ResourceID != "agt_2" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(r.repo.Applied) != 2 || r.repo.Gen != 6 {
		t.Fatalf("applied %d generation %d (want previous + 1)", len(r.repo.Applied), r.repo.Gen)
	}
	if len(r.audit.Entries) != 1 || r.audit.Entries[0].Record.Action != "RestoreRevision" || r.audit.Entries[0].Record.After.(map[string]any)["revision"] != int64(41) {
		t.Fatalf("one audit record naming the revision: %+v", r.audit.Entries)
	}
	if len(r.outbox.Events) != 1 || r.outbox.Events[0].Type != domain.EventTeamUpdated {
		t.Fatalf("%+v", r.outbox.Events)
	}
	if len(r.repo.Locked) != 1 {
		t.Fatal("the team row was not locked")
	}
}

func TestDryRunReturnsTheDiffAndKeepsNothing(t *testing.T) {
	r := newRig()
	res, err := r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, true, nil, nil)
	if err != nil || !res.DryRun || len(res.Changes) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(r.audit.Entries) != 0 || len(r.outbox.Events) != 0 {
		t.Fatalf("a dry run committed audit %d events %d", len(r.audit.Entries), len(r.outbox.Events))
	}
}

func TestRefusedStepIsSkippedAndReported(t *testing.T) {
	r := newRig()
	r.repo.Refuse["team_tags/"+team+"/env"] = "the tag limit is reached"
	res, err := r.svc.Run(t.Context(), acting.Caller{Principal: authz.Principal{ID: "usr_r", ServiceAdmin: true}}, team, 41, false, nil, nil)
	if err != nil || len(res.Changes) != 1 || len(res.Skipped) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if got := res.Skipped[1]; got.Action != domain.RestoreSkip || got.Reason != "the tag limit is reached" {
		t.Fatalf("%+v", got)
	}
}

func TestRefusals(t *testing.T) {
	v2, v3 := domain.Version(2), domain.Version(3)
	cases := map[string]struct {
		c      acting.Caller
		team   string
		rev    int64
		expect *domain.Version
		mod    func(*restoretest.Repo)
		kind   domain.ErrorKind
	}{
		"viewer":         {caller(authz.RoleTeamViewer), team, 41, nil, nil, domain.KindAccessDenied},
		"stale If-Match": {caller(authz.RoleTeamAdmin), team, 41, &v2, nil, domain.KindPreconditionFailed},
		"unknown team":   {acting.Caller{Principal: authz.Principal{ID: "usr_r", ServiceAdmin: true}}, "team_01JD2Q3R4S5T6V7W8X9Y0Z0009", 41, nil, nil, domain.KindNotFound},
		"unknown rev":    {caller(authz.RoleTeamAdmin), team, 99, nil, nil, domain.KindNotFound},
		"deleting team": {caller(authz.RoleTeamAdmin), team, 41, nil, func(r *restoretest.Repo) {
			r.Lock[team] = restore.TeamLock{State: domain.TeamStateDeleting, Version: 3}
		}, domain.KindConflict},
	}
	for name, tc := range cases {
		r := newRig()
		if tc.mod != nil {
			tc.mod(r.repo)
		}
		_, err := r.svc.Run(t.Context(), tc.c, tc.team, tc.rev, false, tc.expect, nil)
		if domain.KindOf(err) != tc.kind {
			t.Errorf("%s: %v", name, err)
		}
		if len(r.repo.Applied) != 0 || r.repo.Gen != 5 {
			t.Errorf("%s: a refused restore changed something", name)
		}
	}
	r := newRig()
	if _, err := r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, false, &v3, nil); err != nil {
		t.Fatalf("matching If-Match: %v", err)
	}
}

func TestIdempotentReplay(t *testing.T) {
	r := newRig()
	idem := func() *acting.Idem {
		return &acting.Idem{Key: port.IdempotencyKey{Key: "k", Operation: "RestoreRevision", TeamID: team, RequestSHA256: [32]byte{1}},
			Render: func(any) (int, json.RawMessage, error) { return 200, json.RawMessage(`{}`), nil }}
	}
	if _, err := r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, false, nil, idem()); err != nil {
		t.Fatal(err)
	}
	res, err := r.svc.Run(t.Context(), caller(authz.RoleTeamAdmin), team, 41, false, nil, idem())
	if err != nil || res.Replay == nil || r.repo.Gen != 6 {
		t.Fatalf("%+v %v gen %d", res, err, r.repo.Gen)
	}
}

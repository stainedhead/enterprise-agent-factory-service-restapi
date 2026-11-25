package mcpcatalog_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/mcpcatalog"
)

type op struct {
	name string
	call func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, srv, tsrv string) error
}

// ops lists every use case once, with arguments valid enough to reach the permission check.
func ops() []op {
	e := func(_ any, err error) error { return err }
	e3 := func(_ any, _ any, err error) error { return err }
	v := domain.Version(1)
	fed := domain.FederatedIdentity{Name: "tools", Audience: "api://tools"}
	out := []op{
		{"List", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e3(s.List(ctx, c, mcpcatalog.ServerFilter{}, acting.Page{}))
		}},
		{"Get", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e(s.Get(ctx, c, id))
		}},
		{"Create", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e(s.Create(ctx, c, remote("newone"), nil))
		}},
		{"Import", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e(s.Import(ctx, c, json.RawMessage(`{}`), "", nil))
		}},
		{"Update", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e(s.Update(ctx, c, id, mcpcatalog.ServerInput{Purpose: str("p")}, v))
		}},
		{"Delete", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return s.Delete(ctx, c, id, v)
		}},
		{"Publish", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e3(s.Publish(ctx, c, id, nil))
		}},
		{"Retire", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e3(s.Retire(ctx, c, id, nil))
		}},
		{"Suspend", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e3(s.Suspend(ctx, c, id, "r", nil))
		}},
		{"Resume", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e3(s.Resume(ctx, c, id, "r", nil))
		}},
		{"Refresh", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return s.Refresh(ctx, c, id)
		}},
		{"ListEvents", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e3(s.ListEvents(ctx, c, domain.McpEventFilter{}, acting.Page{}))
		}},
		{"Usage", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e3(s.Usage(ctx, c, domain.McpUsageQuery{}))
		}},
		{"StartCapture", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e(s.StartCapture(ctx, c, 1, "r", nil))
		}},
		{"StopCapture", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e(s.StopCapture(ctx, c))
		}},
		{"GetCapture", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e(s.GetCapture(ctx, c))
		}},
		{"ListPrimitives", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e3(s.ListPrimitives(ctx, c, id, mcpcatalog.PrimitiveFilter{}, acting.Page{}))
		}},
		{"GetPrimitiveChange", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e(s.GetPrimitiveChange(ctx, c, id, "mpr_1"))
		}},
		{"DecidePrimitive", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e3(s.DecidePrimitive(ctx, c, id, "mpr_1", "ok", true, nil))
		}},
		{"Review", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e3(s.Review(ctx, c, id, domain.McpApproved, mcpcatalog.ReviewInput{Note: "ok"}, nil))
		}},
		{"ListReviews", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e3(s.ListReviews(ctx, c, mcpcatalog.ReviewQueueFilter{}, acting.Page{}))
		}},
		{"PutPolicy", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e(s.PutPolicy(ctx, c, id, mcpcatalog.PolicyInput{}, nil))
		}},
		{"PutCredential", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e(s.PutCredential(ctx, c, id, team, mcpcatalog.CredentialInput{Value: "v"}, nil))
		}},
		// Team operations, all against team (ops marks this entry and every later one as team-scoped).
		{"ListTeamEvents", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e3(s.ListTeamEvents(ctx, c, team, domain.McpEventFilter{}, acting.Page{}))
		}},
		{"TeamUsage", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e3(s.TeamUsage(ctx, c, team, domain.McpUsageQuery{}))
		}},
		{"ListFederated", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e(s.ListFederated(ctx, c, team, "agent", agentID))
		}},
		{"PutFederated", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e(s.PutFederated(ctx, c, team, "agent", agentID, fed, nil))
		}},
		{"DeleteFederated", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return s.DeleteFederated(ctx, c, team, "agent", agentID, "tools", v)
		}},
		{"ListTeam", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e3(s.ListTeam(ctx, c, team, "", acting.Page{}))
		}},
		{"GetTeam", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e(s.GetTeam(ctx, c, team, id))
		}},
		{"PutSwitch", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e(s.PutSwitch(ctx, c, team, id, mcpcatalog.SwitchInput{Enabled: true}, nil))
		}},
		{"CreateTeam", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e(s.CreateTeam(ctx, c, team, remote("teamnew"), nil))
		}},
		{"UpdateTeam", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, id string) error {
			return e(s.UpdateTeam(ctx, c, team, id, mcpcatalog.ServerInput{Purpose: str("p")}, v))
		}},
		{"DeleteTeam", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, id string) error {
			return s.DeleteTeam(ctx, c, team, id, v)
		}},
		{"Submit", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, id string) error {
			return e3(s.Submit(ctx, c, team, id, nil))
		}},
		{"TeamReview", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, id string) error {
			return e3(s.TeamReview(ctx, c, team, id, true, mcpcatalog.ReviewInput{Note: "ok"}, nil))
		}},
		{"RefreshTeam", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, id string) error {
			return s.RefreshTeam(ctx, c, team, id)
		}},
		{"ListTeamPrimitives", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e3(s.ListTeamPrimitives(ctx, c, team, id, mcpcatalog.PrimitiveFilter{}, acting.Page{}))
		}},
		{"TeamDecidePrimitive", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e3(s.TeamDecidePrimitive(ctx, c, team, id, "mpr_1", "ok", false, nil))
		}},
		{"PutTeamPolicy", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e(s.PutTeamPolicy(ctx, c, team, id, nil, nil))
		}},
		{"PutTeamCredential", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e(s.PutTeamCredential(ctx, c, team, id, mcpcatalog.CredentialInput{Value: "v"}, nil))
		}},
		{"DisconnectOAuth", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e3(s.DisconnectOAuth(ctx, c, team, id, nil))
		}},
		{"TestFederation", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, id, _ string) error {
			return e(s.TestFederation(ctx, c, team, id))
		}},
		{"GetEffective", func(ctx context.Context, s *mcpcatalog.Service, c acting.Caller, _, _ string) error {
			return e(s.GetEffective(ctx, c, team, "agent", agentID))
		}},
	}
	return out
}

// teamScoped names the entries of ops from ListTeamEvents on: the team-scoped use cases.
func teamScoped() map[string]bool {
	out, on := map[string]bool{}, false
	for _, o := range ops() {
		on = on || o.name == "ListTeamEvents"
		out[o.name] = on
	}
	return out
}

// seed makes an approved enterprise server with one primitive and a team-owned draft server.
func seed(t *testing.T, w *world) (string, string) {
	t.Helper()
	srv := w.approved(t, "issues")
	w.prim("mpr_1", srv.ID, domain.McpPrimitivePending, `{"name":"search"}`)
	tres, err := w.s.CreateTeam(t.Context(), teamAdmin, team, remote("mine"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return srv.ID, tres.View.Server.ID
}

// TestOutsiderIsDeniedEverywhere proves the denied case for every use case: a team admin of another
// team holds no service permission (403) and cannot see team at all (404, so its existence is hidden).
func TestOutsiderIsDeniedEverywhere(t *testing.T) {
	scoped := teamScoped()
	for _, o := range ops() {
		t.Run(o.name, func(t *testing.T) {
			w := rig()
			srv, tsrv := seed(t, w)
			before := len(w.outbox.Events)
			err := o.call(t.Context(), w.s, other, srv, tsrv)
			want := domain.KindAccessDenied
			if scoped[o.name] {
				want = domain.KindNotFound
			}
			if domain.KindOf(err) != want {
				t.Fatalf("%s by outsider = %v", o.name, err)
			}
			if len(w.outbox.Events) != before {
				t.Errorf("%s emitted an event on denial", o.name)
			}
		})
	}
}

// TestViewerCannotWrite proves a team viewer can read the team's MCP view but change nothing.
func TestViewerCannotWrite(t *testing.T) {
	reads := map[string]bool{"ListTeamEvents": true, "TeamUsage": true, "ListFederated": true, "ListTeam": true, "GetTeam": true,
		"ListTeamPrimitives": true, "GetEffective": true}
	for _, o := range ops() {
		t.Run(o.name, func(t *testing.T) {
			w := rig()
			srv, tsrv := seed(t, w)
			err := o.call(t.Context(), w.s, viewer, srv, tsrv)
			if reads[o.name] {
				if domain.KindOf(err) == domain.KindAccessDenied {
					t.Fatalf("viewer read %s = %v", o.name, err)
				}
				return
			}
			if domain.KindOf(err) != domain.KindAccessDenied {
				t.Fatalf("viewer %s = %v", o.name, err)
			}
		})
	}
}

// TestRepositoryFailuresPropagate fails each repository call in turn through every use case, as the
// service admin and the team admin, and checks the failure comes back rather than being swallowed.
func TestRepositoryFailuresPropagate(t *testing.T) {
	for _, o := range ops() {
		for _, c := range []acting.Caller{svcAdmin, teamAdmin} {
			for i := 1; i < 40; i++ {
				w := rig()
				srv, tsrv := seed(t, w)
				f := &flaky{repo: w.r, failAt: i}
				w.s.Repo = f
				err := o.call(t.Context(), w.s, c, srv, tsrv)
				if f.calls < i {
					break // the use case finished before the failure point
				}
				if err == nil {
					t.Errorf("%s as %s: failure at call %d swallowed", o.name, c.Principal.ID, i)
				}
			}
		}
	}
}

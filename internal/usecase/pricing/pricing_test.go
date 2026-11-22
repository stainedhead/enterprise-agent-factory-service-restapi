package pricing_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pricing"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/pricing/pricingtest"
)

var now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

const model = "anthropic.claude-sonnet-4-5-v1:0"

func proposal(id string, eff time.Time) pricing.Proposal {
	return pricing.Proposal{ID: id, ModelID: model, EffectiveFrom: eff, Input: domain.MustMoney("3"), Output: domain.MustMoney("15"),
		FetchedAt: now.Add(-time.Hour), Status: pricing.Pending}
}

type rigT struct {
	svc   *pricing.Service
	repo  *pricingtest.Repo
	audit *fakes.Audit
	ob    *fakes.Outbox
}

func rig() rigT {
	repo := &pricingtest.Repo{Now: now, Actor: "usr_root", Proposals: map[string]pricing.Proposal{
		"mpp_1": proposal("mpp_1", now.Add(48*time.Hour)),  // in the future
		"mpp_2": proposal("mpp_2", now.Add(-48*time.Hour)), // already past
	}}
	audit, ob := &fakes.Audit{}, &fakes.Outbox{}
	return rigT{&pricing.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: audit, Outbox: ob, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: now}, IDs: &fakes.IDGen{}}, Repo: repo}, repo, audit, ob}
}

func admin() acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_root", Type: authz.PrincipalUser, ServiceAdmin: true}}
}

func callers() map[string]acting.Caller {
	bound := func(role authz.Role) acting.Caller {
		return acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: role, TeamID: "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"}}}}
	}
	return map[string]acting.Caller{
		"team admin":        bound(authz.RoleTeamAdmin),
		"team viewer":       bound(authz.RoleTeamViewer),
		"security operator": {Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, SecurityOperator: true}},
		"unbound":           {Principal: authz.Principal{ID: "usr_x", Type: authz.PrincipalUser}},
		"api key":           {Principal: authz.Principal{ID: "key_1", Type: authz.PrincipalAPIKey}},
	}
}

func TestOnlyServiceAdminsReadOrDecide(t *testing.T) {
	for name, c := range callers() {
		t.Run(name, func(t *testing.T) {
			r := rig()
			if _, _, err := r.svc.List(t.Context(), c, "", acting.Page{}); domain.KindOf(err) != domain.KindAccessDenied {
				t.Fatalf("List: %v", err)
			}
			if _, err := r.svc.Approve(t.Context(), c, "mpp_1", "", nil); domain.KindOf(err) != domain.KindAccessDenied {
				t.Fatalf("Approve: %v", err)
			}
			if _, err := r.svc.Reject(t.Context(), c, "mpp_1", "", nil); domain.KindOf(err) != domain.KindAccessDenied {
				t.Fatalf("Reject: %v", err)
			}
			if r.repo.Proposals["mpp_1"].Status != pricing.Pending || len(r.repo.Prices) != 0 || len(r.ob.Events) != 0 {
				t.Fatal("a refused call changed something")
			}
			// Each refusal is audited as a denial and nothing else.
			if len(r.audit.Entries) != 3 {
				t.Fatalf("audit entries = %d", len(r.audit.Entries))
			}
		})
	}
}

func TestApproveWritesThePriceFromTheLaterOfEffectiveFromAndNow(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want time.Time
	}{
		{"mpp_1", now.Add(48 * time.Hour)}, // future effectiveFrom is kept: nothing earlier is repriced
		{"mpp_2", now},                     // past effectiveFrom is moved to the approval: no earlier usage is repriced
	} {
		t.Run(tc.id, func(t *testing.T) {
			r := rig()
			res, err := r.svc.Approve(t.Context(), admin(), tc.id, "ok", nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Proposal.Status != pricing.Approved || res.Proposal.Note != "ok" || res.Proposal.Version() != 2 {
				t.Fatalf("proposal = %+v", res.Proposal)
			}
			if len(r.repo.Prices) != 1 || !r.repo.Prices[0].EffectiveFrom.Equal(tc.want) {
				t.Fatalf("prices = %+v, want effective %v", r.repo.Prices, tc.want)
			}
			if len(r.ob.Events) != 1 {
				t.Fatalf("events = %d", len(r.ob.Events))
			}
			ev := r.ob.Events[0]
			if ev.Type != domain.EventPricingProposalDecided || ev.AggregateID != tc.id || ev.Detail.Data["decision"] != "approved" ||
				ev.Detail.Data["effectiveFrom"] != tc.want.Format(time.RFC3339) {
				t.Fatalf("event = %+v", ev)
			}
			if len(r.audit.Entries) != 1 || r.audit.Entries[0].Record.Action != "ApproveModelPriceProposal" {
				t.Fatalf("audit = %+v", r.audit.Entries)
			}
		})
	}
}

func TestRejectWritesNoPrice(t *testing.T) {
	r := rig()
	res, err := r.svc.Reject(t.Context(), admin(), "mpp_1", "contract rate pending", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Proposal.Status != pricing.Rejected || len(r.repo.Prices) != 0 {
		t.Fatalf("proposal %+v prices %+v", res.Proposal, r.repo.Prices)
	}
	ev := r.ob.Events[0]
	if ev.Detail.Data["decision"] != "rejected" || ev.Detail.Data["effectiveFrom"] != nil {
		t.Fatalf("event = %+v", ev.Detail.Data)
	}
}

func TestDecisionErrors(t *testing.T) {
	r := rig()
	if _, err := r.svc.Approve(t.Context(), admin(), "mpp_9", "", nil); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := r.svc.Approve(t.Context(), admin(), "mpp_1", "", nil); err != nil {
		t.Fatal(err)
	}
	// An approved proposal cannot be approved again or rejected, and a superseded one cannot move.
	p := r.repo.Proposals["mpp_2"]
	p.Status = pricing.Superseded
	r.repo.Proposals["mpp_2"] = p
	for _, id := range []string{"mpp_1", "mpp_2"} {
		if _, err := r.svc.Approve(t.Context(), admin(), id, "", nil); domain.KindOf(err) != domain.KindConflict {
			t.Fatalf("approve %s again: %v", id, err)
		}
		if _, err := r.svc.Reject(t.Context(), admin(), id, "", nil); domain.KindOf(err) != domain.KindConflict {
			t.Fatalf("reject %s: %v", id, err)
		}
	}
	if len(r.repo.Prices) != 1 || len(r.ob.Events) != 1 {
		t.Fatalf("prices %d events %d", len(r.repo.Prices), len(r.ob.Events))
	}
	long := make([]byte, 501)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := rig().svc.Approve(t.Context(), admin(), "mpp_1", string(long), nil); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("long note: %v", err)
	}
}

func idemFor(key string) *acting.Idem {
	return &acting.Idem{Key: port.IdempotencyKey{Key: key, Operation: "ApproveModelPriceProposal"},
		Render: func(v any) (int, json.RawMessage, error) {
			b, err := json.Marshal(v.(pricing.Proposal).ID)
			return 200, b, err
		}}
}

func TestApproveIsIdempotent(t *testing.T) {
	r := rig()
	first, err := r.svc.Approve(t.Context(), admin(), "mpp_1", "", idemFor("k1"))
	if err != nil || first.Replay != nil {
		t.Fatalf("first: %+v, %v", first, err)
	}
	again, err := r.svc.Approve(t.Context(), admin(), "mpp_1", "", idemFor("k1"))
	if err != nil || again.Replay == nil {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	if len(r.repo.Prices) != 1 || len(r.ob.Events) != 1 || len(r.audit.Entries) != 1 {
		t.Fatalf("prices %d events %d audit %d", len(r.repo.Prices), len(r.ob.Events), len(r.audit.Entries))
	}
}

func TestListFiltersAndPages(t *testing.T) {
	r := rig()
	r.repo.Proposals["mpp_3"] = proposal("mpp_3", now)
	p := r.repo.Proposals["mpp_2"]
	p.Status = pricing.Superseded
	r.repo.Proposals["mpp_2"] = p

	rows, next, err := r.svc.List(t.Context(), admin(), "pending", acting.Page{})
	if err != nil || len(rows) != 2 || next != "" || rows[0].ID != "mpp_3" {
		t.Fatalf("pending: %+v %q %v", rows, next, err)
	}
	rows, next, err = r.svc.List(t.Context(), admin(), "", acting.Page{Limit: 2})
	if err != nil || len(rows) != 2 || next != "mpp_2" {
		t.Fatalf("page 1: %+v %q %v", rows, next, err)
	}
	rows, next, err = r.svc.List(t.Context(), admin(), "", acting.Page{Limit: 2, Cursor: next})
	if err != nil || len(rows) != 1 || rows[0].ID != "mpp_1" || next != "" {
		t.Fatalf("page 2: %+v %q %v", rows, next, err)
	}
	if _, _, err := r.svc.List(t.Context(), admin(), "bogus", acting.Page{}); domain.KindOf(err) != domain.KindValidation {
		t.Fatalf("bad status: %v", err)
	}
}

func TestEffectiveAtWithoutADecision(t *testing.T) {
	p := proposal("mpp_1", now)
	if !p.EffectiveAt().Equal(now) || p.Version() != 1 {
		t.Fatalf("%v %v", p.EffectiveAt(), p.Version())
	}
}

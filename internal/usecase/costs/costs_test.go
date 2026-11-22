package costs_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/costs"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/costs/coststest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

const (
	team  = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	other = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
)

var now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func day(s string) time.Time { t, _ := time.Parse(time.DateOnly, s); return t }

func rig() (*costs.Service, *coststest.Repo, *fakes.Audit) {
	repo := &coststest.Repo{Teams: map[string]bool{team: true, other: true}, Costs: map[string][]costs.Row{}}
	audit := &fakes.Audit{}
	return &costs.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: audit, Outbox: &fakes.Outbox{}, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: now}, IDs: &fakes.IDGen{}}, Repo: repo}, repo, audit
}

func caller(role authz.Role, tm string) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_c", Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: role, TeamID: tm}}}}
}

func admin() acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: "usr_root", Type: authz.PrincipalUser, ServiceAdmin: true}}
}

func TestListCostsAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		caller acting.Caller
		team   string
		want   domain.ErrorKind // "" allowed
	}{
		{"viewer of the team", caller(authz.RoleTeamViewer, team), team, ""},
		{"team admin of the team", caller(authz.RoleTeamAdmin, team), team, ""},
		{"service admin", admin(), team, ""},
		// 404 before 403: a team the caller is not bound to is not disclosed.
		{"viewer of another team", caller(authz.RoleTeamViewer, other), team, domain.KindNotFound},
		{"unbound", acting.Caller{Principal: authz.Principal{ID: "usr_x", Type: authz.PrincipalUser}}, team, domain.KindNotFound},
		// A security operator is 403 on every team route.
		{"security operator", acting.Caller{Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, SecurityOperator: true}}, team, domain.KindAccessDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _ := rig()
			_, _, err := svc.ListCosts(t.Context(), tc.caller, tc.team, "", "", acting.Page{})
			if got := domain.KindOf(err); got != tc.want {
				t.Fatalf("kind = %q (%v), want %q", got, err, tc.want)
			}
		})
	}
}

func TestListCostsUnknownTeamIsNotFound(t *testing.T) {
	svc, repo, _ := rig()
	delete(repo.Teams, team)
	if _, _, err := svc.ListCosts(t.Context(), admin(), team, "", "", acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestListCostsRangeAndPaging(t *testing.T) {
	svc, repo, _ := rig()
	for i := range 5 {
		repo.Costs[team] = append(repo.Costs[team], costs.Row{Day: day(fmt.Sprintf("2026-11-%02d", 16+i)), Source: "gateway", CostUSD: domain.MustMoney("1")})
	}
	// The default range is the last 7 days ending now, as for reports.
	rows, next, err := svc.ListCosts(t.Context(), admin(), team, "", "", acting.Page{Limit: 2})
	if err != nil || len(rows) != 2 || next == "" {
		t.Fatalf("rows=%d next=%q err=%v", len(rows), next, err)
	}
	if repo.LastFrom.Format(time.DateOnly) != "2026-11-15" || repo.LastTo.Format(time.DateOnly) != "2026-11-22" || repo.LastLimit != 3 {
		t.Fatalf("range = %v..%v limit %d", repo.LastFrom, repo.LastTo, repo.LastLimit)
	}
	var seen []string
	for _, r := range rows {
		seen = append(seen, r.Day.Format(time.DateOnly))
	}
	for next != "" {
		rows, nx, err := svc.ListCosts(t.Context(), admin(), team, "", "", acting.Page{Limit: 2, Cursor: next})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			seen = append(seen, r.Day.Format(time.DateOnly))
		}
		next = nx
	}
	if len(seen) != 5 || seen[0] != "2026-11-16" || seen[4] != "2026-11-20" {
		t.Fatalf("pages = %v", seen)
	}

	// An explicit range selects every UTC day it touches.
	rows, _, err = svc.ListCosts(t.Context(), admin(), team, "2026-11-17T18:00:00Z", "2026-11-18T01:00:00Z", acting.Page{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %v err=%v", rows, err)
	}
}

func TestListCostsValidation(t *testing.T) {
	svc, repo, _ := rig()
	repo.Costs[team] = []costs.Row{{Day: day("2026-11-10"), Source: "gateway"}, {Day: day("2026-11-11"), Source: "gateway"}}
	_, next, _ := svc.ListCosts(t.Context(), admin(), team, "2026-11-01T00:00:00Z", "2026-11-30T00:00:00Z", acting.Page{Limit: 1})
	for name, tc := range map[string]struct{ from, to, cursor string }{
		"from not a timestamp":   {"yesterday", "", ""},
		"from after to":          {"2026-11-20T00:00:00Z", "2026-11-10T00:00:00Z", ""},
		"range over 400 days":    {"2024-01-01T00:00:00Z", "2026-11-01T00:00:00Z", ""},
		"garbage token":          {"", "", "abc"},
		"negative offset":        {"", "", "-1:x"},
		"token of another range": {"2026-11-02T00:00:00Z", "2026-11-30T00:00:00Z", next},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := svc.ListCosts(t.Context(), admin(), team, tc.from, tc.to, acting.Page{Cursor: tc.cursor})
			if domain.KindOf(err) != domain.KindValidation {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestListReconciliations(t *testing.T) {
	svc, repo, audit := rig()
	for i, st := range []string{"ok", "over_threshold", "missing_billing", "over_threshold"} {
		repo.Recon = append(repo.Recon, costs.Reconciliation{TeamID: fmt.Sprintf("team_%026d", i), Day: day(fmt.Sprintf("2026-11-%02d", 20-i/2)),
			GatewayUSD: domain.MustMoney("1"), BilledUSD: domain.MustMoney("1"), Status: st})
	}
	rows, _, err := svc.ListReconciliations(t.Context(), admin(), "", acting.Page{})
	if err != nil || len(rows) != 4 || rows[0].Day.Before(rows[3].Day) {
		t.Fatalf("rows = %+v err=%v", rows, err)
	}
	rows, _, err = svc.ListReconciliations(t.Context(), admin(), "over_threshold", acting.Page{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("filtered rows = %d err=%v", len(rows), err)
	}
	// Paging with a keyset cursor visits every row once.
	var got []string
	cur := ""
	for {
		page, next, err := svc.ListReconciliations(t.Context(), admin(), "", acting.Page{Limit: 3, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			got = append(got, r.TeamID)
		}
		if next == "" {
			break
		}
		cur = next
	}
	if len(got) != 4 {
		t.Fatalf("paged %v", got)
	}
	for _, bad := range []string{"nope", "OK"} {
		if _, _, err := svc.ListReconciliations(t.Context(), admin(), bad, acting.Page{}); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("status %q: err = %v", bad, err)
		}
	}
	if _, _, err := svc.ListReconciliations(t.Context(), admin(), "", acting.Page{Cursor: "x"}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("bad cursor: %v", err)
	}

	// Only service admins hold admin:reports; every other caller is denied and the denial is audited.
	for name, c := range map[string]acting.Caller{
		"team admin": caller(authz.RoleTeamAdmin, team), "viewer": caller(authz.RoleTeamViewer, team),
		"security operator": {Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, SecurityOperator: true}},
	} {
		before := len(audit.Entries)
		_, _, err := svc.ListReconciliations(t.Context(), c, "", acting.Page{})
		var de *domain.Error
		if !errors.As(err, &de) || de.Kind != domain.KindAccessDenied || len(audit.Entries) != before+1 {
			t.Errorf("%s: err = %v, audit rows = %d", name, err, len(audit.Entries)-before)
		}
	}
}

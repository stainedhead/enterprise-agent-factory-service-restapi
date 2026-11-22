package domain

import (
	"strings"
	"testing"
	"time"
)

var woNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

const (
	woTpl  = "swt_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	woTeam = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
)

func TestSLAStateAt(t *testing.T) {
	due := woNow.Add(100 * time.Hour)
	done := func(at time.Time) *time.Time { return &at }
	tests := []struct {
		name string
		w    WorkOrder
		now  time.Time
		want SLAState
	}{
		{"on track", WorkOrder{SLADueAt: due, Status: WorkOrderStatusAssigned}, woNow, SLAStateOnTrack},
		{"due soon", WorkOrder{SLADueAt: due, Status: WorkOrderStatusAssigned}, due.Add(-47 * time.Hour), SLAStateDueSoon},
		{"boundary is on track", WorkOrder{SLADueAt: due, Status: WorkOrderStatusOpen}, due.Add(-48 * time.Hour), SLAStateOnTrack},
		{"breached", WorkOrder{SLADueAt: due, Status: WorkOrderStatusPrOpened}, due.Add(time.Minute), SLAStateBreached},
		{"failed past due is breached", WorkOrder{SLADueAt: due, Status: WorkOrderStatusFailed}, due.Add(time.Minute), SLAStateBreached},
		{"met", WorkOrder{SLADueAt: due, Status: WorkOrderStatusDone, CompletedAt: done(due.Add(-time.Hour))}, due.Add(time.Hour), SLAStateMet},
		{"missed", WorkOrder{SLADueAt: due, Status: WorkOrderStatusDone, CompletedAt: done(due.Add(time.Hour))}, due.Add(2 * time.Hour), SLAStateMissed},
		{"cancelled before due is met", WorkOrder{SLADueAt: due, Status: WorkOrderStatusCancelled, CompletedAt: done(woNow)}, due.Add(time.Hour), SLAStateMet},
		{"done without completion time uses now", WorkOrder{SLADueAt: due, Status: WorkOrderStatusDone}, due.Add(time.Hour), SLAStateMissed},
		{"no due time", WorkOrder{}, woNow, ""},
	}
	for _, tc := range tests {
		if got := tc.w.SLAStateAt(tc.now, 48); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestNewWorkOrder(t *testing.T) {
	ok := NewWorkOrderParams{CVEID: "CVE-2026-1234", Severity: "critical", Title: " OpenSSL RCE ", SLADueAt: woNow.Add(24 * time.Hour), TemplateID: woTpl}
	w, err := NewWorkOrder("wo_x", ok, MustMoney("500"), woNow)
	if err != nil || w.Kind != WorkOrderKindCveRemediation || w.Title != "OpenSSL RCE" || w.Budget.String() != "500.00" || w.Status != WorkOrderStatusOpen || w.Source != WorkOrderSourceApi {
		t.Fatalf("%+v %v", w, err)
	}
	own := ok
	own.BudgetUSD = "12.5"
	if w, _ := NewWorkOrder("wo_x", own, MustMoney("500"), woNow); w.Budget.String() != "12.50" {
		t.Errorf("explicit budget wins: %s", w.Budget)
	}
	gen := ok
	gen.CVEID = ""
	if w, err := NewWorkOrder("wo_x", gen, MustMoney("1"), woNow); err != nil || w.Kind != WorkOrderKindGeneral {
		t.Errorf("general default: %+v %v", w, err)
	}
	for name, mut := range map[string]func(*NewWorkOrderParams){
		"cve format":     func(p *NewWorkOrderParams) { p.CVEID = "CVE-1" },
		"cve required":   func(p *NewWorkOrderParams) { p.Kind = "cve_remediation"; p.CVEID = "" },
		"kind":           func(p *NewWorkOrderParams) { p.Kind = "x" },
		"severity":       func(p *NewWorkOrderParams) { p.Severity = "urgent" },
		"title empty":    func(p *NewWorkOrderParams) { p.Title = "  " },
		"title long":     func(p *NewWorkOrderParams) { p.Title = strings.Repeat("x", 201) },
		"sla missing":    func(p *NewWorkOrderParams) { p.SLADueAt = time.Time{} },
		"sla past":       func(p *NewWorkOrderParams) { p.SLADueAt = woNow },
		"template":       func(p *NewWorkOrderParams) { p.TemplateID = "tpl_x" },
		"budget zero":    func(p *NewWorkOrderParams) { p.BudgetUSD = "0" },
		"budget non-num": func(p *NewWorkOrderParams) { p.BudgetUSD = "lots" },
	} {
		p := ok
		mut(&p)
		if _, err := NewWorkOrder("wo_x", p, MustMoney("1"), woNow); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestNormalizeRepoInputs(t *testing.T) {
	got, err := NormalizeRepoInputs("repos", []RepoInput{{Repo: "acme/api"}, {Host: "GitHub.Example.com", Repo: "acme/web", OwningContact: "a@example.com", OwningTeamID: woTeam}}, true)
	if err != nil || got[0].Host != "github.com" || got[1].Host != "github.example.com" || got[0].HasOwner() || !got[1].HasOwner() {
		t.Fatalf("%+v %v", got, err)
	}
	for name, in := range map[string][]RepoInput{
		"empty":    nil,
		"path":     {{Repo: "noslash"}},
		"host":     {{Host: "bad host", Repo: "a/b"}},
		"contact":  {{Repo: "a/b", OwningContact: "nope"}},
		"team":     {{Repo: "a/b", OwningTeamID: "team_x"}},
		"repeat":   {{Repo: "a/b"}, {Host: "GITHUB.com", Repo: "A/B"}},
		"too many": make([]RepoInput, MaxWorkOrderRepos+1),
	} {
		if _, err := NormalizeRepoInputs("repos", in, true); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, err := NormalizeRepoInputs("addRepos", nil, false); err != nil {
		t.Errorf("optional empty: %v", err)
	}
}

func TestWorkOrderApplyAndCancel(t *testing.T) {
	w := WorkOrder{ID: "wo_x", Status: WorkOrderStatusAssigned, Title: "t", SLADueAt: woNow.Add(time.Hour), Budget: MustMoney("1")}
	title, sla, b := "new", woNow.Add(48*time.Hour), "9"
	u, err := w.Apply(WorkOrderPatch{Title: &title, SLADueAt: &sla, BudgetUSD: &b}, woNow)
	if err != nil || u.Title != "new" || !u.SLADueAt.Equal(sla) || u.Budget.String() != "9.00" {
		t.Fatalf("%+v %v", u, err)
	}
	past, empty := woNow.Add(-time.Hour), " "
	if _, err := w.Apply(WorkOrderPatch{SLADueAt: &past, Title: &empty}, woNow); err == nil {
		t.Error("bad patch accepted")
	}
	if !(WorkOrderPatch{}).Empty() || (WorkOrderPatch{Title: &title}).Empty() {
		t.Error("Empty")
	}
	c, err := w.Cancel(" no longer needed ", woNow)
	if err != nil || c.Status != WorkOrderStatusCancelled || c.CancelReason != "no longer needed" || c.CompletedAt == nil || !c.CompletedAt.Equal(woNow) {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := w.Cancel("", woNow); KindOf(err) != KindValidation {
		t.Errorf("empty reason: %v", err)
	}
	for _, st := range []WorkOrderStatus{WorkOrderStatusDone, WorkOrderStatusFailed, WorkOrderStatusCancelled} {
		cl := WorkOrder{Status: st}
		if _, err := cl.Cancel("x", woNow); KindOf(err) != KindConflict {
			t.Errorf("cancel %s: %v", st, err)
		}
		if _, err := cl.Apply(WorkOrderPatch{Title: &title}, woNow); KindOf(err) != KindConflict {
			t.Errorf("apply %s: %v", st, err)
		}
		if KindOf(cl.CanAddRepos()) != KindConflict {
			t.Errorf("add repos %s", st)
		}
	}
	if w.CanAddRepos() != nil {
		t.Error("open work order takes repos")
	}
}

func TestRepoReport(t *testing.T) {
	r := WorkOrderRepo{Host: "github.com", Repo: "a/b", Status: RepoStatusPending}
	detail := "bumped"
	pr, err := r.Apply(RepoReport{Status: "pr_opened", PRURL: "https://github.com/a/b/pull/1", PRNumber: 1, Detail: &detail}, woNow)
	if err != nil || pr.Status != RepoStatusPrOpened || pr.PRNumber != 1 || pr.Detail != "bumped" || pr.MergedAt != nil {
		t.Fatalf("%+v %v", pr, err)
	}
	m, err := pr.Apply(RepoReport{Status: "merged"}, woNow) // the recorded URL is enough
	if err != nil || m.MergedAt == nil || !m.MergedAt.Equal(woNow) {
		t.Fatalf("%+v %v", m, err)
	}
	if again, err := m.Apply(RepoReport{Status: "merged"}, woNow.Add(time.Hour)); err != nil || !again.MergedAt.Equal(woNow) {
		t.Errorf("merged twice keeps merged_at: %+v %v", again, err)
	}
	if _, err := m.Apply(RepoReport{Status: "failed"}, woNow); KindOf(err) != KindConflict {
		t.Errorf("merged is terminal: %v", err)
	}
	for name, rep := range map[string]RepoReport{
		"pr_opened without URL": {Status: "pr_opened"},
		"merged without URL":    {Status: "merged"},
		"bad status":            {Status: "weird"},
		"http URL":              {Status: "in_progress", PRURL: "http://x/y"},
		"pr number":             {Status: "in_progress", PRNumber: -1},
		"detail":                {Status: "failed", Detail: ptr(strings.Repeat("x", 1001))},
	} {
		if _, err := r.Apply(rep, woNow); KindOf(err) != KindValidation {
			t.Errorf("%s: %v", name, err)
		}
	}
	if ok, err := r.Apply(RepoReport{Status: "not_affected"}, woNow); err != nil || ok.Status != RepoStatusNotAffected {
		t.Errorf("not_affected %+v %v", ok, err)
	}
	if r.Key() != "github.com/a/b" || len(r.Audit()) == 0 || len((WorkOrder{}).Audit()) == 0 {
		t.Error("key/audit")
	}
}

func TestOwnerResolutionError(t *testing.T) {
	err := OwnerResolutionError([]AmbiguousRepo{{Repo: "a/b", TeamIDs: []string{"team_1", "team_2"}}}, []string{"c/d"})
	e := err.(*Error)
	if e.Kind != KindValidation || e.Details["unownedRepos"] == nil || e.Details["ambiguousRepos"] == nil ||
		!strings.Contains(e.Message, "a/b") || !strings.Contains(e.Message, "=team_") {
		t.Fatalf("%+v", e)
	}
}

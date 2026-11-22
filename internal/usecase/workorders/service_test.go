package workorders_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/authz"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/acting"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/workorders"
)

const (
	tplID  = "swt_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamA  = "team_01JD2Q3R4S5T6V7W8X9Y0Z000A"
	teamB  = "team_01JD2Q3R4S5T6V7W8X9Y0Z000B"
	teamC  = "team_01JD2Q3R4S5T6V7W8X9Y0Z000C"
	ghost  = "team_01JD2Q3R4S5T6V7W8X9Y0Z000Z"
	prURL1 = "https://github.com/acme/api/pull/1"
)

type fakeRepo struct {
	tpl     map[string]workorders.TemplateRef
	teams   map[string]bool
	owners  map[string][]string // host/repo -> team ids
	wos     map[string]domain.WorkOrder
	repos   map[string][]domain.WorkOrderRepo
	inserts int
}

func (r *fakeRepo) Template(_ context.Context, _ port.Tx, id string) (workorders.TemplateRef, error) {
	t, ok := r.tpl[id]
	if !ok {
		return t, domain.NotFound("swarm template")
	}
	return t, nil
}
func (r *fakeRepo) ExistingTeams(_ context.Context, _ port.Tx, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = r.teams[id]
	}
	return out, nil
}
func (r *fakeRepo) TeamVisible(_ context.Context, _ port.Tx, id string) (bool, error) {
	return r.teams[id], nil
}
func (r *fakeRepo) ResolveOwners(_ context.Context, _ port.Tx, host, repo string) ([]string, error) {
	return r.owners[host+"/"+repo], nil
}
func (r *fakeRepo) OpenCVE(_ context.Context, _ port.Tx, cve string) (bool, error) {
	for _, w := range r.wos {
		if w.CVEID == cve && w.Open() {
			return true, nil
		}
	}
	return false, nil
}
func (r *fakeRepo) Insert(_ context.Context, _ port.Tx, w domain.WorkOrder, repos []domain.WorkOrderRepo) error {
	r.inserts++
	r.wos[w.ID] = w
	r.repos[w.ID] = repos
	return nil
}
func (r *fakeRepo) Get(_ context.Context, _ port.Tx, id string, _ bool) (domain.WorkOrder, error) {
	w, ok := r.wos[id]
	if !ok {
		return w, domain.NotFound("work order")
	}
	return w, nil
}
func (r *fakeRepo) List(_ context.Context, _ port.Tx, f workorders.Filter, after string, limit int) ([]domain.WorkOrder, error) {
	var out []domain.WorkOrder
	for _, w := range r.wos {
		if (after != "" && w.ID >= after) || (f.Status != "" && string(w.Status) != f.Status) || (f.TeamID != "" && w.TeamID != f.TeamID) {
			continue
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (r *fakeRepo) Update(_ context.Context, _ port.Tx, w domain.WorkOrder, expect domain.Version) (domain.WorkOrder, error) {
	if r.wos[w.ID].Version != expect {
		return w, domain.PreconditionFailed()
	}
	w.Version = expect + 1
	r.wos[w.ID] = w
	return w, nil
}
func (r *fakeRepo) Repos(_ context.Context, _ port.Tx, id, after string, limit int) ([]domain.WorkOrderRepo, error) {
	var out []domain.WorkOrderRepo
	for _, x := range r.repos[id] {
		if x.Key() > after {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (r *fakeRepo) OwnerTeams(_ context.Context, _ port.Tx, id string) ([]string, error) {
	var out []string
	for _, x := range r.repos[id] {
		if x.OwningTeamID != "" {
			out = append(out, x.OwningTeamID)
		}
	}
	return out, nil
}
func (r *fakeRepo) Repo(_ context.Context, _ port.Tx, id, host, repo string, _ bool) (domain.WorkOrderRepo, error) {
	for _, x := range r.repos[id] {
		if x.Host == host && x.Repo == repo {
			return x, nil
		}
	}
	return domain.WorkOrderRepo{}, domain.NotFound("repository")
}
func (r *fakeRepo) AddRepos(_ context.Context, _ port.Tx, id string, repos []domain.WorkOrderRepo) error {
	r.repos[id] = append(r.repos[id], repos...)
	return nil
}
func (r *fakeRepo) UpdateRepo(_ context.Context, _ port.Tx, id string, n domain.WorkOrderRepo, expect domain.Version) (domain.WorkOrderRepo, error) {
	for i, x := range r.repos[id] {
		if x.Host == n.Host && x.Repo == n.Repo {
			if x.Version != expect {
				return n, domain.PreconditionFailed()
			}
			n.Version = expect + 1
			r.repos[id][i] = n
			return n, nil
		}
	}
	return n, domain.NotFound("repository")
}

func user(id string, role authz.Role, team string) acting.Caller {
	return acting.Caller{Principal: authz.Principal{ID: id, Type: authz.PrincipalUser, Bindings: []authz.Binding{{Role: role, TeamID: team}}}}
}

var (
	svcAdmin = acting.Caller{Principal: authz.Principal{ID: "usr_s", Type: authz.PrincipalUser, ServiceAdmin: true}}
	secOp    = acting.Caller{Principal: authz.Principal{ID: "key_o", Type: authz.PrincipalAPIKey, SecurityOperator: true}}
	adminA   = user("usr_aa", authz.RoleTeamAdmin, teamA)
	viewerA  = user("usr_va", authz.RoleTeamViewer, teamA)
	adminB   = user("usr_ab", authz.RoleTeamAdmin, teamB)
	adminC   = user("usr_ac", authz.RoleTeamAdmin, teamC)
	workload = acting.Caller{Principal: authz.Principal{ID: "pool_1", Type: authz.PrincipalWorkload, Workload: &authz.Workload{TeamID: teamA, PoolID: "pool_1", WorkerPod: "pod-1"}}}
	otherWl  = acting.Caller{Principal: authz.Principal{ID: "pool_2", Type: authz.PrincipalWorkload, Workload: &authz.Workload{TeamID: teamB, PoolID: "pool_2"}}}
)

func rig() (*workorders.Service, *fakeRepo, *fakes.Audit, *fakes.Outbox) {
	a, o := &fakes.Audit{}, &fakes.Outbox{}
	r := &fakeRepo{
		tpl:    map[string]workorders.TemplateRef{tplID: {ID: tplID, Name: "cve-fix", Enabled: true, Budget: domain.MustMoney("500")}},
		teams:  map[string]bool{teamA: true, teamB: true, teamC: true},
		owners: map[string][]string{"github.com/acme/api": {teamA}, "github.com/acme/both": {teamB, teamA}},
		wos:    map[string]domain.WorkOrder{}, repos: map[string][]domain.WorkOrderRepo{},
	}
	st := &fakes.Settings{}
	return &workorders.Service{Deps: acting.Deps{Tx: &fakes.TxRunner{}, Audit: a, Outbox: o, Idem: &fakes.Idempotency{},
		Clock: &fakes.Clock{T: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}, IDs: &fakes.IDGen{}}, Repo: r, Settings: st}, r, a, o
}

func params() domain.NewWorkOrderParams {
	return domain.NewWorkOrderParams{CVEID: "CVE-2026-1234", Severity: "critical", Title: "OpenSSL", TemplateID: tplID,
		SLADueAt: time.Date(2026, 11, 29, 0, 0, 0, 0, time.UTC)}
}

func create(t *testing.T, s *workorders.Service, repos ...domain.RepoInput) domain.WorkOrder {
	t.Helper()
	res, err := s.Create(t.Context(), secOp, params(), repos, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.WorkOrder
}

func TestCreateOwnerResolution(t *testing.T) {
	s, r, audit, outbox := rig()
	ctx := t.Context()
	res, err := s.Create(ctx, secOp, params(), []domain.RepoInput{{Repo: "acme/api"}, {Repo: "acme/x", OwningContact: "a@example.com"},
		{Repo: "acme/y", OwningTeamID: teamB}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := res.WorkOrder
	if w.Status != domain.WorkOrderStatusOpen || w.Budget.String() != "500.00" || w.SLAState != domain.SLAStateOnTrack || len(w.Repos) != 3 {
		t.Fatalf("%+v", w)
	}
	by := map[string]domain.WorkOrderRepo{}
	for _, x := range w.Repos {
		by[x.Repo] = x
	}
	if x := by["acme/api"]; x.OwningTeamID != teamA || x.OwnerSource != domain.OwnerSourceTeamConfig {
		t.Errorf("resolved: %+v", x)
	}
	if x := by["acme/x"]; x.OwnerSource != domain.OwnerSourceExplicit || x.OwningContact != "a@example.com" {
		t.Errorf("contact: %+v", x)
	}
	if x := by["acme/y"]; x.OwningTeamID != teamB || x.OwnerSource != domain.OwnerSourceExplicit {
		t.Errorf("explicit team: %+v", x)
	}
	if len(audit.Entries) != 1 || len(outbox.Events) != 1 || outbox.Events[0].Type != domain.EventWorkOrderCreated {
		t.Errorf("audit %d outbox %v", len(audit.Entries), outbox.Events)
	}

	// Errors write nothing.
	r.inserts = 0
	for name, tc := range map[string]struct {
		repos []domain.RepoInput
		check func(*testing.T, *domain.Error)
	}{
		"ambiguous": {[]domain.RepoInput{{Repo: "acme/both"}}, func(t *testing.T, e *domain.Error) {
			l := e.Details["ambiguousRepos"].([]map[string]any)
			if len(l) != 1 || l[0]["repo"] != "github.com/acme/both" || len(l[0]["teamIds"].([]string)) != 2 {
				t.Errorf("%v", e.Details)
			}
		}},
		"unowned": {[]domain.RepoInput{{Repo: "acme/none"}}, func(t *testing.T, e *domain.Error) {
			if l := e.Details["unownedRepos"].([]string); len(l) != 1 || l[0] != "github.com/acme/none" {
				t.Errorf("%v", e.Details)
			}
		}},
		"unknown explicit team": {[]domain.RepoInput{{Repo: "acme/z", OwningTeamID: ghost}}, func(t *testing.T, e *domain.Error) {
			if len(e.Fields) != 1 || e.Fields[0].Field != "repos[0].owningTeamId" {
				t.Errorf("%v", e.Fields)
			}
		}},
		"one bad among good": {[]domain.RepoInput{{Repo: "acme/api"}, {Repo: "acme/none"}}, func(t *testing.T, e *domain.Error) {}},
		"no repos":           {nil, func(t *testing.T, e *domain.Error) {}},
	} {
		_, err := s.Create(ctx, secOp, params(), tc.repos, nil)
		var de *domain.Error
		if domain.KindOf(err) != domain.KindValidation || !asDomain(err, &de) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		tc.check(t, de)
	}
	if r.inserts != 0 || len(audit.Entries) != 1 || len(outbox.Events) != 1 {
		t.Errorf("an error wrote something: inserts %d audit %d events %d", r.inserts, len(audit.Entries), len(outbox.Events))
	}
}

func asDomain(err error, out **domain.Error) bool {
	ok := errors.As(err, out)
	return ok
}

func TestCreateRules(t *testing.T) {
	s, r, _, _ := rig()
	ctx := t.Context()
	repos := []domain.RepoInput{{Repo: "acme/api"}}
	create(t, s, repos...)
	if _, err := s.Create(ctx, svcAdmin, params(), repos, nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("second open work order for the CVE = %v", err)
	}
	other := params()
	other.CVEID = "CVE-2026-9999"
	if _, err := s.Create(ctx, svcAdmin, other, repos, nil); err != nil {
		t.Errorf("another CVE: %v", err)
	}
	// Closed work orders free the CVE.
	for id, w := range r.wos {
		w.Status = domain.WorkOrderStatusDone
		r.wos[id] = w
	}
	if _, err := s.Create(ctx, svcAdmin, params(), repos, nil); err != nil {
		t.Errorf("CVE after the old one closed: %v", err)
	}
	// Disabled and unknown templates are 400s.
	tpl := r.tpl[tplID]
	tpl.Enabled = false
	r.tpl[tplID] = tpl
	p := params()
	p.CVEID = "CVE-2026-1111"
	if _, err := s.Create(ctx, svcAdmin, p, repos, nil); domain.KindOf(err) != domain.KindValidation || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("disabled template = %v", err)
	}
	p.TemplateID = "swt_01JD2Q3R4S5T6V7W8X9Y0Z0009"
	if _, err := s.Create(ctx, svcAdmin, p, repos, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("unknown template = %v", err)
	}
}

func TestPermissions(t *testing.T) {
	s, _, audit, _ := rig()
	ctx := t.Context()
	w := create(t, s, domain.RepoInput{Repo: "acme/api"}) // owned by team A, not assigned
	for name, c := range map[string]acting.Caller{"team admin": adminA, "team viewer": viewerA, "workload": workload} {
		if _, err := s.Create(ctx, c, params(), []domain.RepoInput{{Repo: "acme/api"}}, nil); domain.KindOf(err) != domain.KindAccessDenied {
			t.Errorf("%s create = %v", name, err)
		}
		if _, err := s.Update(ctx, c, w.ID, 1, domain.WorkOrderPatch{Title: strptr("x")}, nil); domain.KindOf(err) != domain.KindAccessDenied {
			t.Errorf("%s update = %v", name, err)
		}
		if _, err := s.Cancel(ctx, c, w.ID, "x", nil); domain.KindOf(err) != domain.KindAccessDenied {
			t.Errorf("%s cancel = %v", name, err)
		}
	}
	if _, err := s.Get(ctx, workload, w.ID); domain.KindOf(err) != domain.KindAccessDenied {
		t.Errorf("workload read = %v", err)
	}
	if n := countDenied(audit); n != 10 {
		t.Errorf("denied rows = %d", n)
	}
}

func strptr(s string) *string { return &s }

func countDenied(a *fakes.Audit) int {
	n := 0
	for _, e := range a.Entries {
		if e.Own {
			n++
		}
	}
	return n
}

func TestReadVisibility(t *testing.T) {
	s, r, _, _ := rig()
	ctx := t.Context()
	w := create(t, s, domain.RepoInput{Repo: "acme/api"}, domain.RepoInput{Repo: "acme/q", OwningContact: "o@example.com"}) // team A owns acme/api
	// Assign to team B.
	x := r.wos[w.ID]
	x.TeamID, x.Status = teamB, domain.WorkOrderStatusAssigned
	r.wos[w.ID] = x

	for name, c := range map[string]acting.Caller{"service admin": svcAdmin, "security operator": secOp, "assigned team admin": adminB,
		"owner team viewer": viewerA, "owner team admin": adminA} {
		got, err := s.Get(ctx, c, w.ID)
		if err != nil || len(got.Repos) != 2 {
			t.Errorf("%s get = %+v %v", name, got, err)
		}
		if _, _, err := s.ListRepos(ctx, c, w.ID, acting.Page{}); err != nil {
			t.Errorf("%s repos = %v", name, err)
		}
	}
	// A team that is neither assigned nor an owner cannot tell the work order exists.
	for name, c := range map[string]acting.Caller{"unrelated team admin": adminC, "unrelated workload": otherWl} {
		if _, err := s.Get(ctx, c, w.ID); name == "unrelated team admin" && domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("%s get = %v, want NotFound", name, err)
		}
	}
	if _, _, err := s.ListRepos(ctx, adminC, w.ID, acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unrelated repos = %v", err)
	}
	if _, err := s.Get(ctx, adminA, "wo_nope"); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown = %v", err)
	}
}

func TestUpdateRepoPermissions(t *testing.T) {
	s, r, audit, outbox := rig()
	ctx := t.Context()
	w := create(t, s, domain.RepoInput{Repo: "acme/api"})
	x := r.wos[w.ID]
	x.TeamID, x.Status = teamA, domain.WorkOrderStatusAssigned
	r.wos[w.ID] = x
	open := domain.RepoReport{Status: "pr_opened", PRURL: prURL1, PRNumber: 1}

	// Not assigned to B or C: owner-less unrelated callers see nothing; reporters of other teams too.
	for name, c := range map[string]acting.Caller{"unrelated admin": adminC, "other team workload": otherWl} {
		if _, err := s.UpdateRepo(ctx, c, w.ID, "github.com", "acme/api", 1, open); domain.KindOf(err) != domain.KindNotFound {
			t.Errorf("%s = %v, want NotFound", name, err)
		}
	}
	// A viewer of the assigned team holds no report permission.
	if _, err := s.UpdateRepo(ctx, viewerA, w.ID, "github.com", "acme/api", 1, open); domain.KindOf(err) != domain.KindAccessDenied {
		t.Errorf("viewer = %v", err)
	}
	denied := countDenied(audit)
	if denied != 1 {
		t.Errorf("denied rows = %d", denied)
	}
	// An owner team that is not the assigned team may read but not report.
	x.TeamID = teamB
	r.wos[w.ID] = x
	if _, err := s.UpdateRepo(ctx, adminA, w.ID, "github.com", "acme/api", 1, open); domain.KindOf(err) != domain.KindAccessDenied {
		t.Errorf("owner (not assigned) = %v", err)
	}
	x.TeamID = teamA
	r.wos[w.ID] = x

	before := len(outbox.Events)
	got, err := s.UpdateRepo(ctx, workload, w.ID, "github.com", "acme/api", 1, open)
	if err != nil || got.Status != domain.RepoStatusPrOpened || got.Version != 2 {
		t.Fatalf("workload = %+v %v", got, err)
	}
	last := audit.Entries[len(audit.Entries)-1].Record
	if last.PoolID != "pool_1" || last.WorkerPod != "pod-1" || last.Action != "UpdateWorkOrderRepo" || last.TeamID != teamA {
		t.Errorf("audit %+v", last)
	}
	types := []domain.EventType{}
	for _, e := range outbox.Events[before:] {
		types = append(types, e.Type)
	}
	if len(types) != 2 || types[0] != domain.EventWorkOrderRepoUpdated || types[1] != domain.EventWorkOrderPrOpened {
		t.Errorf("events %v", types)
	}
	if outbox.Events[before].Detail.TeamID != teamA {
		t.Errorf("event team %q", outbox.Events[before].Detail.TeamID)
	}
	// Team admin of the assigned team and write holders may also report; merged emits merged.
	if _, err := s.UpdateRepo(ctx, adminA, w.ID, "github.com", "acme/api", 2, domain.RepoReport{Status: "merged"}); err != nil {
		t.Errorf("team admin = %v", err)
	}
	if got := outbox.Events[len(outbox.Events)-1].Type; got != domain.EventWorkOrderMerged {
		t.Errorf("last event %v", got)
	}
	if _, err := s.UpdateRepo(ctx, secOp, w.ID, "github.com", "acme/api", 3, domain.RepoReport{Status: "merged"}); err != nil {
		t.Errorf("write holder = %v", err)
	}
}

func TestUpdateRepoRules(t *testing.T) {
	s, r, _, _ := rig()
	ctx := t.Context()
	w := create(t, s, domain.RepoInput{Repo: "acme/api"})
	x := r.wos[w.ID]
	x.TeamID, x.Status = teamA, domain.WorkOrderStatusAssigned
	r.wos[w.ID] = x
	if _, err := s.UpdateRepo(ctx, adminA, w.ID, "github.com", "acme/api", 1, domain.RepoReport{Status: "pr_opened"}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("pr_opened without link = %v", err)
	}
	if _, err := s.UpdateRepo(ctx, adminA, w.ID, "github.com", "acme/api", 9, domain.RepoReport{Status: "in_progress"}); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale = %v", err)
	}
	if _, err := s.UpdateRepo(ctx, adminA, w.ID, "github.com", "acme/nope", 1, domain.RepoReport{Status: "in_progress"}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown repo = %v", err)
	}
	x.Status = domain.WorkOrderStatusDone
	r.wos[w.ID] = x
	if _, err := s.UpdateRepo(ctx, adminA, w.ID, "github.com", "acme/api", 1, domain.RepoReport{Status: "in_progress"}); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("closed work order = %v", err)
	}
}

func TestUpdateAndCancel(t *testing.T) {
	s, r, audit, outbox := rig()
	ctx := t.Context()
	w := create(t, s, domain.RepoInput{Repo: "acme/api"})
	if _, err := s.Update(ctx, secOp, w.ID, 1, domain.WorkOrderPatch{}, nil); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("empty = %v", err)
	}
	title := "New title"
	n := len(outbox.Events)
	u, err := s.Update(ctx, secOp, w.ID, 1, domain.WorkOrderPatch{Title: &title}, nil)
	if err != nil || u.Title != title || u.Version != 2 || len(outbox.Events) != n {
		t.Fatalf("%+v %v events %d", u, err, len(outbox.Events)-n)
	}
	if _, err := s.Update(ctx, secOp, w.ID, 1, domain.WorkOrderPatch{Title: &title}, nil); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("stale = %v", err)
	}
	u, err = s.Update(ctx, svcAdmin, w.ID, 2, domain.WorkOrderPatch{}, []domain.RepoInput{{Repo: "acme/new", OwningContact: "c@example.com"}})
	if err != nil || len(u.Repos) != 2 || outbox.Events[len(outbox.Events)-1].Type != domain.EventWorkOrderRepoUpdated {
		t.Fatalf("add repos %+v %v", u, err)
	}
	if _, err := s.Update(ctx, svcAdmin, w.ID, 3, domain.WorkOrderPatch{}, []domain.RepoInput{{Repo: "acme/none"}}); domain.KindOf(err) != domain.KindValidation {
		t.Errorf("unresolvable add = %v", err)
	}
	if got := len(r.repos[w.ID]); got != 2 {
		t.Errorf("repos after failed add = %d", got)
	}
	if _, err := s.Update(ctx, svcAdmin, "wo_nope", 1, domain.WorkOrderPatch{Title: &title}, nil); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown = %v", err)
	}

	c, err := s.Cancel(ctx, secOp, w.ID, " not needed ", nil)
	if err != nil || c.WorkOrder.Status != domain.WorkOrderStatusCancelled || c.WorkOrder.CancelReason != "not needed" {
		t.Fatalf("%+v %v", c, err)
	}
	if outbox.Events[len(outbox.Events)-1].Type != domain.EventWorkOrderCancelled {
		t.Error("cancel event")
	}
	if _, err := s.Cancel(ctx, secOp, w.ID, "again", nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("cancel twice = %v", err)
	}
	if _, err := s.Update(ctx, secOp, w.ID, 4, domain.WorkOrderPatch{Title: &title}, nil); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("update closed = %v", err)
	}
	if len(audit.Entries) == 0 {
		t.Error("no audit")
	}
}

func TestIdempotentCancel(t *testing.T) {
	s, _, audit, _ := rig()
	w := create(t, s, domain.RepoInput{Repo: "acme/api"})
	idem := func(sum byte) *acting.Idem {
		return &acting.Idem{Key: port.IdempotencyKey{Key: "k", Operation: "CancelWorkOrder", RequestSHA256: [32]byte{sum}},
			Render: func(any) (int, json.RawMessage, error) { return 200, json.RawMessage(`{}`), nil }}
	}
	if r, err := s.Cancel(t.Context(), secOp, w.ID, "x", idem(1)); err != nil || r.Replay != nil {
		t.Fatal(r, err)
	}
	n := len(audit.Entries)
	if r, err := s.Cancel(t.Context(), secOp, w.ID, "x", idem(1)); err != nil || r.Replay == nil || len(audit.Entries) != n {
		t.Errorf("replay %+v %v", r, err)
	}
	if _, err := s.Cancel(t.Context(), secOp, w.ID, "x", idem(2)); domain.KindOf(err) != domain.KindConflict {
		t.Errorf("mismatch = %v", err)
	}
}

func TestListAndFilters(t *testing.T) {
	s, r, _, _ := rig()
	ctx := t.Context()
	ids := []string{}
	for _, cve := range []string{"CVE-2026-1001", "CVE-2026-1002", "CVE-2026-1003"} {
		p := params()
		p.CVEID = cve
		res, err := s.Create(ctx, secOp, p, []domain.RepoInput{{Repo: "acme/api"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.WorkOrder.ID)
	}
	x := r.wos[ids[0]]
	x.TeamID, x.Status = teamA, domain.WorkOrderStatusAssigned
	r.wos[ids[0]] = x
	rows, next, err := s.List(ctx, secOp, workorders.Filter{}, acting.Page{Limit: 2})
	if err != nil || len(rows) != 2 || next == "" || rows[0].ID < rows[1].ID || rows[0].SLAState == "" {
		t.Fatalf("page 1 %v %q %v", rows, next, err)
	}
	rows, next, _ = s.List(ctx, secOp, workorders.Filter{}, acting.Page{Limit: 2, Cursor: next})
	if len(rows) != 1 || next != "" {
		t.Errorf("page 2 %v %q", rows, next)
	}
	if rows, _, _ := s.List(ctx, secOp, workorders.Filter{Status: "assigned"}, acting.Page{}); len(rows) != 1 {
		t.Errorf("status filter %v", rows)
	}
	for name, f := range map[string]workorders.Filter{"status": {Status: "x"}, "severity": {Severity: "x"}, "slaState": {SLAState: "x"}} {
		if _, _, err := s.List(ctx, secOp, f, acting.Page{}); domain.KindOf(err) != domain.KindValidation {
			t.Errorf("%s = %v", name, err)
		}
	}
	// Team route: members read; unknown team is 404; a security operator is denied by authz.
	if rows, _, err := s.ListForTeam(ctx, adminA, teamA, acting.Page{}); err != nil || len(rows) != 1 {
		t.Errorf("team list %v %v", rows, err)
	}
	if _, _, err := s.ListForTeam(ctx, svcAdmin, ghost, acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown team = %v", err)
	}
	for name, c := range map[string]acting.Caller{"secop": secOp, "workload": workload} {
		if _, _, err := s.ListForTeam(ctx, c, teamA, acting.Page{}); domain.KindOf(err) != domain.KindAccessDenied {
			t.Errorf("%s team list = %v", name, err)
		}
	}
	if _, _, err := s.List(ctx, workload, workorders.Filter{}, acting.Page{}); domain.KindOf(err) != domain.KindAccessDenied {
		t.Errorf("workload list = %v", err)
	}
}

func TestCreateReplay(t *testing.T) {
	s, r, _, _ := rig()
	idem := &acting.Idem{Key: port.IdempotencyKey{Key: "c", Operation: "CreateWorkOrder"}, Render: func(any) (int, json.RawMessage, error) {
		return 201, json.RawMessage(`{}`), nil
	}}
	repos := []domain.RepoInput{{Repo: "acme/api"}}
	if _, err := s.Create(t.Context(), secOp, params(), repos, idem); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Create(t.Context(), secOp, params(), repos, idem); err != nil || res.Replay == nil || r.inserts != 1 {
		t.Errorf("replay %+v %v inserts %d", res, err, r.inserts)
	}
}

func TestSLAWarningSetting(t *testing.T) {
	// The work order is due in 4.5 days (108h) of the fake clock.
	for _, tc := range []struct {
		name string
		raw  string
		want domain.SLAState
	}{
		{"default window 48h", "", domain.SLAStateOnTrack},
		{"wide window", "200", domain.SLAStateDueSoon},
		{"narrow window", "1", domain.SLAStateOnTrack},
		{"garbage falls back", `"x"`, domain.SLAStateOnTrack},
		{"zero falls back", "0", domain.SLAStateOnTrack},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, _ := rig()
			st := &fakes.Settings{}
			if tc.raw != "" {
				st.Set(domain.SettingWorkOrdersSLAWarningHours, json.RawMessage(tc.raw))
			}
			s.Settings = st
			w := create(t, s, domain.RepoInput{Repo: "acme/api"})
			got, err := s.Get(t.Context(), secOp, w.ID)
			if err != nil || got.SLAState != tc.want {
				t.Errorf("sla state %q want %q (%v)", got.SLAState, tc.want, err)
			}
		})
	}
	s, _, _, _ := rig()
	s.Settings = nil
	w := create(t, s, domain.RepoInput{Repo: "acme/api"})
	if got, err := s.Get(t.Context(), secOp, w.ID); err != nil || got.SLAState != domain.SLAStateOnTrack {
		t.Errorf("nil settings: %+v %v", got, err)
	}
}

func TestRepoLimitAndListRepos(t *testing.T) {
	s, r, _, _ := rig()
	ctx := t.Context()
	w := create(t, s, domain.RepoInput{Repo: "acme/api"})
	for i := 0; i < domain.MaxWorkOrderRepos-1; i++ {
		r.repos[w.ID] = append(r.repos[w.ID], domain.WorkOrderRepo{Host: "github.com", Repo: "x/r" + strings.Repeat("a", 1) + itoa(i), OwningTeamID: teamA, Status: domain.RepoStatusPending})
	}
	_, err := s.Update(ctx, secOp, w.ID, 1, domain.WorkOrderPatch{}, []domain.RepoInput{{Repo: "acme/new1", OwningContact: "c@example.com"}, {Repo: "acme/new2", OwningContact: "c@example.com"}})
	if domain.KindOf(err) != domain.KindValidation {
		t.Errorf("over the limit = %v", err)
	}
	page, next, err := s.ListRepos(ctx, secOp, w.ID, acting.Page{Limit: 10})
	if err != nil || len(page) != 10 || next == "" {
		t.Fatalf("page %d next %q %v", len(page), next, err)
	}
	if _, _, err := s.ListRepos(ctx, secOp, "wo_nope", acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("unknown = %v", err)
	}
	if _, _, err := s.ListRepos(ctx, adminC, w.ID, acting.Page{}); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("denied = %v", err)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// flaky fails the named repository method, so each storage error path is proven to surface.
type flaky struct {
	*fakeRepo
	fail string
}

var errStore = errors.New("store down")

func (f flaky) hit(name string) error {
	if f.fail == name {
		return errStore
	}
	return nil
}
func (f flaky) Template(ctx context.Context, tx port.Tx, id string) (workorders.TemplateRef, error) {
	if err := f.hit("Template"); err != nil {
		return workorders.TemplateRef{}, err
	}
	return f.fakeRepo.Template(ctx, tx, id)
}
func (f flaky) ExistingTeams(ctx context.Context, tx port.Tx, ids []string) (map[string]bool, error) {
	if err := f.hit("ExistingTeams"); err != nil {
		return nil, err
	}
	return f.fakeRepo.ExistingTeams(ctx, tx, ids)
}
func (f flaky) ResolveOwners(ctx context.Context, tx port.Tx, h, r string) ([]string, error) {
	if err := f.hit("ResolveOwners"); err != nil {
		return nil, err
	}
	return f.fakeRepo.ResolveOwners(ctx, tx, h, r)
}
func (f flaky) OpenCVE(ctx context.Context, tx port.Tx, c string) (bool, error) {
	if err := f.hit("OpenCVE"); err != nil {
		return false, err
	}
	return f.fakeRepo.OpenCVE(ctx, tx, c)
}
func (f flaky) Insert(ctx context.Context, tx port.Tx, w domain.WorkOrder, r []domain.WorkOrderRepo) error {
	if err := f.hit("Insert"); err != nil {
		return err
	}
	return f.fakeRepo.Insert(ctx, tx, w, r)
}
func (f flaky) Update(ctx context.Context, tx port.Tx, w domain.WorkOrder, e domain.Version) (domain.WorkOrder, error) {
	if err := f.hit("Update"); err != nil {
		return w, err
	}
	return f.fakeRepo.Update(ctx, tx, w, e)
}
func (f flaky) Repos(ctx context.Context, tx port.Tx, id, a string, l int) ([]domain.WorkOrderRepo, error) {
	if err := f.hit("Repos"); err != nil {
		return nil, err
	}
	return f.fakeRepo.Repos(ctx, tx, id, a, l)
}
func (f flaky) OwnerTeams(ctx context.Context, tx port.Tx, id string) ([]string, error) {
	if err := f.hit("OwnerTeams"); err != nil {
		return nil, err
	}
	return f.fakeRepo.OwnerTeams(ctx, tx, id)
}
func (f flaky) Repo(ctx context.Context, tx port.Tx, id, h, r string, u bool) (domain.WorkOrderRepo, error) {
	if err := f.hit("Repo"); err != nil {
		return domain.WorkOrderRepo{}, err
	}
	return f.fakeRepo.Repo(ctx, tx, id, h, r, u)
}
func (f flaky) AddRepos(ctx context.Context, tx port.Tx, id string, r []domain.WorkOrderRepo) error {
	if err := f.hit("AddRepos"); err != nil {
		return err
	}
	return f.fakeRepo.AddRepos(ctx, tx, id, r)
}
func (f flaky) UpdateRepo(ctx context.Context, tx port.Tx, id string, n domain.WorkOrderRepo, e domain.Version) (domain.WorkOrderRepo, error) {
	if err := f.hit("UpdateRepo"); err != nil {
		return n, err
	}
	return f.fakeRepo.UpdateRepo(ctx, tx, id, n, e)
}

func TestStorageErrorsSurface(t *testing.T) {
	ops := map[string]func(s *workorders.Service, id string) error{
		"create": func(s *workorders.Service, _ string) error {
			_, err := s.Create(t.Context(), secOp, params(), []domain.RepoInput{{Repo: "acme/api"}, {Repo: "x/y", OwningTeamID: teamB}}, nil)
			return err
		},
		"get": func(s *workorders.Service, id string) error { _, err := s.Get(t.Context(), adminA, id); return err },
		"listRepos": func(s *workorders.Service, id string) error {
			_, _, err := s.ListRepos(t.Context(), secOp, id, acting.Page{})
			return err
		},
		"update": func(s *workorders.Service, id string) error {
			_, err := s.Update(t.Context(), secOp, id, 1, domain.WorkOrderPatch{}, []domain.RepoInput{{Repo: "acme/api"}})
			return err
		},
		"cancel": func(s *workorders.Service, id string) error {
			_, err := s.Cancel(t.Context(), secOp, id, "x", nil)
			return err
		},
		"report": func(s *workorders.Service, id string) error {
			_, err := s.UpdateRepo(t.Context(), workload, id, "github.com", "acme/api", 1, domain.RepoReport{Status: "in_progress"})
			return err
		},
	}
	methods := []string{"Template", "ExistingTeams", "ResolveOwners", "OpenCVE", "Insert", "Update", "Repos", "OwnerTeams", "Repo", "AddRepos", "UpdateRepo"}
	for _, m := range methods {
		for name, op := range ops {
			t.Run(m+"/"+name, func(t *testing.T) {
				s, r, _, _ := rig()
				w := create(t, s, domain.RepoInput{Repo: "acme/api"})
				s.Repo = flaky{fakeRepo: r, fail: m}
				if err := op(s, w.ID); err != nil && !errors.Is(err, errStore) && domain.KindOf(err) == "" {
					t.Errorf("%v", err)
				}
			})
		}
	}
}

// CLI-CD-1: a workload cannot read the repository version, so version 0 means the current one for a workload; a
// version it does send is compared, and no other caller may pass 0.
func TestUpdateRepoWorkloadMayOmitTheVersion(t *testing.T) {
	s, r, _, _ := rig()
	ctx := t.Context()
	w := create(t, s, domain.RepoInput{Repo: "acme/api"})
	x := r.wos[w.ID]
	x.TeamID, x.Status = teamA, domain.WorkOrderStatusAssigned
	r.wos[w.ID] = x
	step := domain.RepoReport{Status: "in_progress"}

	if got, err := s.UpdateRepo(ctx, workload, w.ID, "github.com", "acme/api", 0, step); err != nil || got.Version != 2 {
		t.Fatalf("workload without a version = %+v %v", got, err)
	}
	if _, err := s.UpdateRepo(ctx, workload, w.ID, "github.com", "acme/api", 1, step); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("a stale version from a workload = %v, want 412", err)
	}
	if _, err := s.UpdateRepo(ctx, adminA, w.ID, "github.com", "acme/api", 0, step); domain.KindOf(err) != domain.KindPreconditionFailed {
		t.Errorf("a team admin with version 0 = %v, want 412", err)
	}
	// The other team's workload is still not found, with or without a version.
	if _, err := s.UpdateRepo(ctx, otherWl, w.ID, "github.com", "acme/api", 0, step); domain.KindOf(err) != domain.KindNotFound {
		t.Errorf("other team workload = %v", err)
	}
}

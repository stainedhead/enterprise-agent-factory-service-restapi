package workorder_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/workorder"
)

func repo(name, status string) workorder.Repo {
	r := workorder.Repo{Host: "github.com", Repo: "acme/" + name, Status: status, OwningTeamID: teamA}
	if status == workorder.RepoPROpened || status == workorder.RepoMerged {
		r.PRURL = "https://github.com/acme/" + name + "/pull/7"
		r.PRNumber = 7
	}
	return r
}

func TestDeriveStatus(t *testing.T) {
	r := repo
	cases := []struct {
		name string
		w    workorder.WorkOrder
		want string
	}{
		{"open keeps", workorder.WorkOrder{Status: workorder.StatusOpen, Repos: []workorder.Repo{r("a", "merged")}}, workorder.StatusOpen},
		{"cancelled stays cancelled", workorder.WorkOrder{Status: workorder.StatusCancelled, TeamTornDown: true, Repos: []workorder.Repo{r("a", "pending")}}, workorder.StatusCancelled},
		{"done keeps", workorder.WorkOrder{Status: workorder.StatusDone, Repos: []workorder.Repo{r("a", "pending")}}, workorder.StatusDone},
		{"no repos keeps", workorder.WorkOrder{Status: workorder.StatusAssigned}, workorder.StatusAssigned},
		{"all pending", workorder.WorkOrder{Status: workorder.StatusAssigned, Repos: []workorder.Repo{r("a", "pending"), r("b", "pending")}}, workorder.StatusAssigned},
		{"one started", workorder.WorkOrder{Status: workorder.StatusAssigned, Repos: []workorder.Repo{r("a", "in_progress"), r("b", "pending")}}, workorder.StatusInProgress},
		{"remaining all have PRs", workorder.WorkOrder{Status: workorder.StatusInProgress, Repos: []workorder.Repo{r("a", "pr_opened"), r("b", "pr_opened"), r("c", "merged"), r("d", "not_affected")}}, workorder.StatusPROpened},
		{"one remaining lacks a PR", workorder.WorkOrder{Status: workorder.StatusPROpened, Repos: []workorder.Repo{r("a", "pr_opened"), r("b", "in_progress")}}, workorder.StatusInProgress},
		{"a failed repo remains", workorder.WorkOrder{Status: workorder.StatusPROpened, Repos: []workorder.Repo{r("a", "pr_opened"), r("b", "failed")}}, workorder.StatusInProgress},
		{"all merged, not affected or skipped", workorder.WorkOrder{Status: workorder.StatusPROpened, Repos: []workorder.Repo{r("a", "merged"), r("b", "not_affected"), r("c", "skipped")}}, workorder.StatusDone},
		{"torn down with repos remaining", workorder.WorkOrder{Status: workorder.StatusInProgress, TeamTornDown: true, Repos: []workorder.Repo{r("a", "merged"), r("b", "pr_opened")}}, workorder.StatusFailed},
		{"torn down with none remaining", workorder.WorkOrder{Status: workorder.StatusPROpened, TeamTornDown: true, Repos: []workorder.Repo{r("a", "merged")}}, workorder.StatusDone},
	}
	for _, tc := range cases {
		if got := workorder.DeriveStatus(tc.w); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestParsePRRef(t *testing.T) {
	ref, ok := workorder.ParsePRRef(workorder.Repo{Host: "ghe.example.com", Repo: "acme/api", PRNumber: 12})
	if !ok || ref.String() != "ghe.example.com/acme/api#12" {
		t.Errorf("by number: %v %v", ref, ok)
	}
	ref, ok = workorder.ParsePRRef(workorder.Repo{Host: "github.com", Repo: "acme/api", PRURL: "https://github.com/acme/api/pull/31"})
	if !ok || ref.Number != 31 {
		t.Errorf("by url: %v %v", ref, ok)
	}
	for _, r := range []workorder.Repo{{Host: "h", Repo: "noslash", PRNumber: 1}, {Host: "h", Repo: "a/b", PRURL: "https://h/a/b/issues/3"}, {Host: "h", Repo: "a/b"}} {
		if _, ok := workorder.ParsePRRef(r); ok {
			t.Errorf("%+v should not parse", r)
		}
	}
}

func newTracker(st *fakeTrackStore, gh *fakeGitHub, now *time.Time) *workorder.Tracker {
	return &workorder.Tracker{Store: st, GitHub: gh, Now: func() time.Time { return *now }}
}

func wo(id, status string, repos ...workorder.Repo) workorder.WorkOrder {
	return workorder.WorkOrder{ID: id, Status: status, TeamID: "team_wo", Title: "Patch", Severity: "high", Repos: repos}
}

func TestPollMovesMergedAndClosedPullRequests(t *testing.T) {
	merged := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	st := &fakeTrackStore{active: []workorder.WorkOrder{wo("wo_1", workorder.StatusPROpened, repo("a", "pr_opened"), repo("b", "pr_opened"), repo("c", "pr_opened"))}}
	gh := &fakeGitHub{states: map[string]workorder.PRState{
		"github.com/acme/a#7": {State: "closed", Merged: true, MergedAt: merged},
		"github.com/acme/b#7": {State: "closed"},
		"github.com/acme/c#7": {State: "open"},
	}}
	now := t0
	if err := newTracker(st, gh, &now).Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(st.repoChanges) != 2 {
		t.Fatalf("repo changes = %+v", st.repoChanges)
	}
	a, b := st.repoChanges[0], st.repoChanges[1]
	if a.To != workorder.RepoMerged || !a.MergedAt.Equal(merged) || a.FromStatus != workorder.RepoPROpened || a.FromPRURL == "" || a.TeamID != "team_wo" {
		t.Errorf("merged change = %+v", a)
	}
	if b.To != workorder.RepoInProgress || b.Detail == "" {
		t.Errorf("closed change = %+v", b)
	}
	// c is still open, b went back to in_progress: not all remaining repositories have a PR any more.
	if len(st.statusChange) != 1 || st.statusChange[0].To != workorder.StatusInProgress {
		t.Errorf("status changes = %+v", st.statusChange)
	}
}

func TestPollAllMergedCompletesTheWorkOrder(t *testing.T) {
	st := &fakeTrackStore{active: []workorder.WorkOrder{wo("wo_1", workorder.StatusPROpened, repo("a", "pr_opened"), repo("b", "not_affected"))}}
	gh := &fakeGitHub{states: map[string]workorder.PRState{"github.com/acme/a#7": {State: "closed", Merged: true}}}
	now := t0
	if err := newTracker(st, gh, &now).Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(st.statusChange) != 1 || st.statusChange[0].To != workorder.StatusDone || st.statusChange[0].From != workorder.StatusPROpened {
		t.Fatalf("status changes = %+v", st.statusChange)
	}
	if !st.repoChanges[0].MergedAt.Equal(t0) {
		t.Errorf("a merge GitHub gave no time for is stamped with the poll time: %v", st.repoChanges[0].MergedAt)
	}
}

func TestPollKeepsLastKnownStateWhenGitHubBacksOff(t *testing.T) {
	st := &fakeTrackStore{active: []workorder.WorkOrder{
		wo("wo_1", workorder.StatusPROpened, repo("a", "pr_opened"), repo("b", "pr_opened")),
		wo("wo_2", workorder.StatusPROpened, workorder.Repo{Host: "github.com", Repo: "other/x", Status: "pr_opened", PRURL: "https://github.com/other/x/pull/3", PRNumber: 3, OwningTeamID: teamA}),
	}}
	retry := t0.Add(20 * time.Minute)
	gh := &fakeGitHub{
		errs:   map[string]error{"github.com/acme/a#7": &backoffErr{at: retry, scope: workorder.Scope("github.com", "acme")}},
		states: map[string]workorder.PRState{"github.com/other/x#3": {State: "closed", Merged: true}, "github.com/acme/b#7": {State: "closed", Merged: true}},
	}
	now := t0
	tr := newTracker(st, gh, &now)
	if err := tr.Poll(t.Context()); err != nil {
		t.Fatalf("a rate limit is not a failure: %v", err)
	}
	if len(st.repoChanges) != 1 || st.repoChanges[0].Repo != "other/x" {
		t.Fatalf("changes = %+v: acme/a keeps its state, acme/b is skipped with its installation, other/x is read", st.repoChanges)
	}
	if len(gh.calls) != 2 {
		t.Errorf("calls = %v", gh.calls)
	}
	// Still blocked 10 minutes later: no calls for acme at all.
	now = t0.Add(10 * time.Minute)
	gh.calls = nil
	if err := tr.Poll(t.Context()); err != nil || len(gh.calls) != 0 {
		t.Fatalf("blocked poll: err = %v, calls = %v", err, gh.calls)
	}
	// After the reset time it reads again.
	now = retry.Add(time.Second)
	delete(gh.errs, "github.com/acme/a#7")
	gh.states["github.com/acme/a#7"] = workorder.PRState{State: "closed", Merged: true}
	if err := tr.Poll(t.Context()); err != nil || len(gh.calls) != 2 {
		t.Fatalf("after the reset: err = %v, calls = %v", err, gh.calls)
	}
}

func TestPollNoInstallationAndOtherErrors(t *testing.T) {
	st := &fakeTrackStore{active: []workorder.WorkOrder{wo("wo_1", workorder.StatusPROpened, repo("a", "pr_opened"), repo("b", "pr_opened"))}}
	gh := &fakeGitHub{errs: map[string]error{"github.com/acme/a#7": workorder.ErrNoInstallation, "github.com/acme/b#7": errors.New("boom")}}
	now := t0
	err := newTracker(st, gh, &now).Poll(t.Context())
	if err == nil || len(st.repoChanges) != 0 {
		t.Errorf("err = %v, changes = %v: an unknown error is reported, a missing installation is not", err, st.repoChanges)
	}
	// Without a GitHub client nothing is read.
	tr := &workorder.Tracker{Store: st, Now: clock}
	if err := tr.Poll(t.Context()); err != nil {
		t.Error(err)
	}
}

func TestPollLeavesAChangedRowAlone(t *testing.T) {
	st := &fakeTrackStore{stale: true, active: []workorder.WorkOrder{wo("wo_1", workorder.StatusPROpened, repo("a", "pr_opened"))}}
	gh := &fakeGitHub{states: map[string]workorder.PRState{"github.com/acme/a#7": {State: "closed", Merged: true}}}
	now := t0
	if err := newTracker(st, gh, &now).Poll(t.Context()); err != nil || len(st.statusChange) != 0 {
		t.Errorf("err = %v, status changes = %v: a report an agent made meanwhile wins", err, st.statusChange)
	}
}

func TestSettleFollowsAgentReportsAndTeardown(t *testing.T) {
	torn := wo("wo_3", workorder.StatusInProgress, repo("a", "merged"), repo("b", "pr_opened"))
	torn.TeamTornDown = true
	st := &fakeTrackStore{active: []workorder.WorkOrder{
		wo("wo_1", workorder.StatusAssigned, repo("a", "in_progress")),                    // -> in_progress
		wo("wo_2", workorder.StatusInProgress, repo("a", "merged"), repo("b", "skipped")), // -> done
		torn, // -> failed
		wo("wo_4", workorder.StatusAssigned, repo("a", "pending")), // unchanged
		wo("wo_5", workorder.StatusCancelled, repo("a", "pending")),
	}}
	now := t0
	if err := newTracker(st, &fakeGitHub{}, &now).Settle(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range st.statusChange {
		got[c.WorkOrderID] = c.To
	}
	want := map[string]string{"wo_1": "in_progress", "wo_2": "done", "wo_3": "failed"}
	if len(got) != len(want) || got["wo_1"] != want["wo_1"] || got["wo_2"] != want["wo_2"] || got["wo_3"] != want["wo_3"] {
		t.Errorf("changes = %v, want %v", got, want)
	}
	for _, c := range st.statusChange {
		if c.WorkOrderID == "wo_3" && c.Reason == "" {
			t.Error("a failed work order carries its reason")
		}
	}
	// Idempotent: a second pass changes nothing.
	n := len(st.statusChange)
	if err := newTracker(st, &fakeGitHub{}, &now).Settle(t.Context()); err != nil || len(st.statusChange) != n {
		t.Errorf("second pass wrote %d more changes (%v)", len(st.statusChange)-n, err)
	}
}

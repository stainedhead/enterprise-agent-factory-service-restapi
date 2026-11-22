package workorder_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/workorder"
)

const teamB = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"

func notifyWO() workorder.WorkOrder {
	return workorder.WorkOrder{
		ID: "wo_1", ExternalRef: "CVE-2026-12345", Title: "Patch libfoo", Severity: "high", Status: workorder.StatusPROpened, TeamID: "team_wo",
		SLADueAt: t0.Add(24 * time.Hour), SLAState: "on_track",
		Repos: []workorder.Repo{
			{Host: "github.com", Repo: "acme/api", Status: workorder.RepoPROpened, PRURL: "https://github.com/acme/api/pull/1", PRNumber: 1, OwningTeamID: teamA},
			{Host: "github.com", Repo: "acme/web", Status: workorder.RepoMerged, PRURL: "https://github.com/acme/web/pull/2", PRNumber: 2, OwningTeamID: teamB, OwningContact: "sec@example.com"},
			{Host: "github.com", Repo: "acme/old", Status: workorder.RepoPending, OwningContact: "old@example.com"},
		},
	}
}

func kinds(ns []workorder.Notification) map[string]int {
	m := map[string]int{}
	for _, n := range ns {
		m[n.Kind+"|"+n.Host+"/"+n.Repo+"|"+n.Recipient]++
	}
	return m
}

func TestCandidates(t *testing.T) {
	w := notifyWO()
	got := kinds(workorder.Candidates(w))
	want := []string{
		// assigned: every owner, once for the work order
		"assigned|/|" + teamA, "assigned|/|" + teamB, "assigned|/|sec@example.com", "assigned|/|old@example.com",
		// pr_opened for the open and the merged repository; merged for the merged one
		"pr_opened|github.com/acme/api|" + teamA, "pr_opened|github.com/acme/web|" + teamB, "pr_opened|github.com/acme/web|sec@example.com",
		"merged|github.com/acme/web|" + teamB, "merged|github.com/acme/web|sec@example.com",
	}
	if len(got) != len(want) {
		t.Errorf("got %v", got)
	}
	for _, k := range want {
		if got[k] != 1 {
			t.Errorf("missing %s in %v", k, got)
		}
	}

	// SLA warning: the owners of repositories not done yet, plus the work order's own event.
	w.SLAState = workorder.SLADueSoon
	got = kinds(workorder.Candidates(w))
	for _, k := range []string{"sla_warning|/|" + teamA, "sla_warning|/|old@example.com", "sla_warning|/|" + workorder.SentinelRecipient} {
		if got[k] != 1 {
			t.Errorf("missing %s in %v", k, got)
		}
	}
	if got["sla_warning|/|"+teamB] != 0 {
		t.Error("an owner whose repository is merged is not warned")
	}
	w.SLAState = workorder.SLABreached
	got = kinds(workorder.Candidates(w))
	if got["sla_breached|/|"+teamA] != 1 || got["sla_warning|/|"+teamA] != 0 {
		t.Errorf("breached: %v", got)
	}

	// A finished work order is not warned, and tells its owners it is complete.
	w.Status, w.SLAState = workorder.StatusDone, "breached"
	got = kinds(workorder.Candidates(w))
	if got["completed|/|"+teamA] != 1 || got["completed|/|sec@example.com"] != 1 || got["sla_breached|/|"+teamA] != 0 {
		t.Errorf("done: %v", got)
	}
	// Before the team exists nobody hears "assigned"; a cancelled work order sends nothing.
	w = notifyWO()
	w.Status, w.TeamID = workorder.StatusOpen, ""
	if got := kinds(workorder.Candidates(w)); got["assigned|/|"+teamA] != 0 {
		t.Errorf("open: %v", got)
	}
	w.Status = workorder.StatusCancelled
	if len(workorder.Candidates(w)) != 0 {
		t.Error("cancelled work orders notify nobody")
	}
}

func TestCandidateContent(t *testing.T) {
	for _, n := range workorder.Candidates(notifyWO()) {
		if n.Kind == workorder.KindMerged && n.Recipient == teamB {
			if !strings.Contains(n.Subject, "Pull request merged") || !strings.Contains(n.Subject, "acme/web") ||
				!strings.Contains(n.Body, "CVE-2026-12345") || !strings.Contains(n.Body, "https://github.com/acme/web/pull/2") {
				t.Errorf("subject %q body %q", n.Subject, n.Body)
			}
			if n.Data["workOrderId"] != "wo_1" || n.Data["recipient"] != teamB || n.Data["repo"] != "acme/web" || n.Data["prUrl"] == nil {
				t.Errorf("data = %v", n.Data)
			}
			return
		}
	}
	t.Fatal("no merged notification for team B")
}

func TestNotifierSendsOncePerEventAndRecipient(t *testing.T) {
	st := &fakeNotifyStore{channel: "nch_01JD2Q3R4S5T6V7W8X9Y0Z0001"}
	m := &fakeMailer{}
	n := &workorder.Notifier{Store: st, Mailer: m}
	w := notifyWO()
	w.SLAState = workorder.SLADueSoon
	for pass := 0; pass < 3; pass++ {
		if err := n.Notify(t.Context(), []workorder.WorkOrder{w}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[workorder.NotificationKey]bool{}
	for _, c := range append(append(append([]workorder.Notification{}, st.teams...), st.contacts...), st.events...) {
		if seen[c.NotificationKey] {
			t.Errorf("%+v recorded twice", c.NotificationKey)
		}
		seen[c.NotificationKey] = true
	}
	if len(st.events) != 1 || st.events[0].Kind != workorder.KindSLAWarning {
		t.Errorf("events = %+v: the work order's own sla_warning event is emitted once", st.events)
	}
	for _, c := range st.teams {
		if !strings.HasPrefix(c.Recipient, "team_") {
			t.Errorf("team notification to %q", c.Recipient)
		}
	}
	// 4 contact notifications for sec@: assigned, pr_opened, merged (+ none for completed); old@: assigned, sla_warning.
	if len(m.sent) != 5 || len(st.contacts) != 5 {
		t.Errorf("emails = %d, recorded = %d", len(m.sent), len(st.contacts))
	}
	for _, e := range m.sent {
		if e.channel != "nch_01JD2Q3R4S5T6V7W8X9Y0Z0001" || !strings.Contains(e.to, "@") || e.subject == "" {
			t.Errorf("email = %+v", e)
		}
	}
	// The work order finishes: one more notification per owner, once.
	w.Status = workorder.StatusDone
	for pass := 0; pass < 2; pass++ {
		if err := n.Notify(t.Context(), []workorder.WorkOrder{w}); err != nil {
			t.Fatal(err)
		}
	}
	done := 0
	for _, c := range append(append([]workorder.Notification{}, st.teams...), st.contacts...) {
		if c.Kind == workorder.KindCompleted {
			done++
		}
	}
	if done != 4 {
		t.Errorf("completed notifications = %d, want one per owner (4)", done)
	}
}

func TestNotifierSendsNothingToContactsWhenNoChannelIsSet(t *testing.T) {
	st := &fakeNotifyStore{}
	m := &fakeMailer{}
	if err := (&workorder.Notifier{Store: st, Mailer: m}).Notify(t.Context(), []workorder.WorkOrder{notifyWO()}); err != nil {
		t.Fatal(err)
	}
	if len(m.sent) != 0 || len(st.contacts) != 0 {
		t.Errorf("sent %d emails, recorded %d: nothing goes to contacts without a service channel", len(m.sent), len(st.contacts))
	}
	if len(st.teams) == 0 {
		t.Error("teams are notified regardless")
	}
	// Once the channel is set, the contacts hear of what they missed.
	st.channel = "nch_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	if err := (&workorder.Notifier{Store: st, Mailer: m}).Notify(t.Context(), []workorder.WorkOrder{notifyWO()}); err != nil || len(m.sent) == 0 {
		t.Errorf("err = %v, emails = %d", err, len(m.sent))
	}
}

func TestNotifierFailedEmailIsNotRecordedAndIsRetried(t *testing.T) {
	st := &fakeNotifyStore{channel: "nch_1"}
	m := &fakeMailer{err: errors.New("ses: throttled")}
	n := &workorder.Notifier{Store: st, Mailer: m}
	err := n.Notify(t.Context(), []workorder.WorkOrder{notifyWO()})
	if err == nil || len(st.contacts) != 0 {
		t.Fatalf("err = %v, recorded = %d", err, len(st.contacts))
	}
	if strings.Contains(err.Error(), "sec@example.com") || strings.Contains(err.Error(), "old@example.com") {
		t.Errorf("an address must not appear in an error: %v", err)
	}
	m.err = nil
	if err := n.Notify(t.Context(), []workorder.WorkOrder{notifyWO()}); err != nil || len(st.contacts) == 0 {
		t.Errorf("retry: err = %v, recorded = %d", err, len(st.contacts))
	}
}

func TestNotifierRunReadsActiveWorkOrders(t *testing.T) {
	st := &fakeNotifyStore{active: []workorder.WorkOrder{notifyWO()}}
	n := &workorder.Notifier{Source: st, Store: st}
	if err := n.Run(t.Context()); err != nil || len(st.teams) == 0 {
		t.Errorf("err = %v, teams = %d", err, len(st.teams))
	}
}

type recordedJobs struct{ names []string }

func (r *recordedJobs) Every(name string, _ time.Duration, _ func(ctx context.Context) error) {
	r.names = append(r.names, name)
}

func TestRegister(t *testing.T) {
	r := &recordedJobs{}
	workorder.Register(r, workorder.Jobs{Intake: &workorder.Intake{}, Assigner: &workorder.Assigner{}, Tracker: &workorder.Tracker{}, Notifier: &workorder.Notifier{}})
	if strings.Join(r.names, ",") != "work-order-intake,work-order-assign,work-order-poll,work-order-settle,work-order-notify" {
		t.Errorf("jobs = %v", r.names)
	}
	r = &recordedJobs{}
	workorder.Register(r, workorder.Jobs{Tracker: &workorder.Tracker{}})
	if len(r.names) != 2 {
		t.Errorf("jobs = %v", r.names)
	}
}

package workorder_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/workorder"
)

const (
	tpl   = "swt_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
)

func event(id string, mutate func(d map[string]any)) string {
	d := map[string]any{
		"cveId": "CVE-2026-12345", "severity": "high", "title": "Patch libfoo", "slaDueAt": "2026-12-01T00:00:00Z",
		"templateId": tpl, "budgetUsd": "25.50",
		"repos": []any{map[string]any{"host": "github.com", "repo": "acme/api"}, map[string]any{"repo": "acme/web", "owningTeamId": teamA, "owningContact": "sec@example.com"}},
	}
	if mutate != nil {
		mutate(d)
	}
	b, _ := json.Marshal(map[string]any{"version": "0", "id": id, "detail-type": workorder.DetailType, "source": "example.scanner", "detail": d})
	return string(b)
}

func TestParseEvent(t *testing.T) {
	n, err := workorder.ParseEvent(event("evt-1", nil))
	if err != nil {
		t.Fatal(err)
	}
	if n.SourceEventID != "evt-1" || n.Kind != "cve_remediation" || n.CVEID != "CVE-2026-12345" || n.Budget.String() != "25.50" || n.TemplateID != tpl {
		t.Errorf("parsed = %+v", n)
	}
	if len(n.Repos) != 2 || n.Repos[0].Host != "github.com" || n.Repos[0].OwningTeamID != "" || n.Repos[1].OwningTeamID != teamA || n.Repos[1].OwningContact != "sec@example.com" {
		t.Errorf("repos = %+v", n.Repos)
	}
	if n.SLADueAt.IsZero() {
		t.Error("slaDueAt not parsed")
	}

	// detail.sourceEventId wins over the envelope id.
	n, err = workorder.ParseEvent(event("evt-1", func(d map[string]any) { d["sourceEventId"] = "scanner-77" }))
	if err != nil || n.SourceEventID != "scanner-77" {
		t.Errorf("source event id = %q, %v", n.SourceEventID, err)
	}

	bad := []struct {
		name   string
		body   string
		expect string
	}{
		{"not json", "{", "not an EventBridge event"},
		{"wrong detail type", strings.Replace(event("e", nil), workorder.DetailType, "other", 1), "detail-type"},
		{"no cve", event("e", func(d map[string]any) { delete(d, "cveId") }), "cveId"},
		{"bad cve", event("e", func(d map[string]any) { d["cveId"] = "CVE-1" }), "cveId"},
		{"bad severity", event("e", func(d map[string]any) { d["severity"] = "urgent" }), "severity"},
		{"no title", event("e", func(d map[string]any) { d["title"] = " " }), "title"},
		{"bad sla", event("e", func(d map[string]any) { d["slaDueAt"] = "tomorrow" }), "slaDueAt"},
		{"bad template", event("e", func(d map[string]any) { d["templateId"] = "tpl_1" }), "templateId"},
		{"bad budget", event("e", func(d map[string]any) { d["budgetUsd"] = "-1" }), "budgetUsd"},
		{"no repos", event("e", func(d map[string]any) { d["repos"] = []any{} }), "repos"},
		{"bad repo", event("e", func(d map[string]any) { d["repos"] = []any{map[string]any{"repo": "no-slash"}} }), "repos[0].repo"},
		{"bad owner team", event("e", func(d map[string]any) {
			d["repos"] = []any{map[string]any{"repo": "a/b", "owningTeamId": "slug"}}
		}), "owningTeamId"},
		{"bad contact", event("e", func(d map[string]any) {
			d["repos"] = []any{map[string]any{"repo": "a/b", "owningContact": "nobody"}}
		}), "owningContact"},
		{"repeated repo", event("e", func(d map[string]any) {
			d["repos"] = []any{map[string]any{"repo": "a/b"}, map[string]any{"repo": "A/b"}}
		}), "repeats"},
		{"no id", event("", nil), "no usable id"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			_, err := workorder.ParseEvent(tc.body)
			var rej *workorder.Rejection
			if !errors.As(err, &rej) || rej.Reason != "invalid" {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(strings.Join(rej.Detail["problems"].([]string), ";"), tc.expect) {
				t.Errorf("problems = %v, want %q", rej.Detail["problems"], tc.expect)
			}
		})
	}
}

func TestIntakeOneWorkOrderPerEventEvenWhenDeliveredTwice(t *testing.T) {
	q := &fakeQueue{msgs: []workorder.QueueMessage{
		{ID: "m1", Body: event("evt-1", nil)},
		{ID: "m2", Body: event("evt-1", nil)}, // the same event, delivered again
		{ID: "m3", Body: event("evt-2", func(d map[string]any) { d["cveId"] = "CVE-2026-99999" })},
	}}
	st := &fakeIntakeStore{}
	j := &workorder.Intake{Queue: q, Store: st, Now: clock, Batch: 2}
	if err := j.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(st.created) != 2 {
		t.Fatalf("created %d work orders, want 2 (%+v)", len(st.created), st.created)
	}
	if strings.Join(q.deleted, ",") != "m1,m2,m3" {
		t.Errorf("deleted = %v: every handled message, duplicates included, is deleted", q.deleted)
	}
}

func TestIntakeRefusalsAreAuditedAndDeleted(t *testing.T) {
	for _, reason := range []string{"unowned_repos", "ambiguous_repos", "open_work_order", "template_unavailable", "unknown_owner"} {
		t.Run(reason, func(t *testing.T) {
			q := &fakeQueue{msgs: []workorder.QueueMessage{{ID: "m1", Body: event("evt-1", nil)}}}
			st := &fakeIntakeStore{reject: &workorder.Rejection{Reason: reason}}
			j := &workorder.Intake{Queue: q, Store: st, Now: clock}
			if err := j.Run(t.Context()); err != nil {
				t.Fatal(err)
			}
			if st.rejected["evt-1"] == nil || st.rejected["evt-1"].Reason != reason || len(q.deleted) != 1 {
				t.Errorf("rejected = %v, deleted = %v", st.rejected, q.deleted)
			}
		})
	}
	// A malformed message is refused under its queue message id.
	q := &fakeQueue{msgs: []workorder.QueueMessage{{ID: "m9", Body: "garbage"}}}
	st := &fakeIntakeStore{}
	if err := (&workorder.Intake{Queue: q, Store: st, Now: clock}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st.rejected["m9"] == nil || len(st.created) != 0 || len(q.deleted) != 1 {
		t.Errorf("rejected = %v, created = %v, deleted = %v", st.rejected, st.created, q.deleted)
	}
}

func TestIntakeTransientFailureLeavesTheMessage(t *testing.T) {
	q := &fakeQueue{msgs: []workorder.QueueMessage{{ID: "m1", Body: event("evt-1", nil)}}}
	st := &fakeIntakeStore{err: errors.New("database unavailable")}
	err := (&workorder.Intake{Queue: q, Store: st, Now: clock}).Run(t.Context())
	if err == nil || len(q.deleted) != 0 {
		t.Errorf("err = %v, deleted = %v: the message must stay for a retry", err, q.deleted)
	}
	// A receive failure is reported.
	q = &fakeQueue{recvErr: errors.New("throttled")}
	if err := (&workorder.Intake{Queue: q, Store: &fakeIntakeStore{}, Now: clock}).Run(t.Context()); err == nil {
		t.Error("receive error not reported")
	}
	// A delete failure is reported, and the work order exists once (the retry is a duplicate).
	q = &fakeQueue{msgs: []workorder.QueueMessage{{ID: "m1", Body: event("evt-1", nil)}}, delErr: errors.New("gone")}
	st = &fakeIntakeStore{}
	if err := (&workorder.Intake{Queue: q, Store: st, Now: clock}).Run(t.Context()); err == nil {
		t.Error("delete error not reported")
	}
	q.delErr = nil
	q.msgs = []workorder.QueueMessage{{ID: "m1", Body: event("evt-1", nil)}}
	if err := (&workorder.Intake{Queue: q, Store: st, Now: clock}).Run(t.Context()); err != nil || len(st.created) != 1 {
		t.Errorf("retry: err = %v, created = %d", err, len(st.created))
	}
}

func TestIntakeBoundsOnePass(t *testing.T) {
	q := &fakeQueue{}
	for i := 0; i < 30; i++ {
		q.msgs = append(q.msgs, workorder.QueueMessage{ID: "m" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Body: event("evt-"+string(rune('a'+i%26))+string(rune('a'+i/26)), nil)})
	}
	st := &fakeIntakeStore{}
	if err := (&workorder.Intake{Queue: q, Store: st, Now: clock, Batch: 10, MaxMessages: 20}).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(st.created) != 20 || len(q.msgs) != 10 {
		t.Errorf("created %d, left %d; one pass handles at most MaxMessages", len(st.created), len(q.msgs))
	}
}

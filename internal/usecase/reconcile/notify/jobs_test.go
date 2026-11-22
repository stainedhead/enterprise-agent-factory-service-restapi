package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

type fakeQueue struct {
	msgs    []notify.QueueMessage
	deleted []string
	err     error
}

func (q *fakeQueue) Receive(context.Context, int) ([]notify.QueueMessage, error) {
	if q.err != nil {
		return nil, q.err
	}
	out := q.msgs
	q.msgs = nil
	return out, nil
}

func (q *fakeQueue) Delete(_ context.Context, m notify.QueueMessage) error {
	q.deleted = append(q.deleted, m.ID)
	return nil
}

type fakeMailer struct {
	sent [][]string
	err  error
}

func (m *fakeMailer) Send(_ context.Context, to []string, subject, _ string) error {
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, append([]string{subject}, to...))
	return nil
}

type fakeHTTP struct {
	got []string
	err error
}

func (h *fakeHTTP) Send(_ context.Context, ch notify.Channel, cred notify.Credential, m notify.Message) error {
	h.got = append(h.got, ch.ID+"|"+cred.Reveal()+"|"+m.EventID)
	return h.err
}

type fakeBusTester struct {
	arn string
	err error
}

func (b *fakeBusTester) PutTest(_ context.Context, arn string, _ notify.Message) error {
	b.arn = arn
	return b.err
}

type fakeReqs struct {
	reqs []notify.TestRequest
	err  error
}

func (r *fakeReqs) PendingTests(context.Context, time.Time) ([]notify.TestRequest, error) {
	return r.reqs, r.err
}

func emailChan() notify.Channel {
	return notify.Channel{ID: chID, TeamID: teamA, Name: "e", Kind: domain.ChannelEmail, Config: map[string]any{"to": []any{"ops@example.com", "dev@example.com"}, "subjectPrefix": "[pay]"},
		Enabled: true, Version: 1, TeamState: "active"}
}

func emailMsg(id, eventID string, receives string) notify.QueueMessage {
	b, _ := json.Marshal(map[string]any{"channelId": chID, "eventId": eventID, "detailType": "budget.threshold_crossed", "detail": map[string]any{"teamId": teamA}})
	return notify.QueueMessage{ID: id, Receipt: id, Body: string(b), Attrs: map[string]string{"ApproximateReceiveCount": receives}}
}

func TestEmailSentOncePerEventID(t *testing.T) {
	src := &fakeSource{chans: map[string]notify.Channel{chID: emailChan()}}
	q, m, rec, ev := &fakeQueue{}, &fakeMailer{}, &fakeRecorder{}, &fakeEvents{}
	d := &notify.EmailDrainer{Queue: q, Source: src, Mailer: m, Recorder: rec, Events: ev, Now: func() time.Time { return now }, Log: log}
	// The same event arrives twice (RC-US-13.2) plus another event.
	q.msgs = []notify.QueueMessage{emailMsg("m1", "evt_1", "1"), emailMsg("m2", "evt_1", "1"), emailMsg("m3", "evt_2", "1")}
	if err := d.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(m.sent) != 2 || m.sent[0][0] != "[pay] budget.threshold_crossed" || len(m.sent[0]) != 3 {
		t.Fatalf("sent %v", m.sent)
	}
	if len(q.deleted) != 3 || len(rec.got) != 2 || !rec.got[0].OK {
		t.Fatalf("deleted=%v recorded=%v", q.deleted, rec.got)
	}
	// Redelivery after the fact sends nothing.
	q.msgs = []notify.QueueMessage{emailMsg("m4", "evt_1", "2")}
	_ = d.Run(t.Context())
	if len(m.sent) != 2 {
		t.Fatalf("duplicate sent: %v", m.sent)
	}
}

func TestEmailDrainerEdgeCases(t *testing.T) {
	disabled := emailChan()
	disabled.Enabled = false
	slack := slackChan()
	tests := []struct {
		name    string
		chans   map[string]notify.Channel
		msg     notify.QueueMessage
		mailErr error
		sent    int
		deleted bool
		events  int
	}{
		{"malformed body dropped", map[string]notify.Channel{chID: emailChan()}, notify.QueueMessage{ID: "x", Body: "nope"}, nil, 0, true, 0},
		{"unknown channel dropped", map[string]notify.Channel{}, emailMsg("x", "e1", "1"), nil, 0, true, 0},
		{"disabled channel dropped", map[string]notify.Channel{chID: disabled}, emailMsg("x", "e2", "1"), nil, 0, true, 0},
		{"non email channel dropped", map[string]notify.Channel{chID: slack}, emailMsg("x", "e3", "1"), nil, 0, true, 0},
		{"send failure stays queued", map[string]notify.Channel{chID: emailChan()}, emailMsg("x", "e4", "1"), errors.New("MessageRejected https://ses.example/x"), 0, false, 0},
		{"third failure raises the event", map[string]notify.Channel{chID: emailChan()}, emailMsg("x", "e5", "3"), errors.New("MessageRejected"), 0, false, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, m, rec, ev := &fakeQueue{msgs: []notify.QueueMessage{tc.msg}}, &fakeMailer{err: tc.mailErr}, &fakeRecorder{}, &fakeEvents{}
			d := &notify.EmailDrainer{Queue: q, Source: &fakeSource{chans: tc.chans}, Mailer: m, Recorder: rec, Events: ev, Now: func() time.Time { return now }, Log: log}
			if err := d.Run(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(m.sent) != tc.sent || (len(q.deleted) == 1) != tc.deleted || len(ev.got) != tc.events {
				t.Fatalf("sent=%d deleted=%v events=%v", len(m.sent), q.deleted, ev.got)
			}
			for _, e := range ev.got {
				if strings.Contains(e, "http") {
					t.Fatalf("url leaked: %s", e)
				}
			}
		})
	}
	// Queue errors surface; a load error keeps the message.
	if err := (&notify.EmailDrainer{Queue: &fakeQueue{err: errors.New("x")}, Now: func() time.Time { return now }, Log: log}).Run(t.Context()); err == nil {
		t.Fatal("want error")
	}
}

func TestFailureDrainer(t *testing.T) {
	src := &fakeSource{chans: map[string]notify.Channel{chID: slackChan()}}
	q, rec, ev := &fakeQueue{}, &fakeRecorder{}, &fakeEvents{}
	d := &notify.FailureDrainer{Queue: q, Source: src, Recorder: rec, Events: ev, Now: func() time.Time { return now }, Log: log}
	q.msgs = []notify.QueueMessage{
		{ID: "a", Body: `{"detail-type":"budget.threshold_crossed"}`, Attrs: map[string]string{
			"RULE_ARN": "arn:aws:events:us-east-1:111122223333:rule/afs-bus/" + notify.ObjectName(chID), "ERROR_CODE": "SDK_CLIENT_ERROR", "ERROR_MESSAGE": "POST https://hooks.example.com/T1/B2/X returned 404"}},
		{ID: "b", Attrs: map[string]string{"RULE_ARN": "arn:aws:events:us-east-1:111122223333:rule/other-rule"}},
		{ID: "c", Attrs: map[string]string{"RULE_ARN": "arn:aws:events:us-east-1:111122223333:rule/" + notify.ObjectName("nch_01JD2Q3R4S5T6V7W8X9Y0Z0009")}},
	}
	if err := d.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rec.got) != 1 || rec.got[0].OK || strings.Contains(rec.got[0].Err, "hooks.example.com") || !strings.Contains(rec.got[0].Err, "SDK_CLIENT_ERROR") {
		t.Fatalf("%+v", rec.got)
	}
	if len(ev.got) != 1 || !strings.HasSuffix(ev.got[0], "|budget.threshold_crossed") || len(q.deleted) != 3 {
		t.Fatalf("events=%v deleted=%v", ev.got, q.deleted)
	}
	// A recorder failure keeps the message for the next pass.
	rec.err = errors.New("db down")
	q.msgs = []notify.QueueMessage{{ID: "d", Attrs: map[string]string{"RULE_ARN": "rule/" + notify.ObjectName(chID)}}}
	q.deleted = nil
	_ = d.Run(t.Context())
	if len(q.deleted) != 0 {
		t.Fatalf("deleted %v", q.deleted)
	}
	if err := (&notify.FailureDrainer{Queue: &fakeQueue{err: errors.New("x")}, Log: log}).Run(t.Context()); err == nil {
		t.Fatal("want error")
	}
}

func TestTestRunner(t *testing.T) {
	ch := slackChan()
	src := &fakeSource{chans: map[string]notify.Channel{chID: ch, "nch_gone": {}}}
	delete(src.chans, "nch_gone")
	h, rec, ev := &fakeHTTP{}, &fakeRecorder{}, &fakeEvents{}
	reqs := &fakeReqs{reqs: []notify.TestRequest{{ChannelID: chID, TestID: "ntest_1", At: now}, {ChannelID: "nch_gone", TestID: "ntest_2"}}}
	r := &notify.TestRunner{Requests: reqs, Source: src, Secrets: &fakeSecrets{val: "https://hooks.example.com/x"}, Deliverer: notify.Deliverer{HTTP: h},
		Recorder: rec, Events: ev, Now: func() time.Time { return now }, Log: log}
	if err := r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(h.got) != 1 || len(rec.got) != 1 || !rec.got[0].OK || rec.got[0].TestID != "ntest_1" || len(ev.got) != 0 {
		t.Fatalf("%v %+v", h.got, rec.got)
	}
	// The same request again is not sent twice.
	_ = r.Run(t.Context())
	if len(h.got) != 1 {
		t.Fatal("test sent twice")
	}
	// A failing Slack is returned and recorded on the channel (RA-US-17.3) with the delivery_failed event.
	h.err = errors.New("slack returned 404 for https://hooks.example.com/x")
	reqs.reqs = []notify.TestRequest{{ChannelID: chID, TestID: "ntest_3"}}
	if err := r.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	last := rec.got[len(rec.got)-1]
	if last.OK || last.TestID != "ntest_3" || len(ev.got) != 1 || strings.Contains(ev.got[0], "hooks.example.com") {
		t.Fatalf("%+v %v", last, ev.got)
	}
	// Already recorded (restart): skipped.
	ch.Prior = notify.ApplyDelivery(nil, notify.Delivery{At: now, OK: true, TestID: "ntest_4"})
	src.chans[chID] = ch
	reqs.reqs = []notify.TestRequest{{ChannelID: chID, TestID: "ntest_4"}}
	n := len(rec.got)
	_ = r.Run(t.Context())
	if len(rec.got) != n {
		t.Fatal("recorded twice")
	}
	// A secret that cannot be read fails the test without leaking the cause.
	r2 := &notify.TestRunner{Requests: &fakeReqs{reqs: []notify.TestRequest{{ChannelID: chID, TestID: "ntest_5"}}}, Source: &fakeSource{chans: map[string]notify.Channel{chID: slackChan()}},
		Secrets: &fakeSecrets{err: errors.New("arn:aws:secretsmanager:secret:x denied")}, Deliverer: notify.Deliverer{HTTP: h}, Recorder: rec, Now: func() time.Time { return now }, Log: log}
	if err := r2.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := rec.got[len(rec.got)-1]; got.OK || strings.Contains(got.Err, "arn:") {
		t.Fatalf("%+v", got)
	}
	// Request listing and recording failures surface.
	if err := (&notify.TestRunner{Requests: &fakeReqs{err: errors.New("x")}, Now: func() time.Time { return now }}).Run(t.Context()); err == nil {
		t.Fatal("want error")
	}
	rec.err = errors.New("db")
	r3 := &notify.TestRunner{Requests: &fakeReqs{reqs: []notify.TestRequest{{ChannelID: chID, TestID: "ntest_6"}}}, Source: src, Secrets: &fakeSecrets{}, Deliverer: notify.Deliverer{HTTP: &fakeHTTP{}},
		Recorder: rec, Now: func() time.Time { return now }, Log: log}
	src.chans[chID] = slackChan()
	if err := r3.Run(t.Context()); err == nil {
		t.Fatal("want record error")
	}
}

func TestDeliverer(t *testing.T) {
	m, b, h := &fakeMailer{}, &fakeBusTester{}, &fakeHTTP{}
	d := notify.Deliverer{HTTP: h, Mail: m, Bus: b}
	msg := notify.TestMessage(emailChan(), "ntest_1", now)
	if msg.EventID != "ntest_1" || !strings.Contains(msg.Text, "ntest_1") {
		t.Fatalf("%+v", msg)
	}
	if err := d.Deliver(t.Context(), emailChan(), "", msg); err != nil || len(m.sent) != 1 {
		t.Fatal(err, m.sent)
	}
	eb := notify.Channel{Kind: domain.ChannelEventBridge, Config: map[string]any{"eventBusArn": "arn:aws:events:us-east-1:111122223333:event-bus/x"}}
	if err := d.Deliver(t.Context(), eb, "", msg); err != nil || b.arn != "arn:aws:events:us-east-1:111122223333:event-bus/x" {
		t.Fatal(err, b.arn)
	}
	for _, k := range []domain.ChannelKind{domain.ChannelSlack, domain.ChannelEmail, domain.ChannelEventBridge, "sms"} {
		if err := (notify.Deliverer{}).Deliver(t.Context(), notify.Channel{Kind: k}, "", msg); err == nil {
			t.Fatalf("%s: want error", k)
		}
	}
}

func TestSweeper(t *testing.T) {
	other := "nch_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	dead := "nch_01JD2Q3R4S5T6V7W8X9Y0Z0003"
	ch := slackChan()
	del := slackChan()
	del.ID, del.TeamState = other, "deleting"
	bus := &fakeBus{rules: map[string]notify.RuleSpec{chID: {}, other: {}, dead: {}}}
	cl := &fakeCleaner{}
	s := &notify.Sweeper{Bus: bus, Source: &fakeSource{chans: map[string]notify.Channel{chID: ch, other: del}}, Cleaner: cl, Log: log}
	if err := s.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, kept := bus.rules[chID]; !kept {
		t.Fatal("live channel's rule removed")
	}
	if len(bus.rules) != 1 || len(cl.deleted) != 1 || cl.deleted[0] != dead {
		t.Fatalf("rules=%v cleaned=%v", bus.rules, cl.deleted)
	}
	// Errors surface and keep going.
	if err := (&notify.Sweeper{Bus: &fakeBus{err: errors.New("x")}, Log: log}).Run(t.Context()); err == nil {
		t.Fatal("want error")
	}
	s2 := &notify.Sweeper{Bus: &fakeBus{rules: map[string]notify.RuleSpec{chID: {}}}, Source: &fakeSource{err: errors.New("db")}, Log: log}
	if err := s2.Run(t.Context()); err == nil {
		t.Fatal("want error")
	}
}

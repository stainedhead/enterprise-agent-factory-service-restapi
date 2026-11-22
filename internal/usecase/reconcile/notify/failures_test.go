package notify_test

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

type fakeFailures struct {
	mu  sync.Mutex
	got []string
}

func (f *fakeFailures) NotificationFailed(channelType string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, channelType)
}

func (f *fakeFailures) types() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func want(t *testing.T, f *fakeFailures, types ...string) {
	t.Helper()
	if got := f.types(); !slices.Equal(got, types) && (len(got) != 0 || len(types) != 0) {
		t.Fatalf("failures = %v, want %v", got, types)
	}
}

// FR-R06 AC1: each failed send increments afs_notification_failures_total{channel_type}; a success does not.
func TestEmailDrainerCountsFailedSends(t *testing.T) {
	fl := &fakeFailures{}
	mk := func(mailErr error) *notify.EmailDrainer {
		return &notify.EmailDrainer{Queue: &fakeQueue{}, Source: &fakeSource{chans: map[string]notify.Channel{chID: emailChan()}},
			Mailer: &fakeMailer{err: mailErr}, Recorder: &fakeRecorder{}, Events: &fakeEvents{}, Failures: fl,
			Now: func() time.Time { return now }, Log: log}
	}
	ok := mk(nil)
	ok.Queue.(*fakeQueue).msgs = []notify.QueueMessage{emailMsg("m1", "e1", "1")}
	_ = ok.Run(t.Context())
	want(t, fl)
	bad := mk(errors.New("MessageRejected"))
	bad.Queue.(*fakeQueue).msgs = []notify.QueueMessage{emailMsg("m2", "e2", "1")}
	_ = bad.Run(t.Context())
	want(t, fl, string(domain.ChannelEmail))
}

func TestFailureDrainerCountsDeadLetters(t *testing.T) {
	fl := &fakeFailures{}
	q := &fakeQueue{msgs: []notify.QueueMessage{
		{ID: "a", Attrs: map[string]string{"RULE_ARN": "arn:aws:events:us-east-1:111122223333:rule/afs-bus/" + notify.ObjectName(chID), "ERROR_CODE": "X"}},
		{ID: "b", Attrs: map[string]string{"RULE_ARN": "arn:aws:events:us-east-1:111122223333:rule/other-rule"}}, // not ours
	}}
	d := &notify.FailureDrainer{Queue: q, Source: &fakeSource{chans: map[string]notify.Channel{chID: slackChan()}},
		Recorder: &fakeRecorder{}, Events: &fakeEvents{}, Failures: fl, Now: func() time.Time { return now }, Log: log}
	if err := d.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	want(t, fl, string(domain.ChannelSlack))
}

func TestTestRunnerCountsFailedTests(t *testing.T) {
	fl := &fakeFailures{}
	h := &fakeHTTP{}
	r := &notify.TestRunner{Requests: &fakeReqs{reqs: []notify.TestRequest{{ChannelID: chID, TestID: "ntest_1"}}},
		Source: &fakeSource{chans: map[string]notify.Channel{chID: slackChan()}}, Secrets: &fakeSecrets{val: "https://hooks.example.com/x"},
		Deliverer: notify.Deliverer{HTTP: h}, Recorder: &fakeRecorder{}, Events: &fakeEvents{}, Failures: fl,
		Now: func() time.Time { return now }, Log: log}
	_ = r.Run(t.Context())
	want(t, fl)
	h.err = errors.New("404")
	r.Requests = &fakeReqs{reqs: []notify.TestRequest{{ChannelID: chID, TestID: "ntest_2"}}}
	_ = r.Run(t.Context())
	want(t, fl, string(domain.ChannelSlack))
}

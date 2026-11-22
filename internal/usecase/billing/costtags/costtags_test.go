package costtags_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/costtags"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/billing/costtags/costtagstest"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/port/fakes"
)

var day = costtagstest.Day

func setup(b *costtagstest.Billing, st *costtagstest.Store, now time.Time) (*costtags.Checker, *fakes.Outbox, *fakes.Clock) {
	ob := &fakes.Outbox{}
	clk := &fakes.Clock{T: now}
	return &costtags.Checker{Billing: b, Store: st, Tx: &fakes.TxRunner{}, Outbox: ob, Clock: clk, IDs: &fakes.IDGen{},
		Session: port.SystemSession("afs-billing")}, ob, clk
}

func TestRecordsWhatTheCURShowsAndAWSStatus(t *testing.T) {
	st := &costtagstest.Store{
		KeyList: []string{"afs-team-id", "afs-agent-id", "cost-centre", "bad\x01key", "afs-team-id"},
		CUR:     map[string]costtags.Seen{"afs-team-id": {First: day(2026, 11, 3), Last: day(2026, 11, 20)}},
	}
	b := &costtagstest.Billing{Tags: map[string]costtags.Activation{
		"afs-team-id":  {Active: true, Since: day(2026, 11, 1)},
		"afs-agent-id": {Active: false, Since: day(2026, 11, 1)},
	}}
	c, ob, _ := setup(b, st, day(2026, 11, 22))
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Keys != 3 || res.Seen != 1 || res.Active != 1 || len(res.NotSeen) != 0 || len(ob.Events) != 0 {
		t.Fatalf("result %+v events %d", res, len(ob.Events))
	}
	for k, want := range map[string]string{"afs-team-id": "active", "afs-agent-id": "inactive", "cost-centre": "unknown"} {
		if got := st.Rows[k].AWSStatus; got != want {
			t.Errorf("%s status = %q, want %q", k, got, want)
		}
	}
	if r := st.Rows["afs-team-id"]; r.First == nil || !r.First.Equal(day(2026, 11, 3)) || !r.Last.Equal(day(2026, 11, 20)) {
		t.Errorf("team seen range: %+v", r)
	}
	if _, ok := st.Rows["bad\x01key"]; ok {
		t.Error("a key the table cannot store must be skipped")
	}
}

func TestNotSeenRaisedOncePerActivation(t *testing.T) {
	since := day(2026, 11, 10)
	st := &costtagstest.Store{KeyList: []string{"afs-agent-id", "afs-team-id"}}
	b := &costtagstest.Billing{Tags: map[string]costtags.Activation{
		"afs-agent-id": {Active: true, Since: since}, "afs-team-id": {Active: true, Since: since},
	}}
	st.CUR = map[string]costtags.Seen{"afs-team-id": {First: day(2026, 11, 11), Last: day(2026, 11, 12)}}
	c, ob, clk := setup(b, st, since.Add(2*24*time.Hour)) // active 2 days: not yet
	steps := []struct {
		name   string
		at     time.Time
		events int
	}{
		{"two days active raises nothing", since.Add(2 * 24 * time.Hour), 0},
		{"three days active raises one", since.Add(3 * 24 * time.Hour), 1},
		{"rerun the same day raises none", since.Add(3*24*time.Hour + time.Hour), 1},
		{"next day raises none", since.Add(4 * 24 * time.Hour), 1},
	}
	for _, s := range steps {
		clk.T = s.at
		if _, err := c.Run(context.Background()); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if len(ob.Events) != s.events {
			t.Fatalf("%s: %d events, want %d", s.name, len(ob.Events), s.events)
		}
	}
	ev := ob.Events[0]
	if ev.Type != domain.EventCostTagsNotSeen || ev.AggregateID != "afs-agent-id" {
		t.Errorf("event %+v", ev)
	}

	// Deactivated and activated again, still not seen: a new period, a new event after three days.
	b.Tags["afs-agent-id"] = costtags.Activation{Active: false, Since: since.Add(5 * 24 * time.Hour)}
	clk.T = since.Add(5 * 24 * time.Hour)
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.Tags["afs-agent-id"] = costtags.Activation{Active: true, Since: since.Add(6 * 24 * time.Hour)}
	for i, off := range []int{6, 8, 9, 10} {
		clk.T = since.Add(time.Duration(off) * 24 * time.Hour)
		if _, err := c.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := 1
		if i >= 2 {
			want = 2
		}
		if len(ob.Events) != want {
			t.Fatalf("reactivation day +%d: %d events, want %d", off, len(ob.Events), want)
		}
	}
}

func TestBillingFailureKeepsStatusAndFailsTheRun(t *testing.T) {
	boom := errors.New("ce unavailable")
	st := &costtagstest.Store{KeyList: []string{"afs-team-id"}, CUR: map[string]costtags.Seen{"afs-team-id": {First: day(2026, 11, 3), Last: day(2026, 11, 4)}},
		Rows: map[string]costtags.Stored{"afs-team-id": {AWSStatus: "active", CheckedAt: day(2026, 11, 20)}}}
	c, ob, _ := setup(&costtagstest.Billing{Err: boom}, st, day(2026, 11, 22))
	res, err := c.Run(context.Background())
	if !errors.Is(err, boom) || !errors.Is(res.BillingErr, boom) {
		t.Fatalf("err %v res %+v", err, res)
	}
	if r := st.Rows["afs-team-id"]; r.AWSStatus != "active" || r.First == nil {
		t.Errorf("the CUR side is still recorded and the AWS status kept: %+v", r)
	}
	if len(ob.Events) != 0 {
		t.Error("no event without a billing answer")
	}
}

func TestSaveFailureIsReturned(t *testing.T) {
	boom := errors.New("db down")
	st := &costtagstest.Store{KeyList: []string{"afs-team-id"}, FailSave: boom}
	c, _, _ := setup(&costtagstest.Billing{Tags: map[string]costtags.Activation{}}, st, day(2026, 11, 22))
	if _, err := c.Run(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

type failIDs struct{ err error }

func (f failIDs) NewID(domain.IDPrefix) (domain.ID, error) {
	var id domain.ID
	return id, f.err
}

type failOutbox struct{ err error }

func (f failOutbox) Publish(context.Context, port.Tx, domain.Event) error { return f.err }

func TestPortFailuresAreReturned(t *testing.T) {
	boom := errors.New("boom")
	overdueStore := func() *costtagstest.Store { return &costtagstest.Store{KeyList: []string{"afs-agent-id"}} }
	overdueBilling := &costtagstest.Billing{Tags: map[string]costtags.Activation{"afs-agent-id": {Active: true, Since: day(2026, 11, 1)}}}
	for _, tc := range []struct {
		name  string
		store *costtagstest.Store
		mod   func(c *costtags.Checker)
	}{
		{"keys", &costtagstest.Store{FailKeys: boom}, nil},
		{"cur", &costtagstest.Store{FailSeen: boom}, nil},
		{"stored", &costtagstest.Store{FailStored: boom}, nil},
		{"ids", overdueStore(), func(c *costtags.Checker) { c.IDs = failIDs{boom} }},
		{"outbox", overdueStore(), func(c *costtags.Checker) { c.Outbox = failOutbox{boom} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _ := setup(overdueBilling, tc.store, day(2026, 11, 22))
			if tc.mod != nil {
				tc.mod(c)
			}
			if _, err := c.Run(context.Background()); !errors.Is(err, boom) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestLogsWhenALoggerIsGiven(t *testing.T) {
	st := &costtagstest.Store{KeyList: []string{"afs-team-id", "bad\x01key"}}
	c, _, _ := setup(&costtagstest.Billing{Tags: map[string]costtags.Activation{}}, st, day(2026, 11, 22))
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

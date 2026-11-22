package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

const (
	teamA = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	teamB = "team_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	chID  = "nch_01JD2Q3R4S5T6V7W8X9Y0Z0001"
)

var (
	now = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	log = slog.New(slog.NewTextHandler(io.Discard, nil))
)

type fakeBus struct {
	mu      sync.Mutex
	rules   map[string]notify.RuleSpec
	removed []string
	err     error
}

func (b *fakeBus) Apply(_ context.Context, s notify.RuleSpec) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	if b.rules == nil {
		b.rules = map[string]notify.RuleSpec{}
	}
	b.rules[s.Channel.ID] = s
	return []string{"rule/" + s.Channel.ID}, nil
}

func (b *fakeBus) Remove(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	delete(b.rules, id)
	b.removed = append(b.removed, id)
	return nil
}

func (b *fakeBus) List(context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for id := range b.rules {
		out = append(out, id)
	}
	return out, b.err
}

type fakeSource struct {
	chans map[string]notify.Channel
	err   error
}

func (s *fakeSource) LoadChannel(_ context.Context, id string) (notify.Channel, error) {
	if s.err != nil {
		return notify.Channel{}, s.err
	}
	c, ok := s.chans[id]
	if !ok {
		return notify.Channel{}, domain.NotFound("notification channel")
	}
	return c, nil
}

type fakeSecrets struct {
	val notify.Credential
	err error
	n   int
}

func (s *fakeSecrets) Read(context.Context, string) (notify.Credential, error) {
	s.n++
	return s.val, s.err
}

type fakeCleaner struct{ deleted []string }

func (c *fakeCleaner) DeleteStatus(_ context.Context, id string) error {
	c.deleted = append(c.deleted, id)
	return nil
}

type fakeRecorder struct {
	got []notify.Delivery
	chs []string
	err error
}

func (r *fakeRecorder) Record(_ context.Context, ch notify.Channel, d notify.Delivery) error {
	r.got = append(r.got, d)
	r.chs = append(r.chs, ch.ID)
	return r.err
}

type fakeEvents struct{ got []string }

func (e *fakeEvents) DeliveryFailed(_ context.Context, ch notify.Channel, text, typ string) error {
	e.got = append(e.got, ch.ID+"|"+text+"|"+typ)
	return nil
}

func slackChan() notify.Channel {
	//nolint:gosec // a fixture ARN, not a credential
	return notify.Channel{ID: chID, TeamID: teamA, Name: "s", Kind: domain.ChannelSlack, Config: map[string]any{"channel": "#alerts"},
		SecretARN: "arn:aws:secretsmanager:us-east-1:111122223333:secret:afs/x", EventTypes: []string{"budget.*"}, Enabled: true, Version: 3, TeamState: "active"}
}

func newController(t *testing.T, src *fakeSource, bus *fakeBus, sec *fakeSecrets, cl *fakeCleaner) *notify.Controller {
	t.Helper()
	c, err := notify.NewController(notify.Config{Source: src, Bus: bus, Secrets: sec, Cleaner: cl, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func condOf(o engine.Outcome, typ string) (domain.Condition, bool) {
	for _, c := range o.Conditions {
		if c.Type == typ {
			return c, true
		}
	}
	return domain.Condition{}, false
}

func TestNewControllerRequiresPorts(t *testing.T) {
	if _, err := notify.NewController(notify.Config{}); err == nil {
		t.Fatal("want error")
	}
	c := newController(t, &fakeSource{}, &fakeBus{}, &fakeSecrets{}, nil)
	if c.Kind() != notify.Kind || string(notify.Kind) != "notification_channel" {
		t.Fatal(c.Kind())
	}
	rt := notify.Route()
	if rt.Table != "notification_channels" || len(rt.FanOut) != 1 {
		t.Fatalf("%+v", rt)
	}
}

func TestReconcileAppliesRule(t *testing.T) {
	src := &fakeSource{chans: map[string]notify.Channel{chID: slackChan()}}
	bus, sec := &fakeBus{}, &fakeSecrets{val: "https://hooks.example.com/T000/B000/XXXX"}
	c := newController(t, src, bus, sec, nil)
	out, err := c.Reconcile(t.Context(), engine.NewRef(notify.Kind, chID, teamA))
	if err != nil || out.Generation != 3 || len(out.Changed) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	spec := bus.rules[chID]
	if spec.Credential.Reveal() == "" || spec.Pattern == "" {
		t.Fatalf("%+v", spec)
	}
	if strings.Contains(spec.Credential.String(), "hooks") || strings.Contains(slog.AnyValue(spec.Credential).String(), "hooks") {
		t.Fatal("credential must print redacted")
	}
	if h, _ := condOf(out, domain.CondDeliveryHealthy); h.Status != domain.ConditionTrue || h.Reason != domain.DeliveryRuleApplied {
		t.Fatalf("%+v", h)
	}
	// An email channel reads no secret.
	e := notify.Channel{ID: chID, TeamID: teamA, Name: "e", Kind: domain.ChannelEmail, Config: map[string]any{"to": []any{"ops@example.com"}}, Enabled: true, Version: 1, TeamState: "active"}
	src.chans[chID] = e
	sec.n = 0
	if _, err := c.Reconcile(t.Context(), engine.NewRef(notify.Kind, chID, teamA)); err != nil || sec.n != 0 {
		t.Fatalf("%v reads=%d", err, sec.n)
	}
}

func TestReconcileCarriesDeliveryHistory(t *testing.T) {
	ch := slackChan()
	ch.Prior = notify.ApplyDelivery(nil, notify.Delivery{At: now.Add(-time.Hour), Err: "boom https://hooks.example.com/secret"})
	ch.ObservedGeneration = 3
	src := &fakeSource{chans: map[string]notify.Channel{chID: ch}}
	c := newController(t, src, &fakeBus{}, &fakeSecrets{}, nil)
	out, err := c.Reconcile(t.Context(), engine.NewRef(notify.Kind, chID, teamA))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := condOf(out, domain.CondDeliveryHealthy)
	if h.Status != domain.ConditionFalse || strings.Contains(h.Message, "hooks.example.com") {
		t.Fatalf("history kept, url redacted: %+v", h)
	}
	if _, ok := condOf(out, domain.CondDeliveryDetail); !ok {
		t.Fatal("detail dropped")
	}
	// A config change (new generation) resets the health.
	ch.ObservedGeneration = 2
	src.chans[chID] = ch
	out, _ = c.Reconcile(t.Context(), engine.NewRef(notify.Kind, chID, teamA))
	if h, _ := condOf(out, domain.CondDeliveryHealthy); h.Status != domain.ConditionTrue {
		t.Fatalf("reset: %+v", h)
	}
}

func TestReconcileRemovalCases(t *testing.T) {
	disabled := slackChan()
	disabled.Enabled = false
	svcNoTypes := notify.Channel{ID: chID, Name: "svc", Kind: domain.ChannelEmail, Config: map[string]any{"to": []any{"a@example.com"}}, Enabled: true, Version: 1, TeamState: "active"}
	deleting := slackChan()
	deleting.TeamState = "deleting"
	tests := []struct {
		name   string
		ch     *notify.Channel
		health domain.ConditionStatus
		reason string
		noStat bool
		clean  bool
	}{
		{"disabled", &disabled, domain.ConditionUnknown, domain.DeliveryDisabled, false, false},
		{"service without types", &svcNoTypes, domain.ConditionUnknown, domain.DeliveryNoEventTypes, false, false},
		{"team deleting", &deleting, "", "", true, false},
		{"row gone", nil, "", "", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{chans: map[string]notify.Channel{}}
			if tc.ch != nil {
				src.chans[chID] = *tc.ch
			}
			bus, cl := &fakeBus{rules: map[string]notify.RuleSpec{chID: {}}}, &fakeCleaner{}
			c := newController(t, src, bus, &fakeSecrets{}, cl)
			out, err := c.Reconcile(t.Context(), engine.NewRef(notify.Kind, chID, teamA))
			if err != nil {
				t.Fatal(err)
			}
			if _, still := bus.rules[chID]; still || len(bus.removed) != 1 {
				t.Fatalf("rule not removed: %+v", bus.rules)
			}
			if out.NoStatus != tc.noStat || (len(cl.deleted) == 1) != tc.clean {
				t.Fatalf("noStatus=%v cleaned=%v", out.NoStatus, cl.deleted)
			}
			if tc.reason != "" {
				if h, _ := condOf(out, domain.CondDeliveryHealthy); h.Status != tc.health || h.Reason != tc.reason {
					t.Fatalf("%+v", h)
				}
			}
		})
	}
}

func TestReconcileFailures(t *testing.T) {
	boom := errors.New("AccessDeniedException")
	t.Run("bus failure is retried", func(t *testing.T) {
		src := &fakeSource{chans: map[string]notify.Channel{chID: slackChan()}}
		c := newController(t, src, &fakeBus{err: boom}, &fakeSecrets{}, nil)
		if _, err := c.Reconcile(t.Context(), engine.NewRef(notify.Kind, chID, teamA)); err == nil || errors.Is(err, engine.ErrTerminal) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("secret read failure is retried", func(t *testing.T) {
		src := &fakeSource{chans: map[string]notify.Channel{chID: slackChan()}}
		c := newController(t, src, &fakeBus{}, &fakeSecrets{err: boom}, nil)
		if _, err := c.Reconcile(t.Context(), engine.NewRef(notify.Kind, chID, teamA)); err == nil || errors.Is(err, engine.ErrTerminal) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("invalid channel is terminal and never reaches the bus", func(t *testing.T) {
		bad := slackChan()
		bad.Config = map[string]any{"channel": "x y"}
		bus := &fakeBus{}
		c := newController(t, &fakeSource{chans: map[string]notify.Channel{chID: bad}}, bus, &fakeSecrets{}, nil)
		out, err := c.Reconcile(t.Context(), engine.NewRef(notify.Kind, chID, teamA))
		if err == nil || !errors.Is(err, engine.ErrTerminal) || len(bus.rules) != 0 {
			t.Fatalf("%v %v", err, bus.rules)
		}
		if h, _ := condOf(out, domain.CondDeliveryHealthy); h.Status != domain.ConditionFalse {
			t.Fatalf("%+v", h)
		}
	})
	t.Run("load failure", func(t *testing.T) {
		c := newController(t, &fakeSource{err: boom}, &fakeBus{}, &fakeSecrets{}, nil)
		if _, err := c.Reconcile(t.Context(), engine.NewRef(notify.Kind, chID, teamA)); !errors.Is(err, boom) {
			t.Fatal(err)
		}
	})
	t.Run("remove failure on delete", func(t *testing.T) {
		c := newController(t, &fakeSource{chans: map[string]notify.Channel{}}, &fakeBus{err: boom}, &fakeSecrets{}, nil)
		if _, err := c.Reconcile(t.Context(), engine.NewRef(notify.Kind, chID, teamA)); err == nil {
			t.Fatal("want error")
		}
	})
}

func match(t *testing.T, pattern string, detailType, team, channel string) bool {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal([]byte(pattern), &p); err != nil {
		t.Fatal(err)
	}
	for _, clause := range p["$or"].([]any) {
		c := clause.(map[string]any)
		ok := false
		for _, dt := range c["detail-type"].([]any) {
			switch v := dt.(type) {
			case string:
				ok = ok || v == detailType
			case map[string]any:
				ok = ok || strings.HasPrefix(detailType, v["prefix"].(string))
			}
		}
		d, _ := c["detail"].(map[string]any)
		if ts, has := d["teamId"]; has && ts.([]any)[0] != team {
			ok = false
		}
		if dd, has := d["data"]; has && dd.(map[string]any)["channelId"].([]any)[0] != channel {
			ok = false
		}
		if ok {
			return true
		}
	}
	return false
}

func TestEventPattern(t *testing.T) {
	ch := slackChan()
	pat, err := notify.EventPattern(ch)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		dt, team, channel string
		want              bool
	}{
		{"budget.threshold_crossed", teamA, "", true},
		{"budget.threshold_crossed", teamB, "", false}, // never another team's events (RC-US-13.4)
		{"agent.status_changed", teamA, "", false},     // not subscribed
		{"report.delivered", teamA, chID, true},        // addressed to the channel (OQ-S2)
		{"report.delivered", teamA, "nch_other", false},
		{"report.delivered", teamB, chID, false},
	}
	for _, tc := range tests {
		if got := match(t, pat, tc.dt, tc.team, tc.channel); got != tc.want {
			t.Errorf("%s team=%s channel=%s: got %v want %v", tc.dt, tc.team, tc.channel, got, tc.want)
		}
	}
	// No event types: every family but notification.* and report.*.
	ch.EventTypes = nil
	pat, _ = notify.EventPattern(ch)
	for dt, want := range map[string]bool{"agent.status_changed": true, "team.created": true, "notification.delivery_failed": false, "report.failed": false} {
		if got := match(t, pat, dt, teamA, ""); got != want {
			t.Errorf("default %s: got %v", dt, got)
		}
	}
	// A service channel carries no team filter.
	svc := slackChan()
	svc.TeamID, svc.EventTypes = "", []string{"cost.variance_detected"}
	pat, _ = notify.EventPattern(svc)
	if !match(t, pat, "cost.variance_detected", teamB, "") {
		t.Fatal("service channel matches every team")
	}
}

func TestObjectNames(t *testing.T) {
	n := notify.ObjectName(chID)
	if id, ok := notify.ChannelIDOf(n); !ok || id != chID {
		t.Fatal(n, id)
	}
	for _, bad := range []string{"other", "afs-nch_", "afs-team_x"} {
		if _, ok := notify.ChannelIDOf(bad); ok {
			t.Fatal(bad)
		}
	}
	if got := notify.RedactURLs("failed https://hooks.example.com/x?t=1 status 404"); got != "failed <url> status 404" {
		t.Fatal(got)
	}
	if got := notify.RedactURLs(strings.Repeat("a", 900)); len(got) != 500 {
		t.Fatal(len(got))
	}
}

func TestApplyDelivery(t *testing.T) {
	ok := notify.ApplyDelivery(nil, notify.Delivery{At: now, OK: true})
	d := domain.DeliveryDetailOf(ok)
	if ok[0].Status != domain.ConditionTrue || d.LastDeliveredAt == nil || d.LastFailureAt != nil {
		t.Fatalf("%+v %+v", ok, d)
	}
	fail := notify.ApplyDelivery(ok, notify.Delivery{At: now.Add(time.Minute), Err: "x"})
	d = domain.DeliveryDetailOf(fail)
	if fail[0].Status != domain.ConditionFalse || fail[0].Reason != domain.DeliveryFailed || d.LastDeliveredAt == nil || d.LastError != "x" {
		t.Fatalf("history kept: %+v", d)
	}
	tf := notify.ApplyDelivery(nil, notify.Delivery{At: now, Err: "nope", TestID: "ntest_1"})
	d = domain.DeliveryDetailOf(tf)
	if tf[0].Reason != domain.DeliveryTestFailed || d.LastTestID != "ntest_1" || d.TestDelivered == nil || *d.TestDelivered || d.TestError != "nope" {
		t.Fatalf("%+v", d)
	}
	tg := notify.ApplyDelivery(tf, notify.Delivery{At: now, OK: true, TestID: "ntest_2"})
	d = domain.DeliveryDetailOf(tg)
	if tg[0].Status != domain.ConditionTrue || d.TestError != "" || !*d.TestDelivered {
		t.Fatalf("%+v", d)
	}
}

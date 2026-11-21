package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// ---- fakes ----

type fakeFeed struct {
	mu        sync.Mutex
	h         FeedHandler
	ready     chan struct{}
	connects  int
	failFirst bool
}

func newFakeFeed() *fakeFeed { return &fakeFeed{ready: make(chan struct{})} }

func (f *fakeFeed) Listen(ctx context.Context, h FeedHandler) error {
	f.mu.Lock()
	f.connects++
	n := f.connects
	f.h = h
	fail := f.failFirst && n == 1
	f.mu.Unlock()
	if fail {
		return errors.New("connection refused")
	}
	h.Connected()
	if n == 1 || f.failFirst {
		close(f.ready)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeFeed) send(c Change) {
	<-f.ready
	f.mu.Lock()
	h := f.h
	f.mu.Unlock()
	h.Change(c)
}

type fakeLister struct {
	mu     sync.Mutex
	all    []Ref
	stale  []Ref
	listed [][]Kind
	err    error
}

func (l *fakeLister) List(_ context.Context, kinds []Kind) ([]Ref, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.listed = append(l.listed, kinds)
	return append([]Ref(nil), l.all...), l.err
}

func (l *fakeLister) Stale(_ context.Context, _ []Kind) ([]Ref, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.stale
	l.stale = nil
	return s, l.err
}

func (l *fakeLister) setStale(r ...Ref) {
	l.mu.Lock()
	l.stale = r
	l.mu.Unlock()
}

type fakeStatus struct {
	mu      sync.Mutex
	reports []Report
	err     error
}

func (s *fakeStatus) Apply(_ context.Context, r Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, r)
	return s.err
}

func (s *fakeStatus) all() []Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Report(nil), s.reports...)
}

type fakeDrift struct {
	mu   sync.Mutex
	recs []DriftRecord
}

func (d *fakeDrift) Record(_ context.Context, r DriftRecord) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.recs = append(d.recs, r)
	return nil
}

type fakeController struct {
	kind Kind
	fn   func(ctx context.Context, ref Ref, call int) (Outcome, error)
	mu   sync.Mutex
	n    map[Key]int
	seen []Ref
}

func (c *fakeController) Kind() Kind { return c.kind }
func (c *fakeController) Reconcile(ctx context.Context, ref Ref) (Outcome, error) {
	c.mu.Lock()
	if c.n == nil {
		c.n = map[Key]int{}
	}
	c.n[ref.Key]++
	call := c.n[ref.Key]
	c.seen = append(c.seen, ref)
	c.mu.Unlock()
	if c.fn == nil {
		return Outcome{Generation: 1}, nil
	}
	return c.fn(ctx, ref, call)
}

func (c *fakeController) calls(k Key) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n[k]
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type rig struct {
	e      *Engine
	feed   *fakeFeed
	lister *fakeLister
	status *fakeStatus
	drift  *fakeDrift
	ctl    *fakeController
	cancel context.CancelFunc
	done   chan struct{}
}

var teamRef = NewRef(KindTeam, "team_A", "team_A")

func newRig(t *testing.T, mut func(*Config, *rig)) *rig {
	t.Helper()
	reg, err := NewRegistry(
		Route{Table: "teams", Targets: []Target{{Kind: KindTeam, Self: true}}},
		Route{Table: "team_models", Targets: []Target{{Kind: KindTeam}}},
		Route{Table: "models", FanOut: []Kind{KindTeam}},
	)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{feed: newFakeFeed(), lister: &fakeLister{}, status: &fakeStatus{}, drift: &fakeDrift{}, ctl: &fakeController{kind: KindTeam}, done: make(chan struct{})}
	cfg := Config{
		Feed: r.feed, Lister: r.lister, Registry: reg, Controllers: []Controller{r.ctl}, Status: r.status, Drift: r.drift,
		Workers: 2, PollInterval: time.Hour, ResyncInterval: time.Hour,
		Backoff: Backoff{Base: 5 * time.Millisecond, Max: 40 * time.Millisecond},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if mut != nil {
		mut(&cfg, r)
	}
	e, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.e = e
	return r
}

func (r *rig) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() { defer close(r.done); _ = r.e.Run(ctx) }()
	t.Cleanup(r.stop)
}

func (r *rig) stop() {
	r.cancel()
	<-r.done
}

// ---- tests ----

func TestParseChange(t *testing.T) {
	tests := []struct {
		in      string
		want    Change
		wantErr bool
	}{
		{"teams:team_A:team_A", Change{"teams", "team_A", "team_A"}, false},
		{"agent_templates::tpl_X", Change{"agent_templates", "", "tpl_X"}, false},
		{"team_models:team_A:", Change{"team_models", "team_A", ""}, false},
		{"teams:team_A", Change{}, true},
		{":a:b", Change{}, true},
		{"", Change{}, true},
	}
	for _, tt := range tests {
		got, err := ParseChange(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseChange(%q) = %v, %v", tt.in, got, err)
		}
	}
}

func TestRegistry(t *testing.T) {
	if _, err := NewRegistry(Route{}); err == nil {
		t.Error("route without table accepted")
	}
	if _, err := NewRegistry(Route{Table: "a"}, Route{Table: "a"}); err == nil {
		t.Error("duplicate table accepted")
	}
	reg, _ := NewRegistry(
		Route{Table: "teams", Targets: []Target{{Kind: KindTeam, Self: true}}},
		Route{Table: "team_models", Targets: []Target{{Kind: KindTeam}, {Kind: KindAgent, Self: true}}},
		Route{Table: "models", FanOut: []Kind{KindTeam, KindAgent}},
		Route{Table: "other"},
	)
	fan := func(_ context.Context, kinds []Kind) ([]Ref, error) {
		var out []Ref
		for _, k := range kinds {
			out = append(out, NewRef(k, "x_"+string(k), "team_Z"))
		}
		return out, nil
	}
	tests := []struct {
		name string
		c    Change
		want []Ref
	}{
		{"self", Change{"teams", "team_A", "team_A"}, []Ref{NewRef(KindTeam, "team_A", "team_A")}},
		{"team part", Change{"team_models", "team_A", ""}, []Ref{NewRef(KindTeam, "team_A", "team_A")}},
		{"team and self", Change{"team_models", "team_A", "agt_1"}, []Ref{NewRef(KindTeam, "team_A", "team_A"), NewRef(KindAgent, "agt_1", "team_A")}},
		{"service level fans out", Change{"models", "", "m1"}, []Ref{NewRef(KindTeam, "x_team", "team_Z"), NewRef(KindAgent, "x_agent", "team_Z")}},
		{"service level without fan-out", Change{"teams", "", "x"}, nil},
		{"unrouted table", Change{"nothing", "team_A", "x"}, nil},
		{"no targets", Change{"other", "team_A", "x"}, nil},
	}
	for _, tt := range tests {
		got, err := reg.Resolve(t.Context(), tt.c, fan)
		if err != nil || fmt.Sprint(got) != fmt.Sprint(tt.want) {
			t.Errorf("%s: Resolve = %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
	kinds := reg.Kinds()
	if len(kinds) != 2 {
		t.Errorf("Kinds = %v", kinds)
	}
}

func TestBackoffDelay(t *testing.T) {
	b := DefaultBackoff
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second, 256 * time.Second, 300 * time.Second, 300 * time.Second}
	for i, w := range want {
		if got := b.Delay(i + 1); got != w {
			t.Errorf("Delay(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := b.Delay(0); got != time.Second {
		t.Errorf("Delay(0) = %v", got)
	}
	if got := b.Delay(10000); got != 5*time.Minute {
		t.Errorf("Delay(10000) = %v: overflow not capped", got)
	}
}

func TestErrors(t *testing.T) {
	base := errors.New("boom")
	if got := WithCondition(domain.CondIamReady, nil); got != nil {
		t.Error("nil not preserved")
	}
	if got := Terminal(nil); got != nil {
		t.Error("nil terminal not preserved")
	}
	tests := []struct {
		name         string
		err          error
		code, typ    string
		wantTerminal bool
	}{
		{"plain", base, "", "Ready", false},
		{"coded", &CodedError{Code: "AccessDenied", Err: base}, "AccessDenied", "Ready", false},
		{"condition with code", &ConditionError{Type: domain.CondIamReady, Code: "Throttling", Err: base}, "Throttling", domain.CondIamReady, false},
		{"condition derives code", WithCondition(domain.CondNamespaceReady, &CodedError{Code: "Forbidden", Err: base}), "Forbidden", domain.CondNamespaceReady, false},
		{"terminal", Terminal(WithCondition(domain.CondIamReady, base)), "", domain.CondIamReady, true},
	}
	for _, tt := range tests {
		typ, reason := reasonOf(tt.err)
		wantReason := tt.code
		if wantReason == "" {
			wantReason = "ReconcileError"
		}
		if typ != tt.typ || reason != wantReason || errors.Is(tt.err, ErrTerminal) != tt.wantTerminal {
			t.Errorf("%s: reasonOf = %q, %q (terminal %v)", tt.name, typ, reason, errors.Is(tt.err, ErrTerminal))
		}
		if CodeOf(tt.err) != tt.code {
			t.Errorf("%s: CodeOf = %q, want %q", tt.name, CodeOf(tt.err), tt.code)
		}
	}
	ce := &ConditionError{Type: "X", Err: base}
	if ce.Error() != "X: boom" || (&ConditionError{Err: base}).Error() != "boom" || (&CodedError{Err: base}).Error() != "boom" {
		t.Error("error text")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("empty config accepted")
	}
	reg, _ := NewRegistry()
	c := &fakeController{kind: KindTeam}
	_, err := New(Config{Feed: newFakeFeed(), Lister: &fakeLister{}, Registry: reg, Status: &fakeStatus{}, Controllers: []Controller{c, c}})
	if err == nil {
		t.Error("duplicate controllers accepted")
	}
	e, err := New(Config{Feed: newFakeFeed(), Lister: &fakeLister{}, Registry: reg, Status: &fakeStatus{}})
	if err != nil || e.cfg.Workers != 4 || e.cfg.PollInterval != 30*time.Second || e.cfg.ResyncInterval != 10*time.Minute || e.cfg.Backoff != DefaultBackoff {
		t.Errorf("defaults: %v %+v", err, e.cfg)
	}
}

func TestChangeIsReconciledAndStatusWritten(t *testing.T) {
	r := newRig(t, nil)
	r.ctl.fn = func(_ context.Context, _ Ref, _ int) (Outcome, error) {
		return Outcome{Generation: 3, Conditions: []domain.Condition{{Type: domain.CondNamespaceReady, Status: domain.ConditionTrue}}}, nil
	}
	r.start(t)
	r.feed.send(Change{"teams", "team_A", "team_A"})
	eventually(t, "status", func() bool { return len(r.status.all()) > 0 })
	rep := r.status.all()[0]
	if rep.Ref != teamRef || rep.Phase != domain.PhaseReady || rep.Generation == nil || *rep.Generation != 3 || !rep.Replace || len(rep.Conditions) != 1 {
		t.Errorf("report = %+v", rep)
	}
	if rep.ReconciledAt.IsZero() {
		t.Error("ReconciledAt not set")
	}
	st := r.e.Stats()
	if st.Reconciled < 1 || st.LastResync.IsZero() {
		t.Errorf("stats = %+v", st)
	}
}

func TestTeamPartChangeReconcilesTeam(t *testing.T) {
	r := newRig(t, nil)
	r.start(t)
	r.feed.send(Change{"team_models", "team_A", ""})
	eventually(t, "team reconcile", func() bool { return r.ctl.calls(teamRef.Key) >= 1 })
}

func TestServiceLevelChangeFansOut(t *testing.T) {
	r := newRig(t, func(_ *Config, r *rig) {
		r.lister.all = []Ref{NewRef(KindTeam, "team_A", "team_A"), NewRef(KindTeam, "team_B", "team_B")}
	})
	r.start(t)
	r.feed.send(Change{"models", "", "anthropic.x"})
	eventually(t, "both teams", func() bool {
		return r.ctl.calls(Key{KindTeam, "team_A"}) >= 1 && r.ctl.calls(Key{KindTeam, "team_B"}) >= 1
	})
}

func TestStartupResyncAndSingletons(t *testing.T) {
	adm := &fakeController{kind: KindAdmission, fn: func(context.Context, Ref, int) (Outcome, error) { return Outcome{NoStatus: true}, nil }}
	r := newRig(t, func(c *Config, r *rig) {
		r.lister.all = []Ref{teamRef}
		c.Controllers = append(c.Controllers, adm)
		c.Singletons = []Ref{NewRef(KindAdmission, "afs-admission", ""), NewRef(KindPool, "ignored", "")}
	})
	r.start(t)
	eventually(t, "resync", func() bool {
		return r.ctl.calls(teamRef.Key) >= 1 && adm.calls(Key{KindAdmission, "afs-admission"}) >= 1
	})
	// NoStatus writes nothing for the singleton; the team has one report.
	eventually(t, "team status", func() bool { return len(r.status.all()) >= 1 })
	for _, rep := range r.status.all() {
		if rep.Kind == KindAdmission {
			t.Errorf("status written for a NoStatus outcome: %+v", rep)
		}
	}
}

func TestPollPicksUpStaleWithoutNotifications(t *testing.T) {
	r := newRig(t, func(c *Config, _ *rig) { c.PollInterval = 15 * time.Millisecond })
	r.start(t)
	eventually(t, "initial poll", func() bool { return !r.e.Stats().LastPoll.IsZero() })
	r.lister.setStale(NewRef(KindTeam, "team_S", "team_S"))
	eventually(t, "stale reconciled by the poll", func() bool { return r.ctl.calls(Key{KindTeam, "team_S"}) >= 1 })
}

func TestReconnectTriggersPoll(t *testing.T) {
	r := newRig(t, func(c *Config, r *rig) {
		c.PollInterval = time.Hour
		r.feed.failFirst = true
	})
	r.start(t)
	// the first Listen fails, the engine restarts it, and Connected polls.
	eventually(t, "feed restarted", func() bool {
		r.feed.mu.Lock()
		defer r.feed.mu.Unlock()
		return r.feed.connects >= 2
	})
	r.lister.setStale(NewRef(KindTeam, "team_R", "team_R"))
	// A reconnect: calling Connected again polls even though the ticker is an hour away.
	r.feed.mu.Lock()
	h := r.feed.h
	r.feed.mu.Unlock()
	h.Connected()
	eventually(t, "poll after reconnect", func() bool { return r.ctl.calls(Key{KindTeam, "team_R"}) >= 1 })
}

func TestFailureSetsDegradedWithCodeAndRetriesWithBackoff(t *testing.T) {
	var times []time.Time
	var mu sync.Mutex
	r := newRig(t, nil)
	r.ctl.fn = func(_ context.Context, _ Ref, call int) (Outcome, error) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		if call <= 3 {
			return Outcome{}, WithCondition(domain.CondIamReady, &CodedError{Code: "AccessDenied", Err: errors.New("not allowed to iam:CreateRole")})
		}
		return Outcome{Generation: 2}, nil
	}
	r.start(t)
	r.feed.send(Change{"teams", "team_A", "team_A"})
	eventually(t, "recovered", func() bool {
		for _, rep := range r.status.all() {
			if rep.Phase == domain.PhaseReady {
				return true
			}
		}
		return false
	})
	reps := r.status.all()
	fail := reps[0]
	if fail.Phase != domain.PhaseDegraded || fail.Generation != nil || fail.Replace {
		t.Errorf("failure report = %+v", fail)
	}
	var iam, ready *domain.Condition
	for i := range fail.Conditions {
		switch fail.Conditions[i].Type {
		case domain.CondIamReady:
			iam = &fail.Conditions[i]
		case "Ready":
			ready = &fail.Conditions[i]
		}
	}
	if iam == nil || iam.Status != domain.ConditionFalse || iam.Reason != "AccessDenied" || ready == nil || ready.Reason != "AccessDenied" {
		t.Errorf("conditions = %+v", fail.Conditions)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) != 4 {
		t.Fatalf("reconciled %d times, want 4", len(times))
	}
	// 5 ms, 10 ms, 20 ms: each retry waits at least the backoff, never a hot loop.
	for i, min := range []time.Duration{5, 10, 20} {
		if gap := times[i+1].Sub(times[i]); gap < min*time.Millisecond {
			t.Errorf("retry %d came after %v, want at least %v", i+1, gap, min*time.Millisecond)
		}
	}
	if r.e.Stats().Failed != 3 {
		t.Errorf("failed = %d", r.e.Stats().Failed)
	}
}

func TestTerminalFailureIsFailedAndWaitsLongest(t *testing.T) {
	r := newRig(t, nil)
	r.ctl.fn = func(context.Context, Ref, int) (Outcome, error) { return Outcome{}, Terminal(errors.New("refused")) }
	r.start(t)
	r.feed.send(Change{"teams", "team_A", "team_A"})
	eventually(t, "failed", func() bool { return len(r.status.all()) >= 1 })
	if got := r.status.all()[0]; got.Phase != domain.PhaseFailed || got.Conditions[0].Reason != "ReconcileError" {
		t.Errorf("report = %+v", got)
	}
	time.Sleep(20 * time.Millisecond) // Max is 40 ms: not retried yet
	if n := r.ctl.calls(teamRef.Key); n != 1 {
		t.Errorf("terminal failure retried %d times within the longest backoff", n)
	}
}

func TestPanicIsAFailure(t *testing.T) {
	r := newRig(t, nil)
	r.ctl.fn = func(_ context.Context, _ Ref, call int) (Outcome, error) {
		if call == 1 {
			panic("nil map")
		}
		return Outcome{Generation: 1}, nil
	}
	r.start(t)
	r.feed.send(Change{"teams", "team_A", "team_A"})
	eventually(t, "panic then recovery", func() bool { return r.ctl.calls(teamRef.Key) >= 2 })
	if got := r.status.all()[0]; got.Phase != domain.PhaseDegraded {
		t.Errorf("panic reported as %v", got.Phase)
	}
}

func TestRequeueAndPhase(t *testing.T) {
	r := newRig(t, nil)
	r.ctl.fn = func(_ context.Context, _ Ref, call int) (Outcome, error) {
		if call == 1 {
			return Outcome{Generation: 1, Phase: domain.PhaseReconciling, Requeue: 5 * time.Millisecond}, nil
		}
		return Outcome{Generation: 1}, nil
	}
	r.start(t)
	r.feed.send(Change{"teams", "team_A", "team_A"})
	eventually(t, "requeued", func() bool { return r.ctl.calls(teamRef.Key) >= 2 })
	reps := r.status.all()
	if reps[0].Phase != domain.PhaseReconciling {
		t.Errorf("first phase = %v", reps[0].Phase)
	}
}

func TestStatusWriteFailureRetries(t *testing.T) {
	r := newRig(t, nil)
	r.status.err = errors.New("db down")
	r.start(t)
	r.feed.send(Change{"teams", "team_A", "team_A"})
	eventually(t, "retried after a status failure", func() bool { return r.ctl.calls(teamRef.Key) >= 2 })
}

func TestDriftCorrectionRecordedOnce(t *testing.T) {
	obj := ObjectName("NetworkPolicy", "afs-t-a", "default-deny-ingress")
	other := ObjectName("ResourceQuota", "afs-t-a", "quota")
	r := newRig(t, nil)
	r.ctl.fn = func(_ context.Context, _ Ref, call int) (Outcome, error) {
		if call == 1 { // the drift-triggered pass changes the flagged object
			return Outcome{Generation: 1, Changed: []string{obj}}, nil
		}
		return Outcome{Generation: 1}, nil
	}
	r.start(t)
	r.e.NoteDrift(teamRef, obj)
	r.e.NoteDrift(teamRef, obj) // a second event for the same object coalesces
	r.e.NoteDrift(teamRef, other)
	eventually(t, "status", func() bool { return len(r.status.all()) >= 1 })
	rep := r.status.all()[0]
	if rep.DriftDetectedAt == nil {
		t.Error("DriftDetectedAt not set")
	}
	var dc *domain.Condition
	for i := range rep.Conditions {
		if rep.Conditions[i].Type == domain.CondDriftCorrected {
			dc = &rep.Conditions[i]
		}
	}
	if dc == nil || dc.Status != domain.ConditionTrue || dc.Reason != "ObjectReverted" {
		t.Fatalf("conditions = %+v", rep.Conditions)
	}
	r.drift.mu.Lock()
	defer r.drift.mu.Unlock()
	if len(r.drift.recs) != 1 || r.drift.recs[0].Object != obj || r.drift.recs[0].Ref != teamRef {
		t.Errorf("drift records = %+v; the unchanged object must not be recorded and the duplicate coalesced", r.drift.recs)
	}
}

func TestJobsRunPeriodicallyAndSurviveFailures(t *testing.T) {
	var runs, imm atomic.Int32
	r := newRig(t, nil)
	r.e.Every("flaky", 5*time.Millisecond, func(context.Context) error {
		if runs.Add(1)%2 == 0 {
			return errors.New("transient")
		}
		return nil
	})
	r.e.AddJob(Job{Name: "once", Every: time.Hour, Immediate: true, Fn: func(context.Context) error { imm.Add(1); panic("job panic") }})
	r.start(t)
	eventually(t, "job ran several times despite failures", func() bool { return runs.Load() >= 4 })
	if imm.Load() != 1 {
		t.Errorf("immediate job ran %d times", imm.Load())
	}
}

func TestJobsStopWhenRunStops(t *testing.T) {
	var runs atomic.Int32
	r := newRig(t, nil)
	r.e.Every("j", 2*time.Millisecond, func(context.Context) error { runs.Add(1); return nil })
	r.start(t)
	eventually(t, "job", func() bool { return runs.Load() >= 1 })
	r.stop()
	n := runs.Load()
	time.Sleep(15 * time.Millisecond)
	if runs.Load() != n {
		t.Error("job kept running after Run returned")
	}
}

func TestPerTeamOrdering(t *testing.T) {
	var active, maxActive atomic.Int32
	cA := &fakeController{kind: KindTeam}
	cB := &fakeController{kind: KindAgent}
	slow := func(_ context.Context, _ Ref, _ int) (Outcome, error) {
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		return Outcome{Generation: 1}, nil
	}
	cA.fn, cB.fn = slow, slow
	reg, _ := NewRegistry(Route{Table: "teams", Targets: []Target{{Kind: KindTeam, Self: true}}})
	st := &fakeStatus{}
	e, err := New(Config{Feed: newFakeFeed(), Lister: &fakeLister{}, Registry: reg, Controllers: []Controller{cA, cB}, Status: st, Workers: 4,
		PollInterval: time.Hour, ResyncInterval: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Run(ctx) }()
	defer func() { cancel(); <-done }()
	// four resources of team_A: they must run one after the other
	for i := range 2 {
		e.Enqueue(NewRef(KindTeam, fmt.Sprintf("team_A%d", i), "team_A"))
		e.Enqueue(NewRef(KindAgent, fmt.Sprintf("agt_%d", i), "team_A"))
	}
	eventually(t, "all four", func() bool { return len(st.all()) >= 4 })
	if maxActive.Load() != 1 {
		t.Errorf("%d resources of one team reconciled at once", maxActive.Load())
	}
}

func TestFailureKeepsConditionsOfTheSucceededParts(t *testing.T) {
	r := newRig(t, nil)
	r.ctl.fn = func(context.Context, Ref, int) (Outcome, error) {
		return Outcome{Conditions: []domain.Condition{{Type: domain.CondNamespaceReady, Status: domain.ConditionTrue}}},
			WithCondition(domain.CondIamReady, &CodedError{Code: "AccessDenied", Err: errors.New("denied")})
	}
	r.start(t)
	r.feed.send(Change{"teams", "team_A", "team_A"})
	eventually(t, "failure report", func() bool { return len(r.status.all()) >= 1 })
	got := map[string]domain.Condition{}
	for _, c := range r.status.all()[0].Conditions {
		got[c.Type] = c
	}
	if got[domain.CondNamespaceReady].Status != domain.ConditionTrue || got[domain.CondIamReady].Status != domain.ConditionFalse || got["Ready"].Status != domain.ConditionFalse {
		t.Errorf("conditions = %+v", got)
	}
}

func TestEnqueueBackgroundSkipsFailingResources(t *testing.T) {
	r := newRig(t, nil)
	r.e.queue.AddRateLimited(teamRef, false) // in backoff: a retry timer is pending
	r.e.queue.Add(teamRef)                   // no-op, already waiting; drain it
	g, _ := get(t, r.e.queue)
	r.e.queue.Done(g)
	r.e.queue.AddRateLimited(teamRef, false)
	before := r.e.queue.Len()
	r.e.enqueueBackground(teamRef)
	if r.e.queue.Len() != before {
		t.Error("a poll queued a resource that is in backoff")
	}
	other := NewRef(KindTeam, "team_Q", "team_Q")
	r.e.enqueueBackground(other)
	if r.e.queue.Len() != before+1 {
		t.Error("a poll did not queue a healthy resource")
	}
}

// M1-I1: a failing delete step is retried with the engine's backoff and the resource stays Deleting;
// the controller (the finalizer) owns the step conditions, so the failure report adds none.
func TestFailureWhileDeletingKeepsPhaseDeleting(t *testing.T) {
	r := newRig(t, nil)
	r.ctl.fn = func(_ context.Context, _ Ref, call int) (Outcome, error) {
		if call <= 1 {
			return Outcome{Phase: domain.PhaseDeleting, NoStatus: true}, errors.New("step failed")
		}
		return Outcome{NoStatus: true}, nil
	}
	r.start(t)
	r.feed.send(Change{"teams", "team_A", "team_A"})
	eventually(t, "retried", func() bool { return r.ctl.calls(Key{KindTeam, "team_A"}) >= 2 })
	reps := r.status.all()
	if len(reps) != 1 {
		t.Fatalf("reports = %+v", reps)
	}
	if reps[0].Phase != domain.PhaseDeleting || len(reps[0].Conditions) != 0 || reps[0].Replace {
		t.Errorf("failure report while deleting = %+v", reps[0])
	}
}

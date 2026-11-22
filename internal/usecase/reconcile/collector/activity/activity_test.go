package activity_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/activity"
)

const (
	team = "team_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	agt1 = "agt_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	agt2 = "agt_01JD2Q3R4S5T6V7W8X9Y0Z0002"
	agt3 = "agt_01JD2Q3R4S5T6V7W8X9Y0Z0003"
	agt4 = "agt_01JD2Q3R4S5T6V7W8X9Y0Z0004"
	pool = "pool_01JD2Q3R4S5T6V7W8X9Y0Z0001"
	ns   = "afs-t-payments"
)

var t0 = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

type fakes struct {
	mu        sync.Mutex
	roster    activity.Roster
	rosterErr error
	pods      []activity.Pod
	status    map[string]activity.StatusReport // by pod name; missing: ErrNoStatus
	statusErr error
	usage     map[activity.UsageKey]activity.Usage
	usageErr  error
	threshold int
	thErr     error
	stored    []activity.Sample
	storeErr  error
	storeCall int
}

func (f *fakes) Roster(context.Context) (activity.Roster, error) { return f.roster, f.rosterErr }
func (f *fakes) Pods(_ context.Context, namespaces []string) ([]activity.Pod, error) {
	if len(namespaces) != 1 || namespaces[0] != ns {
		return nil, errors.New("unexpected namespaces " + strings.Join(namespaces, ","))
	}
	return f.pods, nil
}
func (f *fakes) Report(_ context.Context, p activity.Pod) (activity.StatusReport, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return activity.StatusReport{}, f.statusErr
	}
	r, ok := f.status[p.Name]
	if !ok {
		return activity.StatusReport{}, activity.ErrNoStatus
	}
	return r, nil
}
func (f *fakes) Window(_ context.Context, from, to time.Time) (map[activity.UsageKey]activity.Usage, error) {
	if to.Sub(from) != activity.BucketSize {
		return nil, errors.New("window is not 5 minutes")
	}
	return f.usage, f.usageErr
}
func (f *fakes) IdleCPUMillicores(context.Context) (int, error) { return f.threshold, f.thErr }
func (f *fakes) Add(_ context.Context, s []activity.Sample) error {
	f.storeCall++
	if f.storeErr != nil {
		return f.storeErr
	}
	f.stored = append(f.stored, s...)
	return nil
}

type upper struct{}

func (upper) Redact(s string) string { return strings.ReplaceAll(s, "hunter2", "[redacted:abcd1234]") }

func ip(n int) *int { return &n }

func newFakes() *fakes {
	return &fakes{threshold: 50, status: map[string]activity.StatusReport{}, usage: map[activity.UsageKey]activity.Usage{}}
}

func (f *fakes) collector(withStatus bool) *activity.Collector {
	cfg := activity.Config{Roster: f, Pods: f, Usage: f, Threshold: f, Store: f, Redactor: upper{}, SampleEvery: 30 * time.Second}
	if withStatus {
		cfg.Status = f
	}
	return activity.New(cfg)
}

func pod(name, agent string, ready bool, cpu *int) activity.Pod {
	return activity.Pod{Namespace: ns, Name: name, AgentID: agent, Ready: ready, Millicores: cpu, MemMiB: ip(256)}
}

func bySubject(s []activity.Sample) map[string]activity.Sample {
	m := map[string]activity.Sample{}
	for _, x := range s {
		m[x.AgentID+x.PoolID+x.WorkerPod] = x
	}
	return m
}

// observeWindow observes at 0, 30 s, ... 270 s of the window starting at t0, then finalizes after it.
func observeWindow(t *testing.T, c *activity.Collector, windowStart time.Time) {
	t.Helper()
	for i := range 10 {
		if err := c.Observe(t.Context(), windowStart.Add(time.Duration(i)*30*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
}

// RC-US-6.1: one sample per running agent per window; working when it made a model call, reported busy,
// or used CPU above the threshold; idle otherwise; non-running states are recorded as such.
func TestOneSamplePerAgentPerWindow(t *testing.T) {
	f := newFakes()
	f.roster = activity.Roster{Agents: []activity.Agent{
		{TeamID: team, AgentID: agt1, Namespace: ns}, // calls
		{TeamID: team, AgentID: agt2, Namespace: ns}, // busy by status
		{TeamID: team, AgentID: agt3, Namespace: ns}, // idle
		{TeamID: team, AgentID: agt4, Namespace: ns}, // hot CPU
		{TeamID: team, AgentID: "agt_01JD2Q3R4S5T6V7W8X9Y0Z0005", Namespace: ns, Paused: true},
		{TeamID: team, AgentID: "agt_01JD2Q3R4S5T6V7W8X9Y0Z0006", Namespace: ns, BudgetBlocked: true},
		{TeamID: team, AgentID: "agt_01JD2Q3R4S5T6V7W8X9Y0Z0007", Namespace: ns},                                    // no pod
		{TeamID: team, AgentID: "agt_01JD2Q3R4S5T6V7W8X9Y0Z0008", Namespace: ns},                                    // not ready
		{TeamID: team, AgentID: "agt_01JD2Q3R4S5T6V7W8X9Y0Z0009", Namespace: ns, Paused: true, BudgetBlocked: true}, // paused wins
	}}
	f.pods = []activity.Pod{
		pod("a1-0", agt1, true, ip(10)), pod("a2-0", agt2, true, ip(5)), pod("a3-0", agt3, true, ip(20)), pod("a4-0", agt4, true, ip(400)),
		pod("a8-0", "agt_01JD2Q3R4S5T6V7W8X9Y0Z0008", false, nil),
	}
	f.status["a2-0"] = activity.StatusReport{Busy: true, Note: "reviewing PR 482 with hunter2"}
	f.status["a3-0"] = activity.StatusReport{Busy: false, Note: "waiting"}
	f.usage[activity.UsageKey{AgentID: agt1}] = activity.Usage{Calls: 4, InputTokens: 1000, OutputTokens: 200, Cost: domain.MustMoney("0.12")}
	c := f.collector(true)
	observeWindow(t, c, t0)
	if err := c.Finalize(t.Context(), t0.Add(2*time.Minute)); err != nil || len(f.stored) != 0 {
		t.Fatalf("a window still open is not stored: %v %v", err, f.stored)
	}
	if err := c.Finalize(t.Context(), t0.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got := bySubject(f.stored)
	if len(f.stored) != 9 {
		t.Fatalf("samples = %d", len(f.stored))
	}
	for _, s := range f.stored {
		if !s.BucketStart.Equal(t0) || s.TeamID != team {
			t.Errorf("sample %+v", s)
		}
	}
	a1 := got[agt1]
	if a1.State != activity.StateWorking || a1.ModelCalls != 4 || a1.InputTokens != 1000 || a1.Cost.String() != "0.12" ||
		a1.CPUAvg == nil || *a1.CPUAvg != 10 || a1.MemAvg == nil || *a1.MemAvg != 256 || a1.ActiveSeconds != 30 {
		t.Errorf("model calls: %+v", a1)
	}
	a2 := got[agt2]
	if a2.State != activity.StateWorking || a2.ModelCalls != 0 || a2.ActiveSeconds != 300 || a2.Note != "reviewing PR 482 with [redacted:abcd1234]" {
		t.Errorf("busy: %+v", a2)
	}
	if a3 := got[agt3]; a3.State != activity.StateIdle || a3.ActiveSeconds != 0 || a3.Note != "waiting" {
		t.Errorf("idle: %+v", a3)
	}
	if a4 := got[agt4]; a4.State != activity.StateWorking || a4.ActiveSeconds != 300 {
		t.Errorf("cpu: %+v", a4)
	}
	for id, want := range map[string]activity.State{
		"agt_01JD2Q3R4S5T6V7W8X9Y0Z0005": activity.StatePaused, "agt_01JD2Q3R4S5T6V7W8X9Y0Z0006": activity.StateBlockedBudget,
		"agt_01JD2Q3R4S5T6V7W8X9Y0Z0007": activity.StateStopped, "agt_01JD2Q3R4S5T6V7W8X9Y0Z0008": activity.StateUnhealthy,
		"agt_01JD2Q3R4S5T6V7W8X9Y0Z0009": activity.StatePaused,
	} {
		if s := got[id]; s.State != want || s.ActiveSeconds != 0 || s.ModelCalls != 0 || s.Note != "" {
			t.Errorf("%s: %+v, want %s", id, s, want)
		}
	}
	// A window is stored once.
	if err := c.Finalize(t.Context(), t0.Add(10*time.Minute)); err != nil || len(f.stored) != 9 || f.storeCall != 1 {
		t.Errorf("second finalize: %v %d %d", err, len(f.stored), f.storeCall)
	}
}

// Without a status endpoint (OQ-7) model calls and CPU decide; a failing endpoint does the same.
func TestStatusEndpointFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status bool
		err    error
	}{{"no status source", false, nil}, {"endpoint missing", true, nil}, {"endpoint broken", true, errors.New("connection reset")}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakes()
			f.statusErr = tc.err
			f.roster = activity.Roster{Agents: []activity.Agent{{TeamID: team, AgentID: agt1, Namespace: ns}, {TeamID: team, AgentID: agt2, Namespace: ns}}}
			f.pods = []activity.Pod{pod("a1-0", agt1, true, ip(100)), pod("a2-0", agt2, true, ip(3))}
			c := f.collector(tc.status)
			observeWindow(t, c, t0)
			if err := c.Finalize(t.Context(), t0.Add(6*time.Minute)); err != nil {
				t.Fatal(err)
			}
			got := bySubject(f.stored)
			if got[agt1].State != activity.StateWorking || got[agt2].State != activity.StateIdle {
				t.Errorf("%+v", got)
			}
		})
	}
}

// CPU is judged on the window's average: one spike in ten observations does not make an agent working.
func TestCPUAverageAndThreshold(t *testing.T) {
	f := newFakes()
	f.threshold = 100
	f.roster = activity.Roster{Agents: []activity.Agent{{TeamID: team, AgentID: agt1, Namespace: ns}}}
	c := f.collector(false)
	for i, cpu := range []int{10, 10, 10, 500, 10, 10, 10, 10, 10, 10} {
		f.pods = []activity.Pod{pod("a1-0", agt1, true, ip(cpu))}
		if err := c.Observe(t.Context(), t0.Add(time.Duration(i)*30*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.Finalize(t.Context(), t0.Add(5*time.Minute))
	if s := f.stored[0]; s.State != activity.StateIdle || *s.CPUAvg != 59 {
		t.Errorf("%+v", s)
	}
	// An unreadable threshold falls back to 50, so 59 is above it.
	f2 := newFakes()
	f2.thErr = errors.New("db down")
	f2.roster = f.roster
	f2.pods = []activity.Pod{pod("a1-0", agt1, true, ip(59))}
	c2 := f2.collector(false)
	_ = c2.Observe(t.Context(), t0)
	_ = c2.Finalize(t.Context(), t0.Add(5*time.Minute))
	if f2.stored[0].State != activity.StateWorking {
		t.Errorf("default threshold: %+v", f2.stored[0])
	}
	// A pod with no metrics has no CPU average and is idle.
	f3 := newFakes()
	f3.roster = f.roster
	f3.pods = []activity.Pod{pod("a1-0", agt1, true, nil)}
	c3 := f3.collector(false)
	_ = c3.Observe(t.Context(), t0)
	_ = c3.Finalize(t.Context(), t0.Add(5*time.Minute))
	if s := f3.stored[0]; s.State != activity.StateIdle || s.CPUAvg != nil {
		t.Errorf("no metrics: %+v", s)
	}
}

// A pool gets one sample per worker pod, with the pool id and the pod name; model calls come from the
// gateway's usage of that worker.
func TestPoolWorkers(t *testing.T) {
	f := newFakes()
	f.roster = activity.Roster{Pools: []activity.Pool{{TeamID: team, PoolID: pool, Namespace: ns}}}
	w := func(name string, ready bool, cpu int) activity.Pod {
		return activity.Pod{Namespace: ns, Name: name, PoolID: pool, Ready: ready, Millicores: ip(cpu)}
	}
	f.pods = []activity.Pod{w("pool-0", true, 5), w("pool-1", true, 5), w("pool-2", false, 5), {Namespace: ns, Name: "stray", Ready: true}}
	f.usage[activity.UsageKey{PoolID: pool, WorkerPod: "pool-1"}] = activity.Usage{Calls: 1}
	c := f.collector(false)
	observeWindow(t, c, t0)
	_ = c.Finalize(t.Context(), t0.Add(5*time.Minute))
	got := bySubject(f.stored)
	if len(got) != 3 || got[pool+"pool-0"].State != activity.StateIdle || got[pool+"pool-1"].State != activity.StateWorking || got[pool+"pool-2"].State != activity.StateUnhealthy {
		t.Fatalf("%+v", got)
	}
	if s := got[pool+"pool-1"]; s.AgentID != "" || s.PoolID != pool || s.WorkerPod != "pool-1" {
		t.Errorf("%+v", s)
	}
	// A paused pool's leftover workers and a budget-blocked pool are recorded as such.
	f.roster.Pools[0].Paused = true
	f.stored = nil
	c2 := f.collector(false)
	_ = c2.Observe(t.Context(), t0)
	_ = c2.Finalize(t.Context(), t0.Add(5*time.Minute))
	for _, s := range f.stored {
		if s.State != activity.StatePaused && s.State != activity.StateWorking {
			t.Errorf("%+v", s)
		}
	}
}

func TestWindowsAndFailures(t *testing.T) {
	f := newFakes()
	f.roster = activity.Roster{Agents: []activity.Agent{{TeamID: team, AgentID: agt1, Namespace: ns}}}
	f.pods = []activity.Pod{pod("a1-0", agt1, true, ip(1))}
	c := f.collector(false)
	// Two windows are stored separately, oldest first.
	_ = c.Observe(t.Context(), t0)
	_ = c.Observe(t.Context(), t0.Add(5*time.Minute))
	if err := c.Finalize(t.Context(), t0.Add(11*time.Minute)); err != nil || len(f.stored) != 2 || !f.stored[0].BucketStart.Equal(t0) || !f.stored[1].BucketStart.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("%v %+v", err, f.stored)
	}

	// A failing store keeps the window for the next try; a failing usage read does too.
	f2 := newFakes()
	f2.roster, f2.pods = f.roster, f.pods
	f2.storeErr = errors.New("db down")
	c2 := f2.collector(false)
	_ = c2.Observe(t.Context(), t0)
	if err := c2.Finalize(t.Context(), t0.Add(6*time.Minute)); err == nil {
		t.Fatal("want the store error")
	}
	f2.storeErr = nil
	f2.usageErr = errors.New("usage down")
	if err := c2.Finalize(t.Context(), t0.Add(7*time.Minute)); err == nil || len(f2.stored) != 0 {
		t.Fatalf("usage error: %v", err)
	}
	f2.usageErr = nil
	if err := c2.Finalize(t.Context(), t0.Add(8*time.Minute)); err != nil || len(f2.stored) != 1 {
		t.Fatalf("retry: %v %d", err, len(f2.stored))
	}
	// After half an hour of failure the window is dropped.
	f3 := newFakes()
	f3.roster, f3.pods = f.roster, f.pods
	f3.storeErr = errors.New("db down")
	c3 := f3.collector(false)
	_ = c3.Observe(t.Context(), t0)
	_ = c3.Finalize(t.Context(), t0.Add(40*time.Minute))
	f3.storeErr = nil
	_ = c3.Finalize(t.Context(), t0.Add(41*time.Minute))
	if len(f3.stored) != 0 {
		t.Errorf("a dropped window came back: %+v", f3.stored)
	}
	// A failing roster is an error of the observation; nothing is recorded.
	f4 := newFakes()
	f4.rosterErr = errors.New("db down")
	if err := f4.collector(false).Observe(t.Context(), t0); err == nil {
		t.Error("want the roster error")
	}
	// Long notes are cut.
	f5 := newFakes()
	f5.roster, f5.pods = f.roster, f.pods
	f5.status[("a1-0")] = activity.StatusReport{Note: strings.Repeat("é", 500)}
	c5 := f5.collector(true)
	_ = c5.Observe(t.Context(), t0)
	_ = c5.Finalize(t.Context(), t0.Add(5*time.Minute))
	if n := []rune(f5.stored[0].Note); len(n) != activity.MaxNote {
		t.Errorf("note length %d", len(n))
	}
}

func TestRunStopsWithContext(t *testing.T) {
	f := newFakes()
	f.roster = activity.Roster{Agents: []activity.Agent{{TeamID: team, AgentID: agt1, Namespace: ns}}}
	f.pods = []activity.Pod{pod("a1-0", agt1, true, ip(1))}
	c := activity.New(activity.Config{Roster: f, Pods: f, Usage: f, Threshold: f, Store: f, SampleEvery: 5 * time.Millisecond})
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	if err := c.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// A failing roster is logged and the loop goes on.
	f.rosterErr = errors.New("down")
	ctx2, cancel2 := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel2()
	if err := c.Run(ctx2); err != nil {
		t.Fatal(err)
	}
}

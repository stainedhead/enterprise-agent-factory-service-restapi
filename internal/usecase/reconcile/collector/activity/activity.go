// Package activity is the activity collector of the reconciler (RC-7, RC-US-6.1). Every five minutes
// it writes one sample per agent, and per worker of a pool, to agent_activity: what state the agent was
// in during the window and what it did.
//
// An agent is working in a window when it made a model call through the gateway, reported itself busy
// on its status endpoint, or used more CPU than reports.idle_cpu_millicores on average; it is idle when
// it is running and did none of these. A paused, stopped, unhealthy or budget-blocked agent is recorded
// as such and counts as neither. The status endpoint is optional (OQ-7): where an agent has none, or it
// does not answer, model calls and CPU decide.
//
// The collector observes every SampleEvery (default 30 s) and finalizes a window once it has ended:
// the observations of the window are folded together with the model calls the gateway metered in it,
// and the samples are stored with the team's tag set at that time (by the Store). A window finalized
// twice, for example after a leader change, is stored once.
package activity

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Collector constants.
const (
	// BucketSize is the width of a sample window.
	BucketSize = 5 * time.Minute
	// DefaultSampleEvery is how often the agents are observed.
	DefaultSampleEvery = 30 * time.Second
	// DefaultIdleCPU is reports.idle_cpu_millicores when the setting cannot be read.
	DefaultIdleCPU = 50
	// MaxNote is the longest activity note stored.
	MaxNote = 200
	// retryFor is how long a window that cannot be stored is retried before it is dropped.
	retryFor = 30 * time.Minute
	// statusParallel bounds the concurrent status endpoint calls of one observation.
	statusParallel = 8
)

// State is the state of a sample (agent_activity.state).
type State string

// States.
const (
	StateWorking       State = "working"
	StateIdle          State = "idle"
	StatePaused        State = "paused"
	StateStopped       State = "stopped"
	StateUnhealthy     State = "unhealthy"
	StateBlockedBudget State = "blocked_budget"
)

// Agent is an agent that exists in the desired state.
type Agent struct {
	TeamID, AgentID, Namespace string
	// Paused is true when the agent, or its team, is paused or suspended.
	Paused bool
	// BudgetBlocked is true when the team's BudgetExhausted condition is True.
	BudgetBlocked bool
}

// Pool is a pool that exists in the desired state; its workers are the pods that carry its label.
type Pool struct {
	TeamID, PoolID, Namespace string
	Paused, BudgetBlocked     bool
}

// Roster is what the desired state says exists.
type Roster struct {
	Agents []Agent
	Pools  []Pool
}

// RosterSource reads the roster.
type RosterSource interface {
	Roster(ctx context.Context) (Roster, error)
}

// Pod is a pod of an agent or a pool worker with its current resource use.
type Pod struct {
	Namespace, Name string
	// AgentID or PoolID is the pod's label; exactly one is set.
	AgentID, PoolID string
	// IP is the pod address the status endpoint is reached at.
	IP string
	// Ready is true for a running pod whose Ready condition is true; Failed for a pod in phase Failed.
	Ready, Failed bool
	// Millicores and MemMiB are nil when the metrics API has no value for the pod.
	Millicores, MemMiB *int
}

// PodSource lists the agent and pool worker pods of the namespaces.
type PodSource interface {
	Pods(ctx context.Context, namespaces []string) ([]Pod, error)
}

// StatusReport is what an agent says about itself.
type StatusReport struct {
	// Busy is true while the agent is working on something.
	Busy bool
	// Note says what it is doing; it is redacted and cut to MaxNote before storage.
	Note string
}

// ErrNoStatus means the pod has no status endpoint or it did not answer (OQ-7: fall back to model calls
// and CPU).
var ErrNoStatus = errors.New("activity: no status endpoint")

// StatusSource asks an agent pod for its status. It is optional.
type StatusSource interface {
	Report(ctx context.Context, p Pod) (StatusReport, error)
}

// Usage is what the gateway metered for one subject in one window.
type Usage struct {
	Calls                     int
	InputTokens, OutputTokens int64
	Cost                      domain.Money
}

// UsageKey names a subject of usage: an agent, or a worker of a pool.
type UsageKey struct{ AgentID, PoolID, WorkerPod string }

// UsageSource reads the metered usage of the window [from, to) per subject.
type UsageSource interface {
	Window(ctx context.Context, from, to time.Time) (map[UsageKey]Usage, error)
}

// ThresholdSource reads reports.idle_cpu_millicores.
type ThresholdSource interface {
	IdleCPUMillicores(ctx context.Context) (int, error)
}

// Sample is one row of agent_activity.
type Sample struct {
	BucketStart time.Time
	TeamID      string
	// AgentID, or PoolID and WorkerPod.
	AgentID, PoolID, WorkerPod string
	State                      State
	ActiveSeconds              int
	ModelCalls                 int
	InputTokens, OutputTokens  int64
	Cost                       domain.Money
	CPUAvg, MemAvg             *int
	Note                       string
}

// Store writes samples, stamping each with the team's tag set, and ignores a sample that already exists.
type Store interface {
	Add(ctx context.Context, samples []Sample) error
}

// Redactor is the redaction filter (usecase/redact).
type Redactor interface {
	Redact(s string) string
}

// Config configures the Collector. Roster, Pods, Usage, Threshold and Store are required.
type Config struct {
	Roster    RosterSource
	Pods      PodSource
	Status    StatusSource
	Usage     UsageSource
	Threshold ThresholdSource
	Store     Store
	Redactor  Redactor

	SampleEvery time.Duration
	Now         func() time.Time
	Log         *slog.Logger
}

type subject struct {
	team, namespace string
	agent, pool     string
	worker          string
}

func (s subject) key() UsageKey {
	return UsageKey{AgentID: s.agent, PoolID: s.pool, WorkerPod: s.worker}
}

// window accumulates the observations of one subject in one window.
type window struct {
	subj         subject
	last         State // state at the latest observation: running is stored as StateIdle
	observations int
	evidence     int // observations in which the subject was busy or above the CPU threshold
	cpuSum, cpuN int
	memSum, memN int
	busy         int
	note         string
}

// Collector observes agents and writes activity samples.
type Collector struct {
	cfg Config

	mu      sync.Mutex
	windows map[time.Time]map[UsageKey]*window
}

// New returns a Collector.
func New(cfg Config) *Collector {
	if cfg.SampleEvery <= 0 {
		cfg.SampleEvery = DefaultSampleEvery
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Collector{cfg: cfg, windows: map[time.Time]map[UsageKey]*window{}}
}

// Run observes and finalizes until ctx ends.
func (c *Collector) Run(ctx context.Context) error {
	t := time.NewTicker(c.cfg.SampleEvery)
	defer t.Stop()
	for {
		now := c.cfg.Now()
		if err := c.Observe(ctx, now); err != nil && ctx.Err() == nil {
			c.cfg.Log.Warn("activity observation failed", slog.Any("err", err))
		}
		if err := c.Finalize(ctx, now); err != nil && ctx.Err() == nil {
			c.cfg.Log.Warn("activity samples not stored", slog.Any("err", err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (c *Collector) idleCPU(ctx context.Context) int {
	n, err := c.cfg.Threshold.IdleCPUMillicores(ctx)
	if err != nil || n < 1 {
		c.cfg.Log.Warn("reports.idle_cpu_millicores unreadable, using the default", slog.Any("err", err))
		return DefaultIdleCPU
	}
	return n
}

// observed is one subject with what was seen of it.
type observed struct {
	subj   subject
	state  State // StateIdle stands for running
	pod    *Pod
	report *StatusReport
}

func stateOf(paused, blocked bool, pod *Pod) State {
	switch {
	case paused:
		return StatePaused
	case blocked:
		return StateBlockedBudget
	case pod == nil:
		return StateStopped
	case pod.Failed || !pod.Ready:
		return StateUnhealthy
	}
	return StateIdle
}

// Observe records the state of every agent and pool worker at now.
func (c *Collector) Observe(ctx context.Context, now time.Time) error {
	roster, err := c.cfg.Roster.Roster(ctx)
	if err != nil {
		return err
	}
	nsSet := map[string]bool{}
	for _, a := range roster.Agents {
		nsSet[a.Namespace] = true
	}
	for _, p := range roster.Pools {
		nsSet[p.Namespace] = true
	}
	namespaces := make([]string, 0, len(nsSet))
	for ns := range nsSet {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	pods, err := c.cfg.Pods.Pods(ctx, namespaces)
	if err != nil {
		return err
	}
	byAgent, byPool := map[string]*Pod{}, map[string][]*Pod{}
	for i := range pods {
		p := &pods[i]
		switch {
		case p.AgentID != "":
			byAgent[p.AgentID] = p
		case p.PoolID != "":
			byPool[p.PoolID] = append(byPool[p.PoolID], p)
		}
	}
	var obs []observed
	for _, a := range roster.Agents {
		pod := byAgent[a.AgentID]
		obs = append(obs, observed{subj: subject{team: a.TeamID, namespace: a.Namespace, agent: a.AgentID}, state: stateOf(a.Paused, a.BudgetBlocked, pod), pod: pod})
	}
	for _, p := range roster.Pools {
		for _, pod := range byPool[p.PoolID] {
			obs = append(obs, observed{subj: subject{team: p.TeamID, namespace: p.Namespace, pool: p.PoolID, worker: pod.Name}, state: stateOf(p.Paused, p.BudgetBlocked, pod), pod: pod})
		}
	}
	c.askStatus(ctx, obs)
	threshold := c.idleCPU(ctx)
	w := now.UTC().Truncate(BucketSize)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.windows[w] == nil {
		c.windows[w] = map[UsageKey]*window{}
	}
	for _, o := range obs {
		acc := c.windows[w][o.subj.key()]
		if acc == nil {
			acc = &window{subj: o.subj}
			c.windows[w][o.subj.key()] = acc
		}
		acc.last = o.state
		acc.observations++
		hot := false
		if o.pod != nil && o.state == StateIdle {
			if o.pod.Millicores != nil {
				acc.cpuSum += *o.pod.Millicores
				acc.cpuN++
				hot = *o.pod.Millicores > threshold
			}
			if o.pod.MemMiB != nil {
				acc.memSum += *o.pod.MemMiB
				acc.memN++
			}
		}
		if o.report != nil {
			if o.report.Busy {
				acc.busy++
				hot = true
			}
			if o.report.Note != "" {
				acc.note = o.report.Note
			}
		}
		if hot {
			acc.evidence++
		}
	}
	return nil
}

// askStatus asks the running pods for their status, a few at a time. A pod without an endpoint, or one
// that does not answer, is left to the model calls and CPU.
func (c *Collector) askStatus(ctx context.Context, obs []observed) {
	if c.cfg.Status == nil {
		return
	}
	sem := make(chan struct{}, statusParallel)
	var wg sync.WaitGroup
	for i := range obs {
		if obs[i].state != StateIdle || obs[i].pod == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(o *observed) {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := c.cfg.Status.Report(ctx, *o.pod)
			if err != nil {
				if !errors.Is(err, ErrNoStatus) {
					c.cfg.Log.Debug("status endpoint failed", slog.String("pod", o.pod.Name), slog.Any("err", err))
				}
				return
			}
			o.report = &r
		}(&obs[i])
	}
	wg.Wait()
}

func (c *Collector) note(s string) string {
	if c.cfg.Redactor != nil {
		s = c.cfg.Redactor.Redact(s)
	}
	if r := []rune(s); len(r) > MaxNote {
		s = string(r[:MaxNote])
	}
	return s
}

// sampleOf folds the observations and the metered usage of a window into a sample.
func (c *Collector) sampleOf(start time.Time, acc *window, u Usage, hasUsage bool, threshold int) Sample {
	s := Sample{BucketStart: start, TeamID: acc.subj.team, AgentID: acc.subj.agent, PoolID: acc.subj.pool, WorkerPod: acc.subj.worker, State: acc.last}
	if acc.cpuN > 0 {
		avg := acc.cpuSum / acc.cpuN
		s.CPUAvg = &avg
	}
	if acc.memN > 0 {
		avg := acc.memSum / acc.memN
		s.MemAvg = &avg
	}
	cpuHigh := s.CPUAvg != nil && *s.CPUAvg > threshold
	if hasUsage && u.Calls > 0 || acc.busy > 0 || cpuHigh {
		s.State = StateWorking
		active := acc.evidence * int(c.cfg.SampleEvery/time.Second)
		// At least one observation interval: a call or a busy report is evidence of that much work.
		s.ActiveSeconds = min(max(active, int(c.cfg.SampleEvery/time.Second)), int(BucketSize/time.Second))
		s.ModelCalls, s.InputTokens, s.OutputTokens, s.Cost = u.Calls, u.InputTokens, u.OutputTokens, u.Cost
	}
	if s.State == StateWorking || s.State == StateIdle {
		s.Note = c.note(acc.note)
	}
	return s
}

// Finalize stores the samples of every window that has ended. A window that cannot be stored is kept
// and tried again on the next call, for half an hour.
func (c *Collector) Finalize(ctx context.Context, now time.Time) error {
	current := now.UTC().Truncate(BucketSize)
	c.mu.Lock()
	var due []time.Time
	for w := range c.windows {
		if w.Before(current) {
			due = append(due, w)
		}
	}
	c.mu.Unlock()
	sort.Slice(due, func(i, j int) bool { return due[i].Before(due[j]) })
	threshold := c.idleCPU(ctx)
	var firstErr error
	for _, w := range due {
		if err := c.finalizeWindow(ctx, w, threshold); err != nil {
			if now.Sub(w) > retryFor {
				c.cfg.Log.Warn("activity window dropped", slog.Time("window", w), slog.Any("err", err))
				c.drop(w)
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (c *Collector) drop(w time.Time) {
	c.mu.Lock()
	delete(c.windows, w)
	c.mu.Unlock()
}

func (c *Collector) finalizeWindow(ctx context.Context, w time.Time, threshold int) error {
	usage, err := c.cfg.Usage.Window(ctx, w, w.Add(BucketSize))
	if err != nil {
		return err
	}
	c.mu.Lock()
	accs := make([]*window, 0, len(c.windows[w]))
	for _, a := range c.windows[w] {
		accs = append(accs, a)
	}
	c.mu.Unlock()
	sort.Slice(accs, func(i, j int) bool {
		a, b := accs[i].subj, accs[j].subj
		return a.agent+a.pool+a.worker < b.agent+b.pool+b.worker
	})
	samples := make([]Sample, 0, len(accs))
	for _, a := range accs {
		u, ok := usage[a.subj.key()]
		samples = append(samples, c.sampleOf(w, a, u, ok, threshold))
	}
	if len(samples) > 0 {
		if err := c.cfg.Store.Add(ctx, samples); err != nil {
			return err
		}
	}
	c.drop(w)
	return nil
}

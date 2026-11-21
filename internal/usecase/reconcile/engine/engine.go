package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Config configures an Engine. Every field without a default is required.
type Config struct {
	Feed        ChangeFeed
	Lister      Lister
	Registry    *Registry
	Controllers []Controller
	Status      StatusWriter
	Drift       DriftRecorder
	// Singletons are resources without a database row (cluster-wide objects the reconciler owns):
	// they are queued at startup and at every resync.
	Singletons []Ref
	// Workers is the number of concurrent reconciles (default 4).
	Workers int
	// PollInterval is the stale-generation backstop (default 30 s).
	PollInterval time.Duration
	// ResyncInterval is the full resync (default 10 min).
	ResyncInterval time.Duration
	Backoff        Backoff
	Now            func() time.Time
	Log            *slog.Logger
}

// Engine runs the control loop. Run it only while holding leadership.
type Engine struct {
	cfg         Config
	queue       *queue
	controllers map[Kind]Controller
	kinds       []Kind

	pollNow chan struct{}

	mu    sync.Mutex
	drift map[Key]map[string]time.Time
	jobs  []Job

	reconciled, failed atomic.Int64
	lastPoll, lastSync atomic.Int64 // unix nanos
}

// New validates cfg and returns an Engine.
func New(cfg Config) (*Engine, error) {
	if cfg.Feed == nil || cfg.Lister == nil || cfg.Registry == nil || cfg.Status == nil {
		return nil, errors.New("engine: Feed, Lister, Registry and Status are required")
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.ResyncInterval <= 0 {
		cfg.ResyncInterval = 10 * time.Minute
	}
	if cfg.Backoff.Base <= 0 {
		cfg.Backoff = DefaultBackoff
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	e := &Engine{
		cfg: cfg, queue: newQueue(cfg.Now, cfg.Backoff), controllers: map[Kind]Controller{},
		pollNow: make(chan struct{}, 1), drift: map[Key]map[string]time.Time{},
	}
	for _, c := range cfg.Controllers {
		if _, dup := e.controllers[c.Kind()]; dup {
			return nil, fmt.Errorf("engine: two controllers for kind %q", c.Kind())
		}
		e.controllers[c.Kind()] = c
		e.kinds = append(e.kinds, c.Kind())
	}
	return e, nil
}

// Run blocks until ctx ends, running the feed, the poll, the resync, the periodic jobs and the
// workers. It returns nil on a clean stop.
func (e *Engine) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { defer wg.Done(); e.feedLoop(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); e.pollLoop(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); e.resyncLoop(ctx) }()
	e.runJobs(ctx, &wg)
	for range e.cfg.Workers {
		wg.Add(1)
		go func() { defer wg.Done(); e.worker(ctx) }()
	}

	<-ctx.Done()
	e.queue.ShutDown()
	wg.Wait()
	return nil
}

// Enqueue queues a resource for an immediate reconcile.
func (e *Engine) Enqueue(ref Ref) {
	if _, ok := e.controllers[ref.Kind]; ok {
		e.queue.Add(ref)
	}
}

// enqueueBackground queues a resource found by the poll or the resync. One that is failing already
// has a retry timer; queueing it now would cut its backoff short, so it is left alone.
func (e *Engine) enqueueBackground(ref Ref) {
	if e.queue.Failures(ref.Key) > 0 {
		return
	}
	e.Enqueue(ref)
}

func (e *Engine) feedLoop(ctx context.Context) {
	h := FeedHandler{
		Change: func(c Change) { e.handleChange(ctx, c) },
		Connected: func() {
			select {
			case e.pollNow <- struct{}{}:
			default:
			}
		},
	}
	wait := e.cfg.Backoff.Base
	for ctx.Err() == nil {
		err := e.cfg.Feed.Listen(ctx, h)
		if ctx.Err() != nil {
			return
		}
		e.cfg.Log.Warn("change feed stopped; restarting", slog.Any("err", err), slog.Duration("in", wait))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, e.cfg.Backoff.Max)
	}
}

func (e *Engine) handleChange(ctx context.Context, c Change) {
	refs, err := e.cfg.Registry.Resolve(ctx, c, e.cfg.Lister.List)
	if err != nil {
		e.cfg.Log.Warn("resolve change", slog.Any("change", c), slog.Any("err", err))
		return
	}
	for _, r := range refs {
		e.Enqueue(r)
	}
}

func (e *Engine) pollLoop(ctx context.Context) {
	t := time.NewTicker(e.cfg.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.pollNow:
		}
		e.poll(ctx)
	}
}

// poll queues every resource whose generation was not observed yet.
func (e *Engine) poll(ctx context.Context) {
	refs, err := e.cfg.Lister.Stale(ctx, e.kinds)
	if err != nil {
		e.cfg.Log.Warn("poll stale generations", slog.Any("err", err))
		return
	}
	e.lastPoll.Store(e.cfg.Now().UnixNano())
	for _, r := range refs {
		e.enqueueBackground(r)
	}
}

func (e *Engine) resyncLoop(ctx context.Context) {
	e.resync(ctx)
	t := time.NewTicker(e.cfg.ResyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.resync(ctx)
		}
	}
}

// resync queues everything: the full pass at startup and every ResyncInterval (RC-4).
func (e *Engine) resync(ctx context.Context) {
	refs, err := e.cfg.Lister.List(ctx, e.kinds)
	if err != nil {
		e.cfg.Log.Warn("full resync", slog.Any("err", err))
	}
	e.lastSync.Store(e.cfg.Now().UnixNano())
	for _, r := range append(refs, e.cfg.Singletons...) {
		e.enqueueBackground(r)
	}
}

func (e *Engine) worker(ctx context.Context) {
	for {
		ref, ok := e.queue.Get(ctx)
		if !ok {
			return
		}
		e.reconcile(ctx, ref)
		e.queue.Done(ref)
	}
}

// reconcile runs one pass for ref and reports the result.
func (e *Engine) reconcile(ctx context.Context, ref Ref) {
	c := e.controllers[ref.Kind]
	out, err := e.safeReconcile(ctx, c, ref)
	now := e.cfg.Now()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		e.failed.Add(1)
		// A terminal failure is fixed only by a change; it is retried at the longest interval.
		delay := e.queue.AddRateLimited(ref, errors.Is(err, ErrTerminal))
		e.cfg.Log.Warn("reconcile failed", slog.String("resource", ref.String()), slog.Any("err", err), slog.Duration("retry_in", delay))
		e.writeFailure(ctx, ref, out, err, now)
		return
	}
	e.reconciled.Add(1)
	e.queue.Forget(ref.Key)
	phase := out.Phase
	if !phase.Valid() {
		phase = domain.PhaseReady
	}
	conds := out.Conditions
	var driftAt *time.Time
	if _, objs := e.takeCorrectedDrift(ref.Key, out.Changed, now); len(objs) > 0 {
		driftAt = &now
		conds = append(append([]domain.Condition(nil), conds...), driftCondition(objs, now))
		if e.cfg.Drift != nil {
			for _, o := range objs {
				if err := e.cfg.Drift.Record(ctx, DriftRecord{Ref: ref, Object: o, At: now}); err != nil {
					e.cfg.Log.Error("record drift", slog.String("resource", ref.String()), slog.Any("err", err))
				}
			}
		}
	}
	if !out.NoStatus {
		gen := out.Generation
		err := e.cfg.Status.Apply(ctx, Report{Ref: ref, Generation: &gen, Phase: phase, Conditions: conds,
			Replace: true, DriftDetectedAt: driftAt, ReconciledAt: now})
		if err != nil && ctx.Err() == nil {
			e.cfg.Log.Error("write status", slog.String("resource", ref.String()), slog.Any("err", err))
			e.queue.AddRateLimited(ref, false)
			return
		}
	}
	if out.Requeue > 0 {
		e.queue.AddAfter(ref, out.Requeue)
	}
}

func (e *Engine) safeReconcile(ctx context.Context, c Controller, ref Ref) (out Outcome, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("controller %s panicked: %v", c.Kind(), r)
		}
	}()
	return c.Reconcile(ctx, ref)
}

// writeFailure reports a failed pass: Degraded (retrying) or Failed (terminal), with the failing
// condition's reason set to the AWS or Kubernetes error code. done are the conditions of the parts of
// the pass that succeeded (a controller may return them with its error); they are kept, so a team whose
// namespace is ready and whose IAM failed shows both.
func (e *Engine) writeFailure(ctx context.Context, ref Ref, out Outcome, err error, now time.Time) {
	if out.Phase == domain.PhaseDeleting {
		// A delete in progress stays Deleting; the finalizer wrote its own step conditions.
		if werr := e.cfg.Status.Apply(ctx, Report{Ref: ref, Phase: domain.PhaseDeleting, ReconciledAt: now}); werr != nil && ctx.Err() == nil {
			e.cfg.Log.Error("write failure status", slog.String("resource", ref.String()), slog.Any("err", werr))
		}
		return
	}
	done := out.Conditions
	condType, reason := reasonOf(err)
	phase := domain.PhaseDegraded
	if errors.Is(err, ErrTerminal) {
		phase = domain.PhaseFailed
	}
	conds := slices.DeleteFunc(slices.Clone(done), func(c domain.Condition) bool { return c.Type == condType || c.Type == "Ready" })
	conds = append(conds, domain.Condition{Type: condType, Status: domain.ConditionFalse, Reason: reason, Message: err.Error(), LastTransitionTime: now})
	if condType != "Ready" {
		conds = append(conds, domain.Condition{Type: "Ready", Status: domain.ConditionFalse, Reason: reason, Message: err.Error(), LastTransitionTime: now})
	}
	if werr := e.cfg.Status.Apply(ctx, Report{Ref: ref, Phase: phase, Conditions: conds, ReconciledAt: now}); werr != nil && ctx.Err() == nil {
		e.cfg.Log.Error("write failure status", slog.String("resource", ref.String()), slog.Any("err", werr))
	}
}

// Stats is a snapshot for metrics and readiness.
type Stats struct {
	Reconciled, Failed int64
	QueueDepth         int
	// OldestOverdue is how long the longest-waiting due item has waited (reconcile lag).
	OldestOverdue time.Duration
	LastPoll      time.Time
	LastResync    time.Time
}

// Stats returns a snapshot.
func (e *Engine) Stats() Stats {
	s := Stats{Reconciled: e.reconciled.Load(), Failed: e.failed.Load(), QueueDepth: e.queue.Len()}
	if due, ok := e.queue.OldestDue(); ok {
		s.OldestOverdue = e.cfg.Now().Sub(due)
	}
	if n := e.lastPoll.Load(); n != 0 {
		s.LastPoll = time.Unix(0, n)
	}
	if n := e.lastSync.Load(); n != 0 {
		s.LastResync = time.Unix(0, n)
	}
	return s
}

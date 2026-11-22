package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/adapter/observability"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/condmsg"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/engine"
	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/notify"
)

type idleFeed struct{}

func (idleFeed) Listen(ctx context.Context, h engine.FeedHandler) error {
	h.Connected()
	<-ctx.Done()
	return ctx.Err()
}

type fixedLister struct{ refs []engine.Ref }

func (l fixedLister) List(context.Context, []engine.Kind) ([]engine.Ref, error) { return l.refs, nil }
func (l fixedLister) Stale(context.Context, []engine.Kind) ([]engine.Ref, error) {
	return l.refs, nil
}

type noStatus struct{}

func (noStatus) Apply(context.Context, engine.Report) error { return nil }

// scriptedController fails on its first pass and succeeds afterwards.
type scriptedController struct {
	mu sync.Mutex
	n  int
}

func (*scriptedController) Kind() engine.Kind { return engine.KindTeam }
func (c *scriptedController) Reconcile(context.Context, engine.Ref) (engine.Outcome, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	if c.n == 1 {
		return engine.Outcome{}, errors.New("AccessDenied")
	}
	return engine.Outcome{Generation: 1}, nil
}

// FR-R06 AC2 (C-4): the engine built with the recorder cmd/afs-reconciler binds changes the registered
// duration, error and lag series on one successful and one failing reconcile.
func TestEngineRecorderIsBoundToMetrics(t *testing.T) {
	m := observability.NewMetrics("afs-reconciler", "test")
	reg, err := engine.NewRegistry(engine.Route{Table: "teams", Targets: []engine.Target{{Kind: engine.KindTeam, Self: true}}})
	if err != nil {
		t.Fatal(err)
	}
	ref := engine.NewRef(engine.KindTeam, "team_A", "team_A")
	e, err := engine.New(engine.Config{
		Feed: idleFeed{}, Lister: fixedLister{refs: []engine.Ref{ref}}, Registry: reg, Controllers: []engine.Controller{&scriptedController{}},
		Status: noStatus{}, Recorder: newReconcileMetrics(m),
		PollInterval: 10 * time.Millisecond, ResyncInterval: time.Hour,
		Backoff: engine.Backoff{Base: 5 * time.Millisecond, Max: 20 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Run(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for testutil.CollectAndCount(m.ReconcileDuration) == 0 || testutil.CollectAndCount(m.ReconcileLag) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out: duration and lag series never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := testutil.ToFloat64(m.ReconcileErrors.WithLabelValues("team")); got != 1 {
		t.Errorf("afs_reconcile_errors_total{kind=team} = %v, want 1", got)
	}
	if testutil.CollectAndCount(m.ReconcileDuration) != 1 {
		t.Error("afs_reconcile_duration_seconds has no team series")
	}
}

func TestReconcileMetricsDriftAndNotifications(t *testing.T) {
	m := observability.NewMetrics("afs-reconciler", "test")
	rm := newReconcileMetrics(m)
	rm.DriftCorrected(engine.KindTeam)
	rm.DriftCorrected(engine.KindTeam)
	rm.ReconcileLag(engine.KindTeam, 3*time.Second)
	var fr notify.FailureRecorder = rm
	fr.NotificationFailed("slack")
	if got := testutil.ToFloat64(m.DriftCorrected.WithLabelValues("team")); got != 2 {
		t.Errorf("drift = %v", got)
	}
	if got := testutil.ToFloat64(m.NotificationFailures.WithLabelValues("slack")); got != 1 {
		t.Errorf("notification failures = %v", got)
	}
	if testutil.CollectAndCount(m.ReconcileLag) != 1 {
		t.Error("lag not observed")
	}
}

// FR-R13: the condition sanitiser runs the observability scrubbers once the reconciler is wired.
func TestConditionScrubberIsInstalled(t *testing.T) {
	newReconcileMetrics(observability.NewMetrics("afs-reconciler", "test"))
	t.Cleanup(func() { condmsg.SetScrubber(nil) })
	got := condmsg.Sanitize("denied: Authorization: Bearer abcdef123456 for 111122223333")
	if strings.Contains(got, "abcdef123456") || strings.Contains(got, "111122223333") {
		t.Errorf("not scrubbed: %q", got)
	}
}

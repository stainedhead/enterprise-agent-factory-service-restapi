package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

type fakeRecorder struct {
	mu     sync.Mutex
	durs   map[Kind]int
	errs   map[Kind]int
	lags   map[Kind][]time.Duration
	drifts map[Kind]int
}

func newFakeRecorder() *fakeRecorder {
	return &fakeRecorder{durs: map[Kind]int{}, errs: map[Kind]int{}, lags: map[Kind][]time.Duration{}, drifts: map[Kind]int{}}
}

func (f *fakeRecorder) ReconcileDone(k Kind, _ time.Duration, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.durs[k]++
	if err != nil {
		f.errs[k]++
	}
}

func (f *fakeRecorder) ReconcileLag(k Kind, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lags[k] = append(f.lags[k], d)
}

func (f *fakeRecorder) DriftCorrected(k Kind) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drifts[k]++
}

func (f *fakeRecorder) snap() (durs, errs, lags, drifts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.durs[KindTeam], f.errs[KindTeam], len(f.lags[KindTeam]), f.drifts[KindTeam]
}

// FR-R06 AC1, AC2: one successful and one failing reconcile change the duration, error and lag series.
func TestRecorderSeesSuccessAndFailure(t *testing.T) {
	rec := newFakeRecorder()
	r := newRig(t, func(c *Config, _ *rig) { c.Recorder = rec })
	r.ctl.fn = func(_ context.Context, _ Ref, call int) (Outcome, error) {
		if call == 1 {
			return Outcome{}, errors.New("boom")
		}
		return Outcome{Generation: 2}, nil
	}
	r.start(t)
	r.feed.send(Change{"teams", "team_A", "team_A"})
	eventually(t, "success after failure", func() bool { d, _, l, _ := rec.snap(); return d >= 2 && l >= 1 })
	d, e, l, _ := rec.snap()
	if d != 2 || e != 1 {
		t.Errorf("durations=%d errors=%d, want 2 and 1", d, e)
	}
	if l != 1 {
		t.Errorf("lag observations = %d, want 1 (only the successful pass observes the generation)", l)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.lags[KindTeam][0] < 0 {
		t.Errorf("negative lag %v", rec.lags[KindTeam][0])
	}
}

// A resync alone is not a desired-state change, so it records no lag.
func TestResyncRecordsNoLag(t *testing.T) {
	rec := newFakeRecorder()
	r := newRig(t, func(c *Config, rg *rig) { c.Recorder = rec; rg.lister.all = []Ref{teamRef} })
	r.start(t)
	eventually(t, "resync reconcile", func() bool { d, _, _, _ := rec.snap(); return d >= 1 })
	if _, _, l, _ := rec.snap(); l != 0 {
		t.Errorf("lag observations = %d, want 0", l)
	}
}

// FR-R06 AC1 (C-2): a corrected drift increments the counter once per object.
func TestRecorderCountsCorrectedDrift(t *testing.T) {
	rec := newFakeRecorder()
	r := newRig(t, func(c *Config, _ *rig) { c.Recorder = rec })
	obj := ObjectName("Namespace", "", "team-a")
	r.ctl.fn = func(context.Context, Ref, int) (Outcome, error) {
		return Outcome{Generation: 1, Changed: []string{obj}}, nil
	}
	r.start(t)
	r.e.NoteDrift(teamRef, obj)
	eventually(t, "drift counted", func() bool { _, _, _, dr := rec.snap(); return dr >= 1 })
	if _, _, _, dr := rec.snap(); dr != 1 {
		t.Errorf("drift = %d, want 1", dr)
	}
}

// A pass that changed nothing flagged is not drift.
func TestRecorderNoDriftWhenNothingCorrected(t *testing.T) {
	rec := newFakeRecorder()
	r := newRig(t, func(c *Config, _ *rig) { c.Recorder = rec })
	r.start(t)
	r.e.NoteDrift(teamRef, "Namespace/x")
	eventually(t, "reconciled", func() bool { d, _, _, _ := rec.snap(); return d >= 1 })
	if _, _, _, dr := rec.snap(); dr != 0 {
		t.Errorf("drift = %d, want 0", dr)
	}
}

// FR-R13: the Degraded/Failed conditions carry a sanitised message; the full error is logged.
func TestFailureConditionMessageIsSanitised(t *testing.T) {
	var logs bytes.Buffer
	r := newRig(t, func(c *Config, _ *rig) { c.Log = slog.New(slog.NewTextHandler(&logs, nil)) })
	r.ctl.fn = func(context.Context, Ref, int) (Outcome, error) {
		return Outcome{}, WithCondition(domain.CondIamReady, &CodedError{Code: "AccessDenied",
			Err: errors.New("User: arn:aws:sts::111122223333:assumed-role/afs-reconciler/s is not authorized " + strings.Repeat("y", 900))})
	}
	r.start(t)
	r.feed.send(Change{"teams", "team_A", "team_A"})
	eventually(t, "failure report", func() bool { return len(r.status.all()) > 0 })
	rep := r.status.all()[0]
	if len(rep.Conditions) < 2 {
		t.Fatalf("conditions %+v", rep.Conditions)
	}
	for _, c := range rep.Conditions {
		if c.Message == "" {
			continue
		}
		if strings.Contains(c.Message, "111122223333") || strings.Contains(c.Message, "arn:aws") || len(c.Message) > 512 {
			t.Errorf("%s message not sanitised: %q", c.Type, c.Message)
		}
	}
	r.stop()
	if !strings.Contains(logs.String(), "111122223333") {
		t.Error("the full error must reach the reconciler log")
	}
}

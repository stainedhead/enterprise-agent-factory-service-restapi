package traffic_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/usecase/reconcile/collector/traffic"
)

type fakeThreshold struct {
	n   int
	err error
}

func (f fakeThreshold) DeniedSpikeThreshold(context.Context) (int, error) { return f.n, f.err }

type fakeEvents struct {
	published  []traffic.Spike
	emitted    map[string]bool
	publishErr error
	emittedErr error
}

func (e *fakeEvents) Emitted(_ context.Context, id string, w time.Time) (bool, error) {
	return e.emitted[id+"@"+w.Format(time.RFC3339)], e.emittedErr
}

func (e *fakeEvents) Publish(_ context.Context, s traffic.Spike) error {
	if e.publishErr != nil {
		return e.publishErr
	}
	e.published = append(e.published, s)
	return nil
}

func deniedRow(agent, pool string, bucket time.Time, host string, n int) traffic.Row {
	return traffic.Row{BucketStart: bucket, TeamID: team, AgentID: agent, PoolID: pool, Host: host, Port: 443, Verdict: traffic.VerdictDenied,
		DeniedReason: traffic.ReasonNotInCat, Connections: n}
}

var win0 = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

func detector(ev *fakeEvents, th fakeThreshold) *traffic.SpikeDetector {
	return traffic.NewSpikeDetector(traffic.SpikeConfig{Threshold: th, Events: ev, Now: func() time.Time { return win0.Add(time.Minute) }})
}

// RC-US-5.3: more than the threshold of denied connections in one window gives one event for that
// window; at the threshold or below, none; a new window starts a new count.
func TestSpikeOncePerWindow(t *testing.T) {
	ev := &fakeEvents{}
	d := detector(ev, fakeThreshold{n: 20})
	ctx := t.Context()

	d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", win0, "a.example.org", 20)})
	if len(ev.published) != 0 {
		t.Fatalf("exactly the threshold is not above it: %+v", ev.published)
	}
	d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", win0, "b.example.org", 1)})
	if len(ev.published) != 1 {
		t.Fatalf("published = %+v", ev.published)
	}
	s := ev.published[0]
	if s.AgentID != agtA || s.PoolID != "" || s.TeamID != team || !s.WindowStart.Equal(win0) || s.Denied != 21 || s.Threshold != 20 ||
		len(s.Hosts) != 2 || s.Hosts[0] != "a.example.org" {
		t.Errorf("spike = %+v", s)
	}
	// More denials in the same window, in this flush and the next: still one event.
	d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", win0, "c.example.org", 500)})
	d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", win0, "c.example.org", 500)})
	if len(ev.published) != 1 {
		t.Fatalf("a second event for the same window: %d", len(ev.published))
	}
	// The next window counts from zero and fires on its own.
	next := win0.Add(traffic.BucketSize)
	d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", next, "a.example.org", 20)})
	if len(ev.published) != 1 {
		t.Fatal("the new window is at the threshold only")
	}
	d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", next, "a.example.org", 1)})
	if len(ev.published) != 2 || !ev.published[1].WindowStart.Equal(next) {
		t.Fatalf("published = %+v", ev.published)
	}
}

func TestSpikeSubjects(t *testing.T) {
	ev := &fakeEvents{}
	d := detector(ev, fakeThreshold{n: 5})
	ctx := t.Context()
	other := "agt_01JD00000000000000000000AA"
	pw := func(host string, n int) traffic.Row {
		r := deniedRow("", pool, win0, host, n)
		r.WorkerPod = "w-" + host[:1]
		return r
	}
	allowed := deniedRow(agtA, "", win0, "ok.example.org", 100)
	allowed.Verdict, allowed.DeniedReason = traffic.VerdictAllowed, ""
	d.Observe(ctx, []traffic.Row{allowed, deniedRow(agtA, "", win0, "x.example.org", 3), deniedRow(other, "", win0, "x.example.org", 3)})
	if len(ev.published) != 0 {
		t.Fatalf("allowed traffic and other agents must not count: %+v", ev.published)
	}
	// A pool counts all its workers together.
	d.Observe(ctx, []traffic.Row{pw("a.example.org", 3), pw("b.example.org", 3)})
	if len(ev.published) != 1 || ev.published[0].PoolID != pool || ev.published[0].AgentID != "" || ev.published[0].Denied != 6 {
		t.Fatalf("published = %+v", ev.published)
	}
}

func TestSpikeHostsAreBounded(t *testing.T) {
	ev := &fakeEvents{}
	d := detector(ev, fakeThreshold{n: 1})
	var rows []traffic.Row
	for _, h := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		rows = append(rows, deniedRow(agtA, "", win0, h+".example.org", 1))
	}
	d.Observe(t.Context(), rows)
	if len(ev.published) != 1 || len(ev.published[0].Hosts) != 5 {
		t.Fatalf("published = %+v", ev.published)
	}
}

func TestSpikeEdgeCases(t *testing.T) {
	ctx := t.Context()
	t.Run("an unreadable threshold uses the default", func(t *testing.T) {
		ev := &fakeEvents{}
		d := detector(ev, fakeThreshold{err: errors.New("db down")})
		d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", win0, "a.example.org", traffic.DefaultSpikeThreshold)})
		if len(ev.published) != 0 {
			t.Fatal("at the default is not above it")
		}
		d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", win0, "a.example.org", 1)})
		if len(ev.published) != 1 || ev.published[0].Threshold != traffic.DefaultSpikeThreshold {
			t.Fatalf("published = %+v", ev.published)
		}
	})
	t.Run("a spike announced before a restart is not announced again", func(t *testing.T) {
		ev := &fakeEvents{emitted: map[string]bool{agtA + "@" + win0.Format(time.RFC3339): true}}
		d := detector(ev, fakeThreshold{n: 1})
		d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", win0, "a.example.org", 50)})
		d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", win0, "a.example.org", 50)})
		if len(ev.published) != 0 {
			t.Fatalf("published = %+v", ev.published)
		}
	})
	t.Run("a failed publish is retried by the next flush", func(t *testing.T) {
		ev := &fakeEvents{publishErr: errors.New("db down")}
		d := detector(ev, fakeThreshold{n: 1})
		d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", win0, "a.example.org", 5)})
		ev.publishErr = nil
		d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", win0, "a.example.org", 1)})
		if len(ev.published) != 1 || ev.published[0].Denied != 6 {
			t.Fatalf("published = %+v", ev.published)
		}
	})
	t.Run("a failed lookup of earlier announcements is retried", func(t *testing.T) {
		ev := &fakeEvents{emittedErr: errors.New("db down")}
		d := detector(ev, fakeThreshold{n: 1})
		d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", win0, "a.example.org", 5)})
		if len(ev.published) != 0 {
			t.Fatal("must not publish blind")
		}
		ev.emittedErr = nil
		d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", win0, "a.example.org", 1)})
		if len(ev.published) != 1 {
			t.Fatalf("published = %+v", ev.published)
		}
	})
	t.Run("old windows are forgotten", func(t *testing.T) {
		ev := &fakeEvents{}
		d := detector(ev, fakeThreshold{n: 3})
		old := win0.Add(-time.Hour)
		d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", old, "a.example.org", 2)})
		// The old window was pruned, so its next rows start from zero.
		d.Observe(ctx, []traffic.Row{deniedRow(agtA, "", old, "a.example.org", 2)})
		if len(ev.published) != 0 {
			t.Fatalf("published = %+v", ev.published)
		}
	})
	t.Run("default clock and logger", func(t *testing.T) {
		d := traffic.NewSpikeDetector(traffic.SpikeConfig{Threshold: fakeThreshold{n: 1}, Events: &fakeEvents{}})
		d.Observe(ctx, nil)
	})
}

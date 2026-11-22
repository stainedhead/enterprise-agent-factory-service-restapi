package collab

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/stainedhead/enterprise-agent-factory-service-restapi/internal/domain"
)

// Spike windows (RC-27): the last five minutes, at most one event per team per 15 minutes.
const (
	SpikeWindow   = 5 * time.Minute
	SpikeCooldown = 15 * time.Minute
)

// Denied is a team's denied requests in a window, by reason.
type Denied struct {
	TeamID                                                      string
	Unauthenticated, Forbidden, SurfaceDisabled, SecretDetected int
}

// Total is the sum of the four denied outcomes.
func (d Denied) Total() int {
	return d.Unauthenticated + d.Forbidden + d.SurfaceDisabled + d.SecretDetected
}

// AccessSource reads collaboration_access and the settings and remembers the events.
type AccessSource interface {
	// DeniedSince sums the denied outcomes per team from the five-minute buckets starting at or after since.
	DeniedSince(ctx context.Context, since time.Time) ([]Denied, error)
	// Threshold is collab.denied_spike_threshold.
	Threshold(ctx context.Context) (int, error)
	// LastSpike returns when a spike of the team was last announced (zero when never).
	LastSpike(ctx context.Context, teamID string) (time.Time, error)
}

// SpikeDetector announces collaboration.denied_spike.
type SpikeDetector struct {
	Source AccessSource
	Events Events
	Now    func() time.Time

	mu   sync.Mutex
	sent map[string]time.Time
}

// Run checks once. The cooldown is kept in memory and, after a leader change, rebuilt from the last
// event in the outbox, so a failover repeats at most one event.
func (d *SpikeDetector) Run(ctx context.Context) error {
	now := d.Now()
	thr, err := d.Source.Threshold(ctx)
	if err != nil {
		return fmt.Errorf("collab: spike threshold: %w", err)
	}
	start := BucketFloor(now.Add(-SpikeWindow))
	rows, err := d.Source.DeniedSince(ctx, start)
	if err != nil {
		return fmt.Errorf("collab: denied counts: %w", err)
	}
	d.mu.Lock()
	if d.sent == nil {
		d.sent = map[string]time.Time{}
	}
	d.mu.Unlock()
	for _, r := range rows {
		if r.Total() <= thr {
			continue
		}
		last, err := d.lastSent(ctx, r.TeamID)
		if err != nil {
			return err
		}
		if now.Sub(last) < SpikeCooldown {
			continue
		}
		err = d.Events.Publish(ctx, domain.EventCollaborationDeniedSpike, r.TeamID, string(KindTeamCollaboration), r.TeamID, "",
			map[string]any{"teamId": r.TeamID, "windowStart": start.UTC().Format(time.RFC3339), "windowEnd": start.Add(SpikeWindow).UTC().Format(time.RFC3339),
				"denied": r.Total(), "threshold": thr, "byReason": map[string]any{"unauthenticated": r.Unauthenticated,
					"forbidden": r.Forbidden, "surface_disabled": r.SurfaceDisabled, "secret_detected": r.SecretDetected}})
		if err != nil {
			return err
		}
		d.mu.Lock()
		d.sent[r.TeamID] = now
		d.mu.Unlock()
	}
	return nil
}

func (d *SpikeDetector) lastSent(ctx context.Context, team string) (time.Time, error) {
	d.mu.Lock()
	t, ok := d.sent[team]
	d.mu.Unlock()
	if ok {
		return t, nil
	}
	t, err := d.Source.LastSpike(ctx, team)
	if err != nil {
		return time.Time{}, fmt.Errorf("collab: last spike: %w", err)
	}
	d.mu.Lock()
	d.sent[team] = t
	d.mu.Unlock()
	return t, nil
}

// BucketFloor rounds t down to a five-minute boundary (UTC).
func BucketFloor(t time.Time) time.Time {
	u := t.UTC().Unix()
	return time.Unix(u-u%300, 0).UTC()
}

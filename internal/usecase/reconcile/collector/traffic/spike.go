package traffic

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// DefaultSpikeThreshold is the denied connections per agent or pool and 5-minute window above which a
// spike is announced, when the setting cannot be read (egress.denied_spike_threshold).
const DefaultSpikeThreshold = 20

// maxSpikeHosts bounds the hosts named in a spike event.
const maxSpikeHosts = 5

// Spike is one egress.denied_spike: the denied connections of one agent or pool in one window.
type Spike struct {
	TeamID string
	// AgentID or PoolID names the subject, never both.
	AgentID string
	PoolID  string
	// WindowStart is the start of the 5-minute window.
	WindowStart time.Time
	// Denied is the number of denied connections counted in the window when the threshold was crossed.
	Denied    int
	Threshold int
	// Hosts are some of the denied hosts (already redacted), sorted.
	Hosts []string
}

// ThresholdSource reads egress.denied_spike_threshold.
type ThresholdSource interface {
	DeniedSpikeThreshold(ctx context.Context) (int, error)
}

// SpikeEvents announces spikes.
type SpikeEvents interface {
	// Emitted reports whether a spike of the subject (agent or pool id) was already announced for the
	// window, so a restart does not announce it again.
	Emitted(ctx context.Context, subjectID string, window time.Time) (bool, error)
	// Publish writes the egress.denied_spike event.
	Publish(ctx context.Context, s Spike) error
}

// SpikeConfig configures a SpikeDetector.
type SpikeConfig struct {
	Threshold ThresholdSource
	Events    SpikeEvents
	Now       func() time.Time
	Log       *slog.Logger
}

type spikeKey struct {
	subject string
	window  time.Time
}

type spikeState struct {
	spike Spike
	fired bool
}

// SpikeDetector watches the stored rows and announces, once per agent or pool and window, when the
// denied connections exceed the threshold (RC-US-5.3). It is an Observer of the Collector.
type SpikeDetector struct {
	cfg SpikeConfig

	mu     sync.Mutex
	window map[spikeKey]*spikeState
}

// NewSpikeDetector returns a SpikeDetector.
func NewSpikeDetector(cfg SpikeConfig) *SpikeDetector {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &SpikeDetector{cfg: cfg, window: map[spikeKey]*spikeState{}}
}

var _ Observer = (*SpikeDetector)(nil)

// Observe implements Observer. A pool counts all its workers together.
func (d *SpikeDetector) Observe(ctx context.Context, rows []Row) {
	var touched []*spikeState
	d.mu.Lock()
	for _, r := range rows {
		if r.Verdict != VerdictDenied {
			continue
		}
		subject := r.AgentID
		if subject == "" {
			subject = r.PoolID
		}
		k := spikeKey{subject: subject, window: r.BucketStart}
		st := d.window[k]
		if st == nil {
			st = &spikeState{spike: Spike{TeamID: r.TeamID, AgentID: r.AgentID, PoolID: r.PoolID, WindowStart: r.BucketStart}}
			d.window[k] = st
		}
		st.spike.Denied += r.Connections
		if !contains(st.spike.Hosts, r.Host) && len(st.spike.Hosts) < maxSpikeHosts {
			st.spike.Hosts = append(st.spike.Hosts, r.Host)
		}
		touched = append(touched, st)
	}
	d.prune()
	d.mu.Unlock()
	if len(touched) == 0 {
		return
	}
	threshold, err := d.cfg.Threshold.DeniedSpikeThreshold(ctx)
	if err != nil || threshold < 1 {
		d.cfg.Log.Warn("egress.denied_spike_threshold unreadable, using the default", slog.Any("err", err))
		threshold = DefaultSpikeThreshold
	}
	seen := map[*spikeState]bool{}
	for _, st := range touched {
		if seen[st] {
			continue
		}
		seen[st] = true
		d.maybeFire(ctx, st, threshold)
	}
}

func (d *SpikeDetector) maybeFire(ctx context.Context, st *spikeState, threshold int) {
	d.mu.Lock()
	if st.fired || st.spike.Denied <= threshold {
		d.mu.Unlock()
		return
	}
	s := st.spike
	s.Threshold = threshold
	s.Hosts = append([]string(nil), s.Hosts...)
	sort.Strings(s.Hosts)
	// Claimed before publishing: a second flush must not announce it while this one is writing.
	st.fired = true
	d.mu.Unlock()

	subject := s.AgentID
	if subject == "" {
		subject = s.PoolID
	}
	done, err := d.cfg.Events.Emitted(ctx, subject, s.WindowStart)
	if err == nil && !done {
		err = d.cfg.Events.Publish(ctx, s)
	}
	if err != nil {
		// Not announced: let the next flush of the window try again.
		d.mu.Lock()
		st.fired = false
		d.mu.Unlock()
		d.cfg.Log.Warn("egress.denied_spike not published", slog.String("subject", subject), slog.Any("err", err))
	}
}

// prune forgets windows that ended more than one window ago. Callers hold d.mu.
func (d *SpikeDetector) prune() {
	cutoff := d.cfg.Now().UTC().Truncate(BucketSize).Add(-BucketSize)
	for k := range d.window {
		if k.window.Before(cutoff) {
			delete(d.window, k)
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

package edge

import (
	"context"
	"time"
)

// RunConfig sets the background loops of Run.
type RunConfig struct {
	// RefreshEvery reloads the route table; default 30 s.
	RefreshEvery time.Duration
	// FlushEvery writes the buffered access counts; default 30 s.
	FlushEvery time.Duration
	// Changes triggers an immediate refresh (a database notification); bursts are merged.
	Changes <-chan struct{}
	// OnError sees a failed refresh or flush; may be nil.
	OnError func(op string, err error)
}

// Run keeps the route table fresh and the access counts flowing until ctx ends, then flushes once more so a
// rolling update loses nothing (EDGE-7).
func (s *Service) Run(ctx context.Context, c RunConfig) {
	if c.RefreshEvery <= 0 {
		c.RefreshEvery = 30 * time.Second
	}
	if c.FlushEvery <= 0 {
		c.FlushEvery = 30 * time.Second
	}
	report := func(op string, err error) {
		if err != nil && c.OnError != nil {
			c.OnError(op, err)
		}
	}
	refresh := time.NewTicker(c.RefreshEvery)
	flush := time.NewTicker(c.FlushEvery)
	defer refresh.Stop()
	defer flush.Stop()
	report("refresh", s.Refresh(ctx))
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			report("flush", s.Flush(fctx))
			cancel()
			return
		case <-refresh.C:
			report("refresh", s.Refresh(ctx))
		case <-c.Changes:
			report("refresh", s.Refresh(ctx))
			// Merge a burst: drop what queued while the refresh ran.
			for len(c.Changes) > 0 {
				<-c.Changes
			}
		case <-flush.C:
			report("flush", s.Flush(ctx))
		}
	}
}

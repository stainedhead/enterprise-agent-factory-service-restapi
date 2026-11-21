package engine

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Job is a periodic task that runs only while this reconciler is the leader (the engine runs only
// then).
type Job struct {
	Name  string
	Every time.Duration
	// Immediate runs the job once at start, before the first interval elapses.
	Immediate bool
	Fn        func(ctx context.Context) error
}

// Every registers a periodic job. Jobs must be registered before Run.
func (e *Engine) Every(name string, every time.Duration, fn func(ctx context.Context) error) {
	e.AddJob(Job{Name: name, Every: every, Fn: fn})
}

// AddJob registers a periodic job. It must be called before Run.
func (e *Engine) AddJob(j Job) {
	e.mu.Lock()
	e.jobs = append(e.jobs, j)
	e.mu.Unlock()
}

func (e *Engine) runJobs(ctx context.Context, wg *sync.WaitGroup) {
	e.mu.Lock()
	jobs := append([]Job(nil), e.jobs...)
	e.mu.Unlock()
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if j.Immediate {
				e.runJob(ctx, j)
			}
			t := time.NewTicker(j.Every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					e.runJob(ctx, j)
				}
			}
		}()
	}
}

// runJob runs one pass; a failure or panic is logged and the job runs again next interval.
func (e *Engine) runJob(ctx context.Context, j Job) {
	defer func() {
		if r := recover(); r != nil {
			e.cfg.Log.Error("job panicked", "job", j.Name, "panic", r)
		}
	}()
	if err := j.Fn(ctx); err != nil && ctx.Err() == nil {
		e.cfg.Log.Warn("job failed", slog.String("job", j.Name), slog.Any("err", err))
	}
}

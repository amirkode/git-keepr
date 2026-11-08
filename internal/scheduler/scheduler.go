// Package scheduler runs jobs on fixed intervals inside the process: one
// goroutine per job (so a job never overlaps itself) and a semaphore that
// bounds how many jobs run at once.
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Job is a unit of recurring work.
type Job struct {
	ID       string
	Interval time.Duration
	Run      func(ctx context.Context)
}

// Options controls Run.
type Options struct {
	// MaxConcurrency bounds simultaneously running jobs (minimum 1).
	MaxConcurrency int
	// OnStart runs every job immediately; otherwise the first run happens after one interval.
	OnStart bool
	Log     *slog.Logger
}

// Run blocks until ctx is cancelled and every in-flight job has returned.
//
// A job that takes longer than its interval simply starts its next run as soon
// as the previous one finished; runs of the same job never overlap.
func Run(ctx context.Context, jobs []Job, opts Options) {
	if opts.MaxConcurrency < 1 {
		opts.MaxConcurrency = 1
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	sem := make(chan struct{}, opts.MaxConcurrency)
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop(ctx, j, opts.OnStart, sem, log)
		}()
	}
	wg.Wait()
}

func loop(ctx context.Context, j Job, onStart bool, sem chan struct{}, log *slog.Logger) {
	next := time.Now().Add(j.Interval)
	if onStart {
		next = time.Now()
	}
	for {
		if d := time.Until(next); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		start := time.Now()
		runSafely(ctx, j, log)
		<-sem
		next = start.Add(j.Interval)
		if ctx.Err() != nil {
			return
		}
	}
}

func runSafely(ctx context.Context, j Job, log *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("job panicked", "repo_id", j.ID, "panic", r)
		}
	}()
	j.Run(ctx)
}

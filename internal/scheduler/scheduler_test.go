package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSyncOnStartRunsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var runs atomic.Int32
	done := make(chan struct{})
	go func() {
		Run(ctx, []Job{{ID: "a", Interval: time.Hour, Run: func(context.Context) { runs.Add(1) }}}, Options{MaxConcurrency: 1, OnStart: true})
		close(done)
	}()
	waitFor(t, func() bool { return runs.Load() == 1 })
	cancel()
	<-done
	if runs.Load() != 1 {
		t.Fatalf("runs = %d", runs.Load())
	}
}

func TestNoSyncOnStartWaitsOneInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs atomic.Int32
	go Run(ctx, []Job{{ID: "a", Interval: 150 * time.Millisecond, Run: func(context.Context) { runs.Add(1) }}}, Options{MaxConcurrency: 1})
	time.Sleep(60 * time.Millisecond)
	if runs.Load() != 0 {
		t.Fatal("must not run before first interval when OnStart=false")
	}
	waitFor(t, func() bool { return runs.Load() >= 1 })
}

func TestRepeatsAtInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var runs atomic.Int32
	go Run(ctx, []Job{{ID: "a", Interval: 30 * time.Millisecond, Run: func(context.Context) { runs.Add(1) }}}, Options{MaxConcurrency: 1, OnStart: true})
	waitFor(t, func() bool { return runs.Load() >= 4 })
}

func TestNoOverlapForSameJobAndBoundedConcurrency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	running := map[string]int{}
	var total, maxTotal, overlaps int
	mk := func(id string) Job {
		return Job{ID: id, Interval: 5 * time.Millisecond, Run: func(context.Context) {
			mu.Lock()
			running[id]++
			total++
			if running[id] > 1 {
				overlaps++
			}
			if total > maxTotal {
				maxTotal = total
			}
			mu.Unlock()
			time.Sleep(25 * time.Millisecond) // longer than the interval
			mu.Lock()
			running[id]--
			total--
			mu.Unlock()
		}}
	}
	done := make(chan struct{})
	go func() {
		Run(ctx, []Job{mk("a"), mk("b"), mk("c"), mk("d")}, Options{MaxConcurrency: 2, OnStart: true})
		close(done)
	}()
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if overlaps != 0 {
		t.Fatalf("a job overlapped itself %d times", overlaps)
	}
	if maxTotal > 2 || maxTotal == 0 {
		t.Fatalf("max concurrency = %d, want 1..2", maxTotal)
	}
}

func TestCancelWaitsForInFlightJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var finished atomic.Bool
	done := make(chan struct{})
	go func() {
		Run(ctx, []Job{{ID: "a", Interval: time.Hour, Run: func(ctx context.Context) {
			close(started)
			<-ctx.Done()
			time.Sleep(50 * time.Millisecond)
			finished.Store(true)
		}}}, Options{MaxConcurrency: 1, OnStart: true})
		close(done)
	}()
	<-started
	cancel()
	<-done
	if !finished.Load() {
		t.Fatal("Run returned before the in-flight job finished")
	}
}

func TestPanicIsContained(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var healthy atomic.Int32
	go Run(ctx, []Job{
		{ID: "bad", Interval: 20 * time.Millisecond, Run: func(context.Context) { panic("boom") }},
		{ID: "good", Interval: 20 * time.Millisecond, Run: func(context.Context) { healthy.Add(1) }},
	}, Options{MaxConcurrency: 2, OnStart: true})
	waitFor(t, func() bool { return healthy.Load() >= 3 })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

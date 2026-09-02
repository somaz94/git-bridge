package task

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// The point of the group is that shutdown can wait for work the handler already
// answered "accepted" for.
func TestGroupWaitBlocksUntilWorkFinishes(t *testing.T) {
	g := NewGroup(context.Background())

	var finished atomic.Bool
	release := make(chan struct{})
	g.Go(func(context.Context) {
		<-release
		finished.Store(true)
	})

	close(release)
	g.Wait()

	if !finished.Load() {
		t.Error("Wait() returned before the work finished")
	}
}

// Work gets the group's context, not the caller's: that is what keeps a sync
// alive after the request that started it is gone.
func TestGroupPassesItsContextToWork(t *testing.T) {
	type ctxKey string
	const key ctxKey = "k"

	g := NewGroup(context.WithValue(context.Background(), key, "v"))

	got := make(chan any, 1)
	g.Go(func(ctx context.Context) { got <- ctx.Value(key) })
	g.Wait()

	if v := <-got; v != "v" {
		t.Errorf("work context value = %v, want %q", v, "v")
	}
}

// A group nobody used must not make shutdown wait.
func TestGroupWaitReturnsImmediatelyWhenIdle(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewGroup(context.Background()).Wait()
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Wait() blocked on an idle group")
	}
}

func TestWaitAllReportsTrueWhenEverythingFinishesInTime(t *testing.T) {
	var ran atomic.Int32
	ok := WaitAll(2*time.Second,
		func() { ran.Add(1) },
		func() { ran.Add(1) },
	)

	if !ok {
		t.Error("WaitAll() = false, want true when the waits finish in time")
	}
	if n := ran.Load(); n != 2 {
		t.Errorf("waits run = %d, want 2", n)
	}
}

// The bound is the whole point: one stuck wait must not hold shutdown open, and
// the false return is what tells the caller to stop waiting and start cancelling.
func TestWaitAllReportsFalseWhenAWaitOutlastsTheTimeout(t *testing.T) {
	blocked := make(chan struct{})
	defer close(blocked)

	start := time.Now()
	ok := WaitAll(50*time.Millisecond,
		func() {},
		func() { <-blocked },
	)

	if ok {
		t.Error("WaitAll() = true, want false when a wait is still running")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("WaitAll() took %v, want it to give up near the timeout", elapsed)
	}
}

// No waits is the degenerate case of "nothing in flight", not an error.
func TestWaitAllWithNoWaitsSucceeds(t *testing.T) {
	if !WaitAll(time.Second) {
		t.Error("WaitAll() = false with no waits, want true")
	}
}

// Package task tracks detached background work so shutdown can wait for it.
//
// A mirror sync outlives the request that triggered it: the webhook and retry
// handlers answer "accepted" immediately and let the sync run on its own. Those
// goroutines used to be untracked, so SIGTERM tore them down mid-git-command —
// the process group is killed on context cancellation, which is how a fetch can
// die leaving an abandoned pack .keep marker behind that then pins its packfile
// out of every later repack.
package task

import (
	"context"
	"sync"
	"time"
)

// Group runs background work under a shared context and counts what is still
// in flight.
//
// The context is deliberately separate from the one that stops the listeners.
// Shutdown closes the door first and only then waits, so the drain window is
// spent finishing work instead of racing new arrivals.
type Group struct {
	ctx context.Context
	wg  sync.WaitGroup
}

// NewGroup returns a Group whose work runs under ctx.
func NewGroup(ctx context.Context) *Group {
	return &Group{ctx: ctx}
}

// Go runs fn in its own goroutine, passing the group's context, and counts it
// as in flight until fn returns.
func (g *Group) Go(fn func(context.Context)) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		fn(g.ctx)
	}()
}

// Wait blocks until every goroutine started by Go has returned.
func (g *Group) Wait() {
	g.wg.Wait()
}

// WaitAll waits for every wait function to return, and reports whether they all
// did so within timeout.
//
// It exists because shutdown has to bound the wait: a git operation that hangs
// must not hold the pod past its termination grace period, where the kubelet
// would SIGKILL it anyway and the caller would lose the chance to log why.
// A false return is the signal to cancel the work context and stop being
// polite.
func WaitAll(timeout time.Duration, waits ...func()) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, wait := range waits {
			wg.Add(1)
			go func() {
				defer wg.Done()
				wait()
			}()
		}
		wg.Wait()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

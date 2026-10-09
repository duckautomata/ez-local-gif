package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// flight de-duplicates concurrent calls by key (a stdlib-only singleflight):
// the first caller of a key runs fn, later callers with the same key wait
// for its result. Scrubbing a preview fires many stills whose autocrop
// detection would otherwise run once per request until the first one has
// written the memo; identical still/proxy requests likewise share one
// ffmpeg run.
type flight[T any] struct {
	mu    sync.Mutex
	calls map[string]*flightCall[T]
}

// flightCall is one in-progress fn.
type flightCall[T any] struct {
	done chan struct{}
	val  T
	err  error

	// Abandon-aware runs (doDetachedAbandon) count their waiters: when the
	// count stays at zero for the grace period the leader's ctx is
	// cancelled. All three are guarded by flight.mu.
	waiters int
	cancel  context.CancelFunc
	timer   *time.Timer
}

// do returns fn's result for key, sharing one run among concurrent callers.
// fn runs under the leader's ctx; a waiter whose own ctx ends returns its
// ctx.Err() at once, and a waiter that sees the leader's run fail with a
// context error while its own ctx is still alive (the leader's request was
// aborted or timed out) runs fn itself, so one abandoned request never
// fails the others.
func (g *flight[T]) do(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, error) {
	for {
		val, err := g.once(ctx, key, fn, false)
		if err != nil && isContextError(err) && ctx.Err() == nil {
			continue
		}
		return val, err
	}
}

// doDetached is do for a run that must outlive the request that started
// it: fn runs in its own goroutine under context.WithoutCancel(ctx) — the
// leader's cancellation never reaches it, so fn must bound itself — and
// every caller, the leader included, waits for its result or for its own
// ctx to end (returning ctx.Err(), while the run carries on for the callers
// still waiting and for whoever asks next). An autocrop detection is run
// this way: a superseded still request or the still deadline used to kill a
// pass the next request then restarted from zero, so a detection longer
// than one request never completed through the preview path.
func (g *flight[T]) doDetached(ctx context.Context, key string, fn func(context.Context) (T, error)) (T, error) {
	return g.once(ctx, key, fn, true)
}

// once joins the in-progress call for key or starts one (synchronously in
// the leader, or in a detached goroutine — see doDetached).
func (g *flight[T]) once(ctx context.Context, key string, fn func(context.Context) (T, error), detached bool) (T, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[string]*flightCall[T]{}
	}
	c, joined := g.calls[key]
	if !joined {
		c = &flightCall[T]{done: make(chan struct{})}
		g.calls[key] = c
	}
	g.mu.Unlock()
	switch {
	case joined:
		// A waiter.
	case detached:
		go func() {
			defer func() {
				// A panic in a detached run must not take the process down;
				// waiters see an error instead.
				if r := recover(); r != nil {
					c.err = fmt.Errorf("jobs: in-flight call panicked: %v", r)
				}
				g.mu.Lock()
				delete(g.calls, key)
				g.mu.Unlock()
				close(c.done)
			}()
			c.val, c.err = fn(context.WithoutCancel(ctx))
		}()
	default:
		finished := false
		defer func() {
			// Runs on a panic in fn as well, so waiters never hang: they see
			// an error while the panic propagates to the leader's caller.
			if !finished {
				c.err = errors.New("jobs: in-flight call did not complete")
			}
			g.mu.Lock()
			delete(g.calls, key)
			g.mu.Unlock()
			close(c.done)
		}()
		c.val, c.err = fn(ctx)
		finished = true
		return c.val, c.err
	}
	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// doDetachedAbandon is doDetached for a run nobody should pay for once
// nobody wants it: fn runs in its own goroutine under a cancellable child
// of context.WithoutCancel(ctx), every caller waits for its result or for
// its own ctx to end, and when the call has had NO waiter for grace the
// leader's ctx is cancelled (fn then returns a context error to no one; the
// next caller starts a fresh run). A polling client that re-joins inside
// the grace keeps the run alive. An AI matte pass runs this way (Phase 5b):
// a 20-minute CPU pass that every waiter has abandoned must not run to the
// end, while an autocrop detection (doDetached) must. Like do, a waiter
// whose own ctx is still alive but sees the run end with a context error
// (cancelled by the abandon timer a moment before it joined) runs fn
// again rather than failing.
func (g *flight[T]) doDetachedAbandon(ctx context.Context, key string, grace time.Duration, fn func(context.Context) (T, error)) (T, error) {
	for {
		val, err := g.abandonable(ctx, key, grace, fn)
		if err != nil && isContextError(err) && ctx.Err() == nil {
			continue
		}
		return val, err
	}
}

// abandonable joins or starts the abandon-aware call for key and waits
// once (see doDetachedAbandon).
func (g *flight[T]) abandonable(ctx context.Context, key string, grace time.Duration, fn func(context.Context) (T, error)) (T, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[string]*flightCall[T]{}
	}
	c, joined := g.calls[key]
	if !joined {
		runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		c = &flightCall[T]{done: make(chan struct{}), cancel: cancel}
		g.calls[key] = c
		go func() {
			defer func() {
				if r := recover(); r != nil {
					c.err = fmt.Errorf("jobs: in-flight call panicked: %v", r)
				}
				g.mu.Lock()
				delete(g.calls, key)
				if c.timer != nil {
					c.timer.Stop()
					c.timer = nil
				}
				g.mu.Unlock()
				cancel()
				close(c.done)
			}()
			c.val, c.err = fn(runCtx)
		}()
	}
	c.waiters++
	if c.timer != nil {
		// A waiter came back inside the grace: the run stays.
		c.timer.Stop()
		c.timer = nil
	}
	g.mu.Unlock()
	defer g.leave(key, c, grace)
	select {
	case <-c.done:
		return c.val, c.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// join waits for the abandon-aware call for key that is ALREADY running —
// counting as one of its waiters, so the run stays alive while the caller
// waits and the abandon timer is armed again when the caller leaves — and
// reports joined false at once, starting nothing, when no call for key is
// in flight. It is the preview's way of following a matte pass that an
// eager request or a render started (Phase 5c: a plain still or proxy
// never starts a pass of its own, it only watches one that runs). Like
// abandonable, a waiter whose own ctx ends returns its ctx.Err(); a run
// that ends with a context error (the abandon timer fired) hands that
// error to the joiner, who treats it as nothing in flight.
func (g *flight[T]) join(ctx context.Context, key string, grace time.Duration) (val T, joined bool, err error) {
	g.mu.Lock()
	c, ok := g.calls[key]
	if !ok {
		g.mu.Unlock()
		return val, false, nil
	}
	c.waiters++
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	g.mu.Unlock()
	defer g.leave(key, c, grace)
	select {
	case <-c.done:
		return c.val, true, c.err
	case <-ctx.Done():
		return val, true, ctx.Err()
	}
}

// leave drops one waiter of c and, when it was the last while the run is
// still going, arms the abandon timer (a call started by do / doDetached
// has no cancel and is never cancelled this way).
func (g *flight[T]) leave(key string, c *flightCall[T], grace time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c.waiters--
	if c.waiters > 0 || g.calls[key] != c || c.cancel == nil {
		return
	}
	c.timer = time.AfterFunc(grace, func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if c.waiters == 0 && g.calls[key] == c {
			c.cancel()
		}
	})
}

// inFlight reports whether a call for key is running right now.
func (g *flight[T]) inFlight(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.calls[key]
	return ok
}

// isContextError reports whether err is (or wraps) a cancellation or
// deadline error.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

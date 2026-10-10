package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// isContextError reports whether err is (or wraps) a cancellation or
// deadline error.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

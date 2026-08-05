package lru

import (
	"context"
	"runtime"
	"sync"
)

// flightGroup suppresses duplicate in-flight calls for the same typed key.
type flightGroup[K comparable, V any] struct {
	mu    sync.Mutex
	calls map[K]*flightCall[V]
	// skipKeyCheck is set when K can never produce an invalid key. The zero
	// value validates, so an uninitialized group stays safe.
	skipKeyCheck bool
}

type flightCall[V any] struct {
	done chan struct{}
	// waiting counts follower goroutines blocked on done. Same-package
	// tests use it as a join barrier before releasing the leader.
	waiting    int
	val        V
	err        error
	panicked   bool
	panicValue any
	goexited   bool
}

func (g *flightGroup[K, V]) Do(key K, fn func() (V, error)) (V, error) {
	return g.do(nil, key, fn)
}

// DoContext behaves like Do, except a follower waiting for an existing call can
// stop waiting when ctx is canceled. The goroutine that starts the call remains
// responsible for running fn to completion.
func (g *flightGroup[K, V]) DoContext(ctx context.Context, key K, fn func() (V, error)) (V, error) {
	if ctx == nil {
		panic("lru: nil Context")
	}
	return g.do(ctx, key, fn)
}

func (g *flightGroup[K, V]) do(ctx context.Context, key K, fn func() (V, error)) (V, error) {
	if !g.skipKeyCheck {
		if err := validateKey(key); err != nil {
			var zero V
			return zero, err
		}
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			var zero V
			return zero, err
		}
	}

	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[K]*flightCall[V])
	}
	if c := g.calls[key]; c != nil {
		c.waiting++
		g.mu.Unlock()
		if ctx == nil {
			<-c.done
		} else {
			select {
			case <-c.done:
			case <-ctx.Done():
				g.mu.Lock()
				c.waiting--
				g.mu.Unlock()
				var zero V
				return zero, ctx.Err()
			}
		}
		if c.panicked {
			panic(c.panicValue)
		}
		if c.goexited {
			runtime.Goexit()
		}
		return c.val, c.err
	}

	c := &flightCall[V]{done: make(chan struct{})}
	g.calls[key] = c
	g.mu.Unlock()

	// normalReturn distinguishes fn returning from fn panicking, and recovered
	// distinguishes a recovered panic from runtime.Goexit, which cannot be
	// stopped and must not be reported to waiters as a successful zero result.
	normalReturn := false
	recovered := false

	defer func() {
		if !normalReturn && !recovered {
			c.goexited = true
		}

		// Release the waiters and drop the call under a single lock hold.
		// Signalling first would leave a window where a caller could still
		// find this call and join a fn that has already returned, contrary to
		// deduplicating only calls that are genuinely in flight.
		g.mu.Lock()
		close(c.done)
		delete(g.calls, key)
		g.mu.Unlock()

		if c.panicked {
			panic(c.panicValue)
		}
	}()

	func() {
		defer func() {
			if !normalReturn {
				if r := recover(); r != nil {
					c.panicked = true
					c.panicValue = r
				}
			}
		}()
		c.val, c.err = fn()
		normalReturn = true
	}()

	if !normalReturn {
		// Reaching this point distinguishes a recovered panic(nil) from
		// runtime.Goexit, which never resumes execution here on Go versions
		// where recover returns nil for a nil panic value.
		recovered = true
		if !c.panicked {
			c.panicked = true
			c.panicValue = nil
		}
	}
	return c.val, c.err
}

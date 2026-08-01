package lru

import (
	"runtime"
	"sync"
)

// flightGroup suppresses duplicate in-flight calls for the same typed key.
type flightGroup[K comparable, V any] struct {
	mu    sync.Mutex
	calls map[K]*flightCall[V]
}

type flightCall[V any] struct {
	wg         sync.WaitGroup
	val        V
	err        error
	panicked   bool
	panicValue any
	goexited   bool
}

func (g *flightGroup[K, V]) Do(key K, fn func() (V, error)) (V, error) {
	if err := validateKey(key); err != nil {
		var zero V
		return zero, err
	}

	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[K]*flightCall[V])
	}
	if c := g.calls[key]; c != nil {
		g.mu.Unlock()
		c.wg.Wait()
		if c.panicked {
			panic(c.panicValue)
		}
		if c.goexited {
			runtime.Goexit()
		}
		return c.val, c.err
	}

	c := &flightCall[V]{}
	c.wg.Add(1)
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
		c.wg.Done()

		g.mu.Lock()
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
		recovered = true
	}
	return c.val, c.err
}

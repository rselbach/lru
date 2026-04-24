package lru

import "sync"

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
}

func (g *flightGroup[K, V]) Do(key K, fn func() (V, error)) (V, error) {
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
		return c.val, c.err
	}

	c := &flightCall[V]{}
	c.wg.Add(1)
	g.calls[key] = c
	g.mu.Unlock()

	defer func() {
		if r := recover(); r != nil {
			c.panicked = true
			c.panicValue = r
		}
		c.wg.Done()

		g.mu.Lock()
		delete(g.calls, key)
		g.mu.Unlock()

		if c.panicked {
			panic(c.panicValue)
		}
	}()

	c.val, c.err = fn()
	return c.val, c.err
}

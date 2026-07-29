package lru

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlightGroup_GoexitPropagatesToWaiters(t *testing.T) {
	r := require.New(t)

	var g flightGroup[string, int]

	started := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan struct{})

	go func() {
		defer close(leaderDone)
		_, _ = g.Do("k", func() (int, error) {
			close(started)
			<-release
			runtime.Goexit()
			return 42, nil
		})
	}()

	<-started

	const waiters = 3
	var wg sync.WaitGroup
	returned := make([]bool, waiters)
	vals := make([]int, waiters)

	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			// Done must run via defer so it fires even when the waiter
			// goroutine exits through the propagated Goexit.
			defer wg.Done()
			v, _ := g.Do("k", func() (int, error) { return 99, nil })
			vals[i] = v
			returned[i] = true
		}(i)
	}

	// give the waiters a chance to join the in-flight call
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	<-leaderDone

	for i := 0; i < waiters; i++ {
		if returned[i] {
			// a waiter that returned raced ahead of joining and ran its own
			// compute; it must have that result, never the zero value of the
			// goexited call
			r.Equal(99, vals[i], "waiter %d returned the goexited call's zero value", i)
		}
	}

	// the goexited call must be forgotten so later calls compute fresh
	v, err := g.Do("k", func() (int, error) { return 7, nil })
	r.NoError(err)
	r.Equal(7, v)
}

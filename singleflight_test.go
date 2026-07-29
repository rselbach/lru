package lru

import (
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlightGroup_PanicPropagatesToLeader(t *testing.T) {
	r := require.New(t)

	var g flightGroup[string, int]

	r.PanicsWithValue("boom", func() {
		_, _ = g.Do("k", func() (int, error) { panic("boom") })
	})

	// the panicked call must be forgotten so later calls compute fresh
	v, err := g.Do("k", func() (int, error) { return 7, nil })
	r.NoError(err)
	r.Equal(7, v)
}

func TestFlightGroup_PanicPropagatesToWaiters(t *testing.T) {
	r := require.New(t)

	var g flightGroup[string, int]

	started := make(chan struct{})
	release := make(chan struct{})

	leaderPanic := make(chan any, 1)
	go func() {
		defer func() { leaderPanic <- recover() }()
		_, _ = g.Do("k", func() (int, error) {
			close(started)
			<-release
			panic("boom")
		})
	}()

	<-started

	const waiters = 3
	var wg sync.WaitGroup
	panics := make([]any, waiters)
	vals := make([]int, waiters)

	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { panics[i] = recover() }()
			v, _ := g.Do("k", func() (int, error) { return 99, nil })
			vals[i] = v
		}(i)
	}

	// give the waiters a chance to join the in-flight call
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	r.Equal("boom", <-leaderPanic)
	for i := 0; i < waiters; i++ {
		// a waiter either joined the panicked call and re-panicked, or raced
		// ahead of joining and computed its own result
		if panics[i] == nil {
			r.Equal(99, vals[i], "waiter %d", i)
			continue
		}
		r.Equal("boom", panics[i], "waiter %d", i)
	}
}

func TestFlightGroup_ErrorDeliveredToWaiters(t *testing.T) {
	r := require.New(t)

	var g flightGroup[string, int]

	wantErr := errors.New("compute failed")
	started := make(chan struct{})
	release := make(chan struct{})

	leaderErr := make(chan error, 1)
	go func() {
		_, err := g.Do("k", func() (int, error) {
			close(started)
			<-release
			return 0, wantErr
		})
		leaderErr <- err
	}()

	<-started

	const waiters = 3
	var wg sync.WaitGroup
	errs := make([]error, waiters)
	computed := make([]bool, waiters)

	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := g.Do("k", func() (int, error) {
				computed[i] = true
				return 99, nil
			})
			errs[i] = err
		}(i)
	}

	// give the waiters a chance to join the in-flight call
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	r.ErrorIs(<-leaderErr, wantErr)
	for i := 0; i < waiters; i++ {
		// a waiter that ran its own compute raced ahead of joining
		if computed[i] {
			r.NoError(errs[i], "waiter %d", i)
			continue
		}
		r.ErrorIs(errs[i], wantErr, "waiter %d", i)
	}
}

func TestFlightGroup_RetryAfterErrorRecomputes(t *testing.T) {
	r := require.New(t)

	var g flightGroup[string, int]

	_, err := g.Do("k", func() (int, error) { return 0, errors.New("first") })
	r.Error(err)

	// the failed call must be forgotten so a retry recomputes
	v, err := g.Do("k", func() (int, error) { return 7, nil })
	r.NoError(err)
	r.Equal(7, v)
}

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

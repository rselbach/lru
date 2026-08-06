package lru

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func waitForFlightWaiters[K comparable, V any](
	t *testing.T,
	group *flightGroup[K, V],
	key K,
	want int,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		group.mu.Lock()
		call := group.calls[key]
		got := 0
		if call != nil {
			got = call.waiting
		}
		group.mu.Unlock()
		if got == want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("singleflight waiters for %v did not reach %d", key, want)
}

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

func TestFlightGroup_NilPanicPropagatesToLeader(t *testing.T) {
	r := require.New(t)

	var g flightGroup[string, int]
	returned := false
	func() {
		defer func() {
			_ = recover()
		}()
		_, _ = g.Do("k", func() (int, error) { panic(nil) })
		returned = true
	}()

	r.False(returned, "panic(nil) must not become a successful zero result")

	value, err := g.Do("k", func() (int, error) { return 7, nil })
	r.NoError(err)
	r.Equal(7, value)
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
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { panics[i] = recover() }()
			_, _ = g.Do("k", func() (int, error) { return 99, nil })
		}(i)
	}

	waitForFlightWaiters(t, &g, "k", waiters)
	close(release)
	wg.Wait()

	r.Equal("boom", <-leaderPanic)
	for i := 0; i < waiters; i++ {
		r.Equal("boom", panics[i], "waiter %d", i)
	}
}

func TestFlightGroup_NilPanicPropagatesToWaiters(t *testing.T) {
	r := require.New(t)

	var g flightGroup[string, int]
	started := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		defer func() { _ = recover() }()
		_, _ = g.Do("k", func() (int, error) {
			close(started)
			<-release
			panic(nil)
		})
	}()
	<-started

	const waiters = 3
	var wg sync.WaitGroup
	returned := make([]bool, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { _ = recover() }()
			_, _ = g.Do("k", func() (int, error) { return 99, nil })
			returned[i] = true
		}(i)
	}

	waitForFlightWaiters(t, &g, "k", waiters)
	close(release)
	wg.Wait()
	<-leaderDone
	r.Equal([]bool{false, false, false}, returned)
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

	waitForFlightWaiters(t, &g, "k", waiters)
	close(release)
	wg.Wait()

	r.ErrorIs(<-leaderErr, wantErr)
	for i := 0; i < waiters; i++ {
		r.False(computed[i], "waiter %d computed independently", i)
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
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			// Done must run via defer so it fires even when the waiter
			// goroutine exits through the propagated Goexit.
			defer wg.Done()
			_, _ = g.Do("k", func() (int, error) { return 99, nil })
			returned[i] = true
		}(i)
	}

	waitForFlightWaiters(t, &g, "k", waiters)
	close(release)
	wg.Wait()
	<-leaderDone
	r.Equal([]bool{false, false, false}, returned)

	// the goexited call must be forgotten so later calls compute fresh
	value, err := g.Do("k", func() (int, error) { return 7, nil })
	r.NoError(err)
	r.Equal(7, value)
}

func TestFlightGroup_ContextFollowerCanCancel(t *testing.T) {
	r := require.New(t)
	var g flightGroup[string, int]
	started := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan struct{})
	leaderErr := make(chan error, 1)

	go func() {
		defer close(leaderDone)
		value, err := g.Do("k", func() (int, error) {
			close(started)
			<-release
			return 7, nil
		})
		if err == nil && value != 7 {
			err = errors.New("leader received wrong value")
		}
		leaderErr <- err
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	waiterComputed := false
	go func() {
		_, err := g.DoContext(ctx, "k", func() (int, error) {
			waiterComputed = true
			return 99, nil
		})
		waiterDone <- err
	}()
	waitForFlightWaiters(t, &g, "k", 1)
	cancel()

	select {
	case err := <-waiterDone:
		r.ErrorIs(err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("context follower did not stop waiting")
	}
	r.False(waiterComputed)
	select {
	case <-leaderDone:
		t.Fatal("canceling a follower canceled the leader")
	default:
	}

	close(release)
	<-leaderDone
	r.NoError(<-leaderErr)
}

func TestFlightGroup_CanceledContextDoesNotStartCall(t *testing.T) {
	r := require.New(t)
	var g flightGroup[string, int]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	computed := false

	_, err := g.DoContext(ctx, "k", func() (int, error) {
		computed = true
		return 1, nil
	})
	r.ErrorIs(err, context.Canceled)
	r.False(computed)
	r.Nil(g.calls)
}

func TestGetOrSetSingleflightContext_CanceledBeforeCompute(t *testing.T) {
	r := require.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cache := MustNew[string, int](4)
	expirable := MustNewExpirable[string, int](4, time.Minute)
	sharded := MustNewSharded[string, int](4)
	clock := MustNewClock[string, int](4)
	tiny := MustNewTinyLFU[string, int](4)

	tests := map[string]func(context.Context, func(context.Context) (int, error)) (int, error){
		"cache": func(ctx context.Context, fn func(context.Context) (int, error)) (int, error) {
			return cache.GetOrSetSingleflightContext(ctx, "k", fn)
		},
		"expirable": func(ctx context.Context, fn func(context.Context) (int, error)) (int, error) {
			return expirable.GetOrSetSingleflightContext(ctx, "k", fn)
		},
		"sharded": func(ctx context.Context, fn func(context.Context) (int, error)) (int, error) {
			return sharded.GetOrSetSingleflightContext(ctx, "k", fn)
		},
		"clock": func(ctx context.Context, fn func(context.Context) (int, error)) (int, error) {
			return clock.GetOrSetSingleflightContext(ctx, "k", fn)
		},
		"tiny LFU": func(ctx context.Context, fn func(context.Context) (int, error)) (int, error) {
			return tiny.GetOrSetSingleflightContext(ctx, "k", fn)
		},
	}

	for name, call := range tests {
		t.Run(name, func(t *testing.T) {
			computed := false
			_, err := call(ctx, func(context.Context) (int, error) {
				computed = true
				return 1, nil
			})
			r.ErrorIs(err, context.Canceled)
			r.False(computed)
		})
	}
}

func TestCache_GetOrSetSingleflightContextCachesLeaderResult(t *testing.T) {
	type contextKey struct{}

	r := require.New(t)
	cache := MustNew[string, int](4)
	ctx := context.WithValue(context.Background(), contextKey{}, "leader")

	value, err := cache.GetOrSetSingleflightContext(ctx, "k", func(got context.Context) (int, error) {
		r.Same(ctx, got)
		return 7, nil
	})
	r.NoError(err)
	r.Equal(7, value)
	stored, found := cache.Get("k")
	r.True(found)
	r.Equal(7, stored)
}

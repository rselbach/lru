package lru

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mockTime is a helper for testing time-based functionality.
// It is safe for concurrent use.
type mockTime struct {
	mu          sync.Mutex
	currentTime time.Time
}

func newMockTime() *mockTime {
	return &mockTime{
		currentTime: time.Now(),
	}
}

func (m *mockTime) Now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentTime
}

func (m *mockTime) Add(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.currentTime = m.currentTime.Add(d)
}

func TestExpirable_New(t *testing.T) {
	tests := map[string]struct {
		capacity int
		ttl      time.Duration
		wantErr  error
	}{
		"valid parameters": {
			capacity: 5,
			ttl:      time.Minute,
		},
		"zero capacity": {
			capacity: 0,
			ttl:      time.Minute,
			wantErr:  ErrInvalidCapacity,
		},
		"negative capacity": {
			capacity: -1,
			ttl:      time.Minute,
			wantErr:  ErrInvalidCapacity,
		},
		"zero ttl": {
			capacity: 5,
			ttl:      0,
			wantErr:  ErrInvalidTTL,
		},
		"negative ttl": {
			capacity: 5,
			ttl:      -time.Second,
			wantErr:  ErrInvalidTTL,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache, err := NewExpirable[string, int](tc.capacity, tc.ttl)
			if tc.wantErr != nil {
				r.ErrorIs(err, tc.wantErr)
				r.Nil(cache)
			} else {
				r.NoError(err)
				r.NotNil(cache)
				r.Equal(tc.capacity, cache.Capacity())
				r.Equal(tc.ttl, cache.TTL())
			}
		})
	}
}

func TestExpirable_MustNew(t *testing.T) {
	tests := map[string]struct {
		capacity  int
		ttl       time.Duration
		wantPanic error
	}{
		"valid parameters": {
			capacity: 5,
			ttl:      time.Minute,
		},
		"zero capacity": {
			capacity:  0,
			ttl:       time.Minute,
			wantPanic: ErrInvalidCapacity,
		},
		"negative capacity": {
			capacity:  -1,
			ttl:       time.Minute,
			wantPanic: ErrInvalidCapacity,
		},
		"zero ttl": {
			capacity:  5,
			ttl:       0,
			wantPanic: ErrInvalidTTL,
		},
		"negative ttl": {
			capacity:  5,
			ttl:       -time.Second,
			wantPanic: ErrInvalidTTL,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			if tc.wantPanic != nil {
				r.PanicsWithError(tc.wantPanic.Error(), func() {
					MustNewExpirable[string, int](tc.capacity, tc.ttl)
				})
			} else {
				cache := MustNewExpirable[string, int](tc.capacity, tc.ttl)
				r.NotNil(cache)
				r.Equal(tc.capacity, cache.Capacity())
				r.Equal(tc.ttl, cache.TTL())
			}
		})
	}
}

func TestExpirable_Expiration(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)

	cache.SetTimeNowFunc(mockClock.Now)

	// Add some items
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// Verify all items are in the cache
	r.Equal(3, cache.Len())
	r.True(cache.Contains("a"))
	r.True(cache.Contains("b"))
	r.True(cache.Contains("c"))

	// Advance time by 40 seconds (no items should expire yet)
	mockClock.Add(40 * time.Second)

	// All items should still be in the cache
	r.Equal(3, cache.Len())
	r.True(cache.Contains("a"))
	r.True(cache.Contains("b"))
	r.True(cache.Contains("c"))

	// Advance time past the TTL
	mockClock.Add(21 * time.Second) // total: 61 seconds > 1 minute

	// Now all items should be expired
	r.Equal(0, cache.Len())
	// expired entries still occupy storage until purged
	r.Equal(3, cache.PhysicalLen())
	r.False(cache.Contains("a"))
	r.False(cache.Contains("b"))
	r.False(cache.Contains("c"))
	r.Equal([]string{}, cache.Keys())

	r.Equal(3, cache.RemoveExpired())
	r.Equal(0, cache.PhysicalLen())
}

func TestExpirable_PhysicalLen(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[string, int](5, time.Minute)

	r.Equal(0, cache.PhysicalLen())
	cache.Set("a", 1)
	cache.Set("b", 2)
	r.Equal(2, cache.PhysicalLen())
	r.Equal(cache.Len(), cache.PhysicalLen())

	cache.Remove("a")
	r.Equal(1, cache.PhysicalLen())
}

func TestExpirable_ExpiryBoundary(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache := MustNewExpirable[string, int](5, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)

	cache.Set("a", 1)

	// an entry is still live at exactly its expiration instant
	mockClock.Add(time.Minute)
	val, found := cache.Get("a")
	r.True(found)
	r.Equal(1, val)
	r.True(cache.Contains("a"))

	// and expired strictly after it
	mockClock.Add(time.Nanosecond)
	r.False(cache.Contains("a"))
	_, found = cache.Get("a")
	r.False(found)
}

func TestExpirable_GetWithTTL(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)

	cache.SetTimeNowFunc(mockClock.Now)

	// Add an item
	cache.Set("a", 1)

	// Get with TTL; the clock is mocked, so the value is exact
	val, ttl, found := cache.GetWithTTL("a")
	r.True(found)
	r.Equal(1, val)
	r.Equal(time.Minute, ttl)

	// Advance time a bit
	mockClock.Add(30 * time.Second)

	// Get with TTL again, should show reduced TTL
	val, ttl, found = cache.GetWithTTL("a")
	r.True(found)
	r.Equal(1, val)
	r.Equal(30*time.Second, ttl)

	// Try with a non-existent key
	val, ttl, found = cache.GetWithTTL("nonexistent")
	r.False(found)
	r.Equal(0, val)
	r.Equal(time.Duration(0), ttl)

	// Advance past expiry
	mockClock.Add(31 * time.Second)

	// Should not find the expired item
	val, ttl, found = cache.GetWithTTL("a")
	r.False(found)
	r.Equal(0, val)
	r.Equal(time.Duration(0), ttl)
}

func TestExpirable_GetOrSet(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)

	cache.SetTimeNowFunc(mockClock.Now)

	// Track compute calls
	computeCalled := 0

	// GetOrSet on a new key should compute
	val, err := cache.GetOrSet("a", func() (int, error) {
		computeCalled++
		return 1, nil
	})
	r.NoError(err)
	r.Equal(1, val)
	r.Equal(1, computeCalled)

	// GetOrSet on an existing key should not compute
	val, err = cache.GetOrSet("a", func() (int, error) {
		computeCalled++
		return 99, nil
	})
	r.NoError(err)
	r.Equal(1, val)           // should still be original value
	r.Equal(1, computeCalled) // compute not called again

	// Advance past expiry
	mockClock.Add(time.Minute + time.Second)

	// GetOrSet on an expired key should compute again
	val, err = cache.GetOrSet("a", func() (int, error) {
		computeCalled++
		return 2, nil
	})
	r.NoError(err)
	r.Equal(2, val)           // new computed value
	r.Equal(2, computeCalled) // compute called again

	// Test error case
	_, err = cache.GetOrSet("b", func() (int, error) {
		computeCalled++
		return 0, errors.New("compute error")
	})
	r.Error(err)
	r.Equal(3, computeCalled) // compute called
	r.Equal(1, cache.Len())   // error should not add to cache
	r.False(cache.Contains("b"))
}

func TestExpirable_GetOrSet_KeyAddedWhileComputing(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[string, int](5, time.Minute)

	computeStarted := make(chan struct{})
	release := make(chan struct{})
	result := make(chan int, 1)
	go func() {
		val, _ := cache.GetOrSet("a", func() (int, error) {
			close(computeStarted)
			<-release
			return 10, nil
		})
		result <- val
	}()

	<-computeStarted
	cache.Set("a", 99) // beat the compute to the key
	close(release)

	r.Equal(99, <-result, "GetOrSet must return the value that won the race")
	val, found := cache.Get("a")
	r.True(found)
	r.Equal(99, val)
}

func TestExpirable_GetOrSet_ExpiredWhileComputing(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()
	cache := MustNewExpirable[string, int](5, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)

	evicted := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted[key] = value
	})

	computeStarted := make(chan struct{})
	release := make(chan struct{})
	result := make(chan int, 1)
	go func() {
		val, _ := cache.GetOrSet("a", func() (int, error) {
			close(computeStarted)
			<-release
			return 10, nil
		})
		result <- val
	}()

	<-computeStarted
	// the key is written and expires while the compute is still running
	cache.Set("a", 50, WithTTL(30*time.Second))
	mockClock.Add(31 * time.Second)
	close(release)

	// the locked re-check must replace the expired entry with the computed
	// value and report the dead value to the eviction callback
	r.Equal(10, <-result)
	r.Equal(map[string]int{"a": 50}, evicted)

	val, found := cache.Get("a")
	r.True(found)
	r.Equal(10, val)
}

func TestExpirable_GetOrSetSingleflight_ExpiredWhileComputing(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()
	cache := MustNewExpirable[string, int](5, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)

	evicted := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted[key] = value
	})

	computeStarted := make(chan struct{})
	release := make(chan struct{})
	result := make(chan int, 1)
	go func() {
		val, _ := cache.GetOrSetSingleflight("a", func() (int, error) {
			close(computeStarted)
			<-release
			return 10, nil
		})
		result <- val
	}()

	<-computeStarted
	// the key is written and expires while the compute is still running
	cache.Set("a", 50, WithTTL(30*time.Second))
	mockClock.Add(31 * time.Second)
	close(release)

	// the locked re-check must replace the expired entry with the computed
	// value and report the dead value to the eviction callback
	r.Equal(10, <-result)
	r.Equal(map[string]int{"a": 50}, evicted)

	val, found := cache.Get("a")
	r.True(found)
	r.Equal(10, val)
}

func TestExpirable_RemoveExpired(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)

	cache.SetTimeNowFunc(mockClock.Now)

	// Add some items
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// Nothing expired yet
	removed := cache.RemoveExpired()
	r.Equal(0, removed)
	r.Equal(3, cache.Len())

	// Advance time by 40 seconds (nothing should expire yet)
	mockClock.Add(40 * time.Second)

	// Still nothing expired
	removed = cache.RemoveExpired()
	r.Equal(0, removed)
	r.Equal(3, cache.Len())

	// Advance time past the TTL
	mockClock.Add(21 * time.Second) // total: 61 seconds > 1 minute

	// All items should be removed
	removed = cache.RemoveExpired()
	r.Equal(3, removed)
	r.Equal(0, cache.Len())
}

func TestExpirable_JanitorLifecycle(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[string, int](5, time.Minute)

	r.ErrorIs(cache.StartJanitor(0), ErrInvalidJanitorInterval)
	r.ErrorIs(cache.StartJanitor(-time.Second), ErrInvalidJanitorInterval)

	r.NoError(cache.StartJanitor(5 * time.Millisecond))
	r.NoError(cache.StartJanitor(5*time.Millisecond), "starting an already running janitor should be a no-op")

	cache.StopJanitor()
	cache.StopJanitor()

	r.NoError(cache.StartJanitor(5 * time.Millisecond))
	cache.StopJanitor()

	r.NoError(cache.StartJanitor(5 * time.Millisecond))
	cache.SignalStopJanitor()
	cache.StopJanitor() // wait for the signaled stop to finish
	r.NoError(cache.StartJanitor(5 * time.Millisecond))
	cache.StopJanitor()
}

func TestExpirable_SignalStopJanitorFromOnEvict(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[string, int](5, time.Minute)

	restartErr := make(chan error, 1)
	cache.OnEvict(func(string, int) {
		cache.SignalStopJanitor()
		restartErr <- cache.StartJanitor(2 * time.Millisecond)
	})

	cache.Set("a", 1, WithTTL(5*time.Millisecond))
	r.NoError(cache.StartJanitor(2 * time.Millisecond))

	select {
	case err := <-restartErr:
		r.ErrorIs(err, ErrJanitorStopping)
	case <-time.After(time.Second):
		t.Fatal("OnEvict did not run or StartJanitor blocked")
	}

	// StopJanitor must still be safe after a signal and must not hang.
	done := make(chan struct{})
	go func() {
		cache.StopJanitor()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("StopJanitor hung after SignalStopJanitor from OnEvict")
	}

	r.NoError(cache.StartJanitor(2 * time.Millisecond))
	cache.StopJanitor()
}

func TestExpirable_JanitorRemovesExpired(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[string, int](5, time.Minute)

	removed := make(chan string, 1)
	cache.OnEvict(func(key string, _ int) {
		cache.Len()
		removed <- key
	})

	cache.Set("a", 1, WithTTL(5*time.Millisecond))

	err := cache.StartJanitor(2 * time.Millisecond)
	r.NoError(err)
	defer cache.StopJanitor()

	select {
	case key := <-removed:
		r.Equal("a", key)
	case <-time.After(time.Second):
		t.Fatal("janitor did not remove expired entry")
	}

	waitForExpirablePhysicalLen(t, cache, 0)
}

func TestExpirable_JanitorConcurrentOperations(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[int, int](100, 10*time.Millisecond)

	err := cache.StartJanitor(time.Millisecond)
	r.NoError(err)
	defer cache.StopJanitor()

	var wg sync.WaitGroup
	for worker := 0; worker < 20; worker++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := base*1000 + i
				switch i % 7 {
				case 0:
					cache.Set(key, i, WithTTL(2*time.Millisecond))
				case 1:
					cache.Get(key)
				case 2:
					cache.Peek(key)
				case 3:
					cache.Contains(key)
				case 4:
					cache.Remove(key)
				case 5:
					cache.Keys()
				default:
					cache.Len()
				}
			}
		}(worker)
	}
	wg.Wait()
}

func TestExpirable_JanitorConcurrentStartStop(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[int, int](10, time.Minute)

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := cache.StartJanitor(time.Millisecond); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			cache.StopJanitor()
		}()
	}
	wg.Wait()
	close(errs)
	cache.StopJanitor()

	for err := range errs {
		r.ErrorIs(err, ErrJanitorStopping)
	}
}

func waitForExpirablePhysicalLen[K comparable, V any](t *testing.T, cache *Expirable[K, V], want int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cache.PhysicalLen() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}

	t.Fatalf("physical len: got %d, want %d", cache.PhysicalLen(), want)
}

func TestExpirable_SetTTL(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)

	cache.SetTimeNowFunc(mockClock.Now)

	// Set TTL
	err = cache.SetTTL(30 * time.Second)
	r.NoError(err)
	r.Equal(30*time.Second, cache.TTL())

	// Try setting to invalid value
	err = cache.SetTTL(0)
	r.ErrorIs(err, ErrInvalidTTL)
	r.Equal(30*time.Second, cache.TTL()) // should not change

	// Add an item with the new TTL
	cache.Set("a", 1)

	// Advance time past the new TTL
	mockClock.Add(40 * time.Second)

	// Item should be expired
	r.False(cache.Contains("a"))
}

func TestExpirable_SetTTLConcurrentWrites(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[int, int](128, time.Minute)

	const iterations = 1000

	start := make(chan struct{})
	done := make(chan struct{})
	errs := make(chan error, 8)

	// reportErr forwards the first errors to the test goroutine without
	// blocking; panicking or failing from a worker would be unreliable
	reportErr := func(err error) {
		select {
		case errs <- err:
		default:
		}
	}

	var ttlWG sync.WaitGroup
	ttlWG.Add(1)
	go func() {
		defer ttlWG.Done()
		<-start
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}

			ttl := time.Duration(i%10+1) * time.Second
			if err := cache.SetTTL(ttl); err != nil {
				reportErr(err)
				return
			}
		}
	}()

	var writerWG sync.WaitGroup
	for worker := 0; worker < 6; worker++ {
		writerWG.Add(1)
		go func(worker int) {
			defer writerWG.Done()
			<-start
			base := worker * iterations * 10
			for i := 0; i < iterations; i++ {
				key := base + i
				switch worker % 3 {
				case 0:
					cache.Set(key, i)
				case 1:
					if _, err := cache.GetOrSet(key, func() (int, error) {
						return i, nil
					}); err != nil {
						reportErr(err)
						return
					}
				default:
					if _, err := cache.GetOrSetSingleflight(key, func() (int, error) {
						return i, nil
					}); err != nil {
						reportErr(err)
						return
					}
				}
			}
		}(worker)
	}

	close(start)
	writerWG.Wait()
	close(done)
	ttlWG.Wait()
	close(errs)

	for err := range errs {
		r.NoError(err)
	}
}

func TestExpirable_LRUEviction(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](3, time.Minute)
	r.NoError(err)

	cache.SetTimeNowFunc(mockClock.Now)

	// Add items to fill the cache
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// Access "a" to make it recently used
	_, found := cache.Get("a")
	r.True(found)

	// Add a new item, should evict "b" (least recently used)
	cache.Set("d", 4)

	r.Equal(3, cache.Len())
	r.True(cache.Contains("a"))
	r.False(cache.Contains("b"))
	r.True(cache.Contains("c"))
	r.True(cache.Contains("d"))

	// Verify keys order (most recently used to least)
	r.Equal([]string{"d", "a", "c"}, cache.Keys())
}

func TestExpirable_TracksEarliestExpiry(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()
	start := mockClock.Now()

	cache := MustNewExpirable[string, int](3, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)
	cache.Set("default", 1)
	cache.Set("short", 2, WithTTL(30*time.Second))

	r.True(cache.hasNextExpiry)
	r.Equal(start.Add(30*time.Second), cache.nextExpiry)
	r.False(cache.expiryDueLocked(start.Add(30 * time.Second)))
	r.True(cache.expiryDueLocked(start.Add(30*time.Second + time.Nanosecond)))

	mockClock.Add(31 * time.Second)
	r.Equal(1, cache.RemoveExpired())
	r.Equal(start.Add(time.Minute), cache.nextExpiry)

	cache.Clear()
	r.False(cache.hasNextExpiry)
}

// requireConservativeWatermark asserts the invariant the expiry watermark must
// hold: it is never later than the earliest stored expiry, so a due expiry is
// never missed. It may be earlier, which only costs a later cleanup scan.
func requireConservativeWatermark[K comparable, V any](t *testing.T, cache *Expirable[K, V]) {
	t.Helper()
	r := require.New(t)

	var earliest time.Time
	stored := false
	for e := cache.head; e != nil; e = e.next {
		if !stored || e.meta.expiry.Before(earliest) {
			earliest = e.meta.expiry
			stored = true
		}
	}

	r.Equal(stored, cache.hasNextExpiry)
	if !stored {
		r.True(cache.nextExpiry.IsZero())
		return
	}
	r.False(cache.nextExpiry.After(earliest),
		"watermark %v is later than the earliest stored expiry %v", cache.nextExpiry, earliest)
}

func TestExpirable_NextExpiryAfterSingleRemovals(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()
	start := mockClock.Now()

	cache := MustNewExpirable[string, int](3, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)
	cache.Set("long", 1)
	cache.Set("short", 2, WithTTL(30*time.Second))
	r.Equal(start.Add(30*time.Second), cache.nextExpiry)

	// Removing the earliest entry leaves the watermark behind rather than
	// rescanning the cache; only the empty case resets it.
	r.True(cache.Remove("short"))
	requireConservativeWatermark(t, cache)

	cache.Set("mid", 3, WithTTL(45*time.Second))
	requireConservativeWatermark(t, cache)

	mockClock.Add(46 * time.Second)
	_, found := cache.Get("mid")
	r.False(found)
	requireConservativeWatermark(t, cache)
	r.True(cache.Contains("long"))

	r.True(cache.Remove("long"))
	r.False(cache.hasNextExpiry)
	r.True(cache.nextExpiry.IsZero())
}

func TestExpirable_NextExpiryAfterRemoveOldest(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache := MustNewExpirable[string, int](3, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)
	cache.Set("a", 1, WithTTL(20*time.Second))
	cache.Set("b", 2, WithTTL(40*time.Second))
	cache.Set("c", 3, WithTTL(60*time.Second))

	mockClock.Add(21 * time.Second)
	// Removes expired tail "a", then the live oldest "b".
	key, _, found := cache.RemoveOldest()
	r.True(found)
	r.Equal("b", key)
	r.Equal([]string{"c"}, cache.Keys())
	requireConservativeWatermark(t, cache)
}

func TestExpirable_NextExpiryAfterExtendingEarliest(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()
	start := mockClock.Now()

	cache := MustNewExpirable[string, int](2, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)
	cache.Set("short", 1, WithTTL(30*time.Second))
	cache.Set("long", 2)
	r.Equal(start.Add(30*time.Second), cache.nextExpiry)

	// Extending the earliest entry leaves the watermark at the old expiry
	// instead of rescanning. Both entries must stay live past it.
	cache.Set("short", 3, WithTTL(2*time.Minute))
	requireConservativeWatermark(t, cache)

	mockClock.Add(31 * time.Second)
	r.True(cache.Contains("short"))
	r.True(cache.Contains("long"))
}

func TestExpirable_StaleWatermarkPurgesOnCapacityWrite(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()
	start := mockClock.Now()

	cache := MustNewExpirable[string, int](3, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)

	evicted := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted[key] = value
	})

	cache.Set("a", 1, WithTTL(20*time.Second))
	cache.Set("b", 2, WithTTL(40*time.Second))
	cache.Set("c", 3, WithTTL(60*time.Second))

	// Removing the earliest entry leaves the watermark at "a"'s expiry even
	// though the earliest stored expiry is now "b"'s.
	r.True(cache.Remove("a"))
	r.Equal(start.Add(20*time.Second), cache.nextExpiry)
	cache.Set("d", 4, WithTTL(10*time.Minute))

	// The stale watermark must still make the capacity write purge "b" rather
	// than evict a live entry, and the scan restores an exact watermark.
	mockClock.Add(41 * time.Second)
	cache.Set("e", 5, WithTTL(10*time.Minute))

	r.Equal(map[string]int{"a": 1, "b": 2}, evicted)
	r.ElementsMatch([]string{"e", "d", "c"}, cache.Keys())
	r.Equal(start.Add(60*time.Second), cache.nextExpiry)
}

func TestExpirable_ZeroTimeExpiryIsTracked(t *testing.T) {
	r := require.New(t)
	now := time.Time{}.Add(-time.Nanosecond)
	cache := MustNewExpirable[string, int](2, time.Hour)
	cache.SetTimeNowFunc(func() time.Time { return now })

	cache.Set("live", 1)
	cache.Set("expired", 2, WithTTL(time.Nanosecond))
	r.True(cache.hasNextExpiry)
	r.True(cache.nextExpiry.IsZero())

	now = time.Time{}.Add(time.Nanosecond)
	cache.Set("new", 3)

	r.True(cache.Contains("live"))
	r.False(cache.Contains("expired"))
	r.True(cache.Contains("new"))
}

func TestExpirable_SetPurgesExpiredBeforeLiveEviction(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache := MustNewExpirable[string, int](3, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)

	cache.Set("live-tail", 1)
	cache.Set("expired", 2, WithTTL(30*time.Second))
	cache.Set("live-head", 3)

	var evictedKeys []string
	cache.OnEvict(func(key string, _ int) {
		evictedKeys = append(evictedKeys, key)
	})

	mockClock.Add(31 * time.Second)
	cache.Set("new", 4)

	r.True(cache.Contains("live-tail"))
	r.True(cache.Contains("live-head"))
	r.True(cache.Contains("new"))
	r.False(cache.Contains("expired"))
	r.Equal([]string{"new", "live-head", "live-tail"}, cache.Keys())
	r.Len(cache.items, 3)
	r.Equal([]string{"expired"}, evictedKeys)
}

func TestExpirable_SetExpiredCleanup_CallbackAfterUnlock(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache := MustNewExpirable[string, int](1, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)
	cache.Set("expired", 1, WithTTL(30*time.Second))
	mockClock.Add(31 * time.Second)

	callbackLen := make(chan int, 1)
	cache.OnEvict(func(string, int) {
		callbackLen <- cache.Len()
	})

	done := make(chan struct{})
	go func() {
		cache.Set("new", 2)
		close(done)
	}()

	select {
	case <-done:
		r.Equal(1, <-callbackLen)
	case <-time.After(time.Second):
		t.Fatal("Set cleanup callback appears to have run while the cache lock was held")
	}
}

func TestExpirable_Resize(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[string, int](3, time.Minute)

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	evicted, err := cache.Resize(5)
	r.NoError(err)
	r.Equal(0, evicted)
	r.Equal(5, cache.Capacity())
	r.Equal([]string{"c", "b", "a"}, cache.Keys())

	cache.Set("d", 4)
	cache.Set("e", 5)

	var evictedKeys []string
	cache.OnEvict(func(key string, _ int) {
		evictedKeys = append(evictedKeys, key)
	})

	evicted, err = cache.Resize(2)
	r.NoError(err)
	r.Equal(3, evicted)
	r.Equal(2, cache.Capacity())
	r.Equal([]string{"e", "d"}, cache.Keys())
	r.Equal([]string{"a", "b", "c"}, evictedKeys)

	evicted, err = cache.Resize(0)
	r.ErrorIs(err, ErrInvalidCapacity)
	r.Equal(0, evicted)
	r.Equal(2, cache.Capacity())
}

func TestExpirable_Resize_PurgesExpiredBeforeLiveEviction(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache := MustNewExpirable[string, int](4, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)
	cache.Set("expired-tail", 1, WithTTL(30*time.Second))
	cache.Set("live-tail", 2)
	cache.Set("expired-middle", 3, WithTTL(30*time.Second))
	cache.Set("live-head", 4)

	var evictedKeys []string
	cache.OnEvict(func(key string, _ int) {
		evictedKeys = append(evictedKeys, key)
	})

	mockClock.Add(31 * time.Second)

	evicted, err := cache.Resize(1)
	r.NoError(err)
	r.Equal(1, evicted)
	r.Equal(1, cache.Capacity())
	r.Equal([]string{"live-head"}, cache.Keys())
	r.Len(cache.items, 1)
	r.Equal([]string{"expired-tail", "live-tail", "expired-middle"}, evictedKeys)
}

func TestExpirable_Resize_CallbackAfterUnlock(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[string, int](2, time.Minute)
	cache.Set("a", 1)
	cache.Set("b", 2)

	callbackLen := make(chan int, 1)
	cache.OnEvict(func(string, int) {
		callbackLen <- cache.Len()
	})

	done := make(chan error, 1)
	go func() {
		_, err := cache.Resize(1)
		done <- err
	}()

	select {
	case err := <-done:
		r.NoError(err)
		r.Equal(1, <-callbackLen)
	case <-time.After(time.Second):
		t.Fatal("Resize callback appears to have run while the cache lock was held")
	}
}

func TestExpirable_GetOldest(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache := MustNewExpirable[string, int](5, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)

	key, value, ok := cache.GetOldest()
	r.False(ok)
	r.Empty(key)
	r.Zero(value)

	cache.Set("expired", 1, WithTTL(30*time.Second))
	cache.Set("live1", 2)
	cache.Set("live2", 3)

	mockClock.Add(31 * time.Second)

	key, value, ok = cache.GetOldest()
	r.True(ok)
	r.Equal("live1", key)
	r.Equal(2, value)
	r.Equal([]string{"live2", "live1"}, cache.Keys())
	r.Len(cache.items, 3, "GetOldest should not purge expired entries")
}

func TestExpirable_RemoveOldest(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache := MustNewExpirable[string, int](5, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)

	key, value, ok := cache.RemoveOldest()
	r.False(ok)
	r.Empty(key)
	r.Zero(value)

	cache.Set("expired", 1, WithTTL(30*time.Second))
	cache.Set("live1", 2)
	cache.Set("live2", 3)

	var evictedKeys []string
	cache.OnEvict(func(key string, _ int) {
		evictedKeys = append(evictedKeys, key)
	})

	mockClock.Add(31 * time.Second)

	key, value, ok = cache.RemoveOldest()
	r.True(ok)
	r.Equal("live1", key)
	r.Equal(2, value)
	r.Equal([]string{"live2"}, cache.Keys())
	r.Len(cache.items, 1)
	r.Equal([]string{"expired", "live1"}, evictedKeys)
}

func TestExpirable_RemoveOldest_AllExpired(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache := MustNewExpirable[string, int](5, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)
	cache.Set("a", 1, WithTTL(30*time.Second))
	cache.Set("b", 2, WithTTL(30*time.Second))

	var evictedKeys []string
	cache.OnEvict(func(key string, _ int) {
		evictedKeys = append(evictedKeys, key)
	})

	mockClock.Add(31 * time.Second)

	key, value, ok := cache.RemoveOldest()
	r.False(ok)
	r.Empty(key)
	r.Zero(value)
	r.Empty(cache.Keys())
	r.Empty(cache.items)
	r.Equal([]string{"a", "b"}, evictedKeys)
}

func TestExpirable_RemoveOldest_CallbackAfterUnlock(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[string, int](2, time.Minute)
	cache.Set("a", 1)
	cache.Set("b", 2)

	callbackLen := make(chan int, 1)
	cache.OnEvict(func(string, int) {
		callbackLen <- cache.Len()
	})

	done := make(chan bool, 1)
	go func() {
		_, _, ok := cache.RemoveOldest()
		done <- ok
	}()

	select {
	case ok := <-done:
		r.True(ok)
		r.Equal(1, <-callbackLen)
	case <-time.After(time.Second):
		t.Fatal("RemoveOldest callback appears to have run while the cache lock was held")
	}
}

func TestExpirable_Values(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache := MustNewExpirable[string, int](5, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)

	emptyValues := cache.Values()
	r.Empty(emptyValues)
	r.Zero(cap(emptyValues))

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	r.Equal([]string{"c", "b", "a"}, cache.Keys())
	r.Equal([]int{3, 2, 1}, cache.Values())

	_, _ = cache.Get("a")
	r.Equal([]string{"a", "c", "b"}, cache.Keys())
	r.Equal([]int{1, 3, 2}, cache.Values())

	cache.Set("short", 4, WithTTL(30*time.Second))
	mockClock.Add(31 * time.Second)

	keys := cache.Keys()
	values := cache.Values()
	r.Equal([]string{"a", "c", "b"}, keys)
	r.Equal([]int{1, 3, 2}, values)
	r.Equal(3, cap(keys))
	r.Equal(3, cap(values))
}

func TestExpirable_Items(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()
	cache := MustNewExpirable[string, int](5, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("short", 3, WithTTL(30*time.Second))
	mockClock.Add(31 * time.Second)

	r.Equal([]Item[string, int]{
		{Key: "b", Value: 2},
		{Key: "a", Value: 1},
	}, cache.Items())
	r.Equal(3, cache.PhysicalLen(), "Items must not purge expired entries")
}

func TestExpirable_Peek(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)
	cache.SetTimeNowFunc(mockClock.Now)

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// peek should return value without affecting LRU order
	val, found := cache.Peek("a")
	r.True(found)
	r.Equal(1, val)

	// order should still be c, b, a (a was not moved to front)
	r.Equal([]string{"c", "b", "a"}, cache.Keys())

	// peek non-existent key
	_, found = cache.Peek("z")
	r.False(found)

	// advance time past expiration
	mockClock.Add(time.Minute + time.Second)

	// peek should return not found for expired entry (but not remove it)
	_, found = cache.Peek("a")
	r.False(found)

	// all three expired entries must still be physically present: Peek does
	// not purge, and Len only excludes them from the count
	r.Len(cache.items, 3)
	r.Equal(0, cache.Len())

	// but Get() should remove the entry it touches
	_, found = cache.Get("b")
	r.False(found)
	r.Len(cache.items, 2)
}

func TestExpirable_GetOrSetSingleflight(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)
	cache.SetTimeNowFunc(mockClock.Now)

	// basic functionality: compute is called when key doesn't exist
	var computeCount int32
	val, err := cache.GetOrSetSingleflight("a", func() (int, error) {
		atomic.AddInt32(&computeCount, 1)
		return 42, nil
	})
	r.NoError(err)
	r.Equal(42, val)
	r.Equal(int32(1), atomic.LoadInt32(&computeCount))

	// second call should use cached value, compute not called
	val, err = cache.GetOrSetSingleflight("a", func() (int, error) {
		atomic.AddInt32(&computeCount, 1)
		return 99, nil
	})
	r.NoError(err)
	r.Equal(42, val)
	r.Equal(int32(1), atomic.LoadInt32(&computeCount))

	// expire the entry
	mockClock.Add(time.Minute + time.Second)

	// now compute should be called again
	val, err = cache.GetOrSetSingleflight("a", func() (int, error) {
		atomic.AddInt32(&computeCount, 1)
		return 100, nil
	})
	r.NoError(err)
	r.Equal(100, val)
	r.Equal(int32(2), atomic.LoadInt32(&computeCount))

	// error case
	_, err = cache.GetOrSetSingleflight("error", func() (int, error) {
		return 0, errors.New("compute error")
	})
	r.Error(err)
	r.False(cache.Contains("error"))
}

func TestExpirable_GetOrSetSingleflight_Concurrent(t *testing.T) {
	r := require.New(t)
	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)

	const goroutines = 100
	var computeCount int32
	var wg sync.WaitGroup
	begin := make(chan struct{})
	computeStarted := make(chan struct{})
	release := make(chan struct{})
	results := make([]int, goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-begin
			val, err := cache.GetOrSetSingleflight("shared", func() (int, error) {
				if atomic.AddInt32(&computeCount, 1) == 1 {
					close(computeStarted)
				}
				<-release
				return 42, nil
			})
			results[idx] = val
			errs[idx] = err
		}(i)
	}

	close(begin)
	<-computeStarted
	waitForFlightWaiters(t, &cache.sfGroup, "shared", goroutines-1)
	close(release)
	wg.Wait()

	r.Equal(int32(1), atomic.LoadInt32(&computeCount), "compute should be called exactly once")
	for i, result := range results {
		r.NoError(errs[i], "goroutine %d", i)
		r.Equal(42, result, "goroutine %d got wrong result", i)
	}
}

func TestExpirable_GetOrSetSingleflight_DistinctStringifiedKeys(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[collidingStringKey, string](5, time.Minute)

	computeStarted := make(chan int, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseComputes := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	defer releaseComputes()

	type result struct {
		keyID int
		value string
		err   error
	}
	results := make(chan result, 2)

	for _, tc := range []struct {
		key   collidingStringKey
		value string
	}{
		{key: collidingStringKey{id: 1}, value: "value-1"},
		{key: collidingStringKey{id: 2}, value: "value-2"},
	} {
		tc := tc
		go func() {
			value, err := cache.GetOrSetSingleflight(tc.key, func() (string, error) {
				computeStarted <- tc.key.id
				<-release
				return tc.value, nil
			})
			results <- result{keyID: tc.key.id, value: value, err: err}
		}()
	}

	started := make(map[int]bool)
	for len(started) < 2 {
		select {
		case keyID := <-computeStarted:
			started[keyID] = true
		case <-time.After(time.Second):
			releaseComputes()
			t.Fatalf("want both distinct keys to compute; started: %v", started)
		}
	}
	releaseComputes()

	got := make(map[int]string)
	for i := 0; i < 2; i++ {
		res := <-results
		r.NoError(res.err)
		got[res.keyID] = res.value
	}

	r.Equal(map[int]string{1: "value-1", 2: "value-2"}, got)

	value, found := cache.Peek(collidingStringKey{id: 1})
	r.True(found)
	r.Equal("value-1", value)
	value, found = cache.Peek(collidingStringKey{id: 2})
	r.True(found)
	r.Equal("value-2", value)
}

func TestExpirable_WithTTL(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)
	cache.SetTimeNowFunc(mockClock.Now)

	// set with default TTL (1 minute)
	cache.Set("default", 1)

	// set with shorter TTL (30 seconds)
	cache.Set("short", 2, WithTTL(30*time.Second))

	// set with longer TTL (2 minutes)
	cache.Set("long", 3, WithTTL(2*time.Minute))

	// all should be present initially
	r.True(cache.Contains("default"))
	r.True(cache.Contains("short"))
	r.True(cache.Contains("long"))

	// advance 35 seconds - short should expire
	mockClock.Add(35 * time.Second)

	r.True(cache.Contains("default"))
	r.False(cache.Contains("short"))
	r.True(cache.Contains("long"))

	// advance to 65 seconds - default should also expire
	mockClock.Add(30 * time.Second)

	r.False(cache.Contains("default"))
	r.False(cache.Contains("short"))
	r.True(cache.Contains("long"))

	// advance to 2.5 minutes - all should be expired
	mockClock.Add(90 * time.Second)

	r.False(cache.Contains("default"))
	r.False(cache.Contains("short"))
	r.False(cache.Contains("long"))
}

func TestExpirable_WithTTL_GetOrSet(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)
	cache.SetTimeNowFunc(mockClock.Now)

	// GetOrSet with custom TTL
	val, err := cache.GetOrSet("key", func() (int, error) {
		return 42, nil
	}, WithTTL(30*time.Second))
	r.NoError(err)
	r.Equal(42, val)

	// verify TTL by checking it expires at the right time
	mockClock.Add(25 * time.Second)
	r.True(cache.Contains("key"))

	mockClock.Add(10 * time.Second) // 35 seconds total
	r.False(cache.Contains("key"))
}

func TestExpirable_WithTTL_GetOrSetSingleflight(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)
	cache.SetTimeNowFunc(mockClock.Now)

	// GetOrSetSingleflight with custom TTL
	val, err := cache.GetOrSetSingleflight("key", func() (int, error) {
		return 42, nil
	}, WithTTL(30*time.Second))
	r.NoError(err)
	r.Equal(42, val)

	// verify TTL by checking it expires at the right time
	mockClock.Add(25 * time.Second)
	r.True(cache.Contains("key"))

	mockClock.Add(10 * time.Second) // 35 seconds total
	r.False(cache.Contains("key"))
}

func TestExpirable_WithTTL_RejectsNonPositiveValues(t *testing.T) {
	tests := map[string]struct {
		ttl time.Duration
	}{
		"zero":     {ttl: 0},
		"negative": {ttl: -time.Second},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			cache := MustNewExpirable[string, int](5, time.Minute)
			cache.Set("existing", 1)

			r.PanicsWithValue(ErrInvalidTTL, func() {
				cache.Set("new", 42, WithTTL(tc.ttl))
			})

			computed := false
			_, err := cache.GetOrSet("existing", func() (int, error) {
				computed = true
				return 42, nil
			}, WithTTL(tc.ttl))
			r.ErrorIs(err, ErrInvalidTTL)
			r.False(computed)

			_, err = cache.GetOrSetSingleflight("new", func() (int, error) {
				computed = true
				return 42, nil
			}, WithTTL(tc.ttl))
			r.ErrorIs(err, ErrInvalidTTL)
			r.False(computed)
			r.Equal(1, cache.PhysicalLen())
		})
	}
}

func TestExpirable_SetTimeNowFunc_NilResetsToRealTime(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache := MustNewExpirable[string, int](5, time.Hour)
	cache.SetTimeNowFunc(mockClock.Now)

	cache.Set("a", 1)
	mockClock.Add(2 * time.Hour)
	r.False(cache.Contains("a"), "entry must be expired under the mock clock")

	// resetting to the real clock revives the entry: its expiry is one hour
	// after the mock start time, which is (roughly) the real present
	cache.SetTimeNowFunc(nil)
	r.True(cache.Contains("a"))
}

func TestExpirable_GetOldest_AllExpired(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache := MustNewExpirable[string, int](5, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)

	cache.Set("a", 1)
	cache.Set("b", 2)
	mockClock.Add(time.Minute + time.Second)

	key, value, ok := cache.GetOldest()
	r.False(ok)
	r.Empty(key)
	r.Zero(value)
	r.Len(cache.items, 2, "GetOldest should not purge expired entries")
}

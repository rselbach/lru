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
		capacity    int
		ttl         time.Duration
		expectError bool
	}{
		"valid parameters": {
			capacity:    5,
			ttl:         time.Minute,
			expectError: false,
		},
		"zero capacity": {
			capacity:    0,
			ttl:         time.Minute,
			expectError: true,
		},
		"negative capacity": {
			capacity:    -1,
			ttl:         time.Minute,
			expectError: true,
		},
		"zero ttl": {
			capacity:    5,
			ttl:         0,
			expectError: true,
		},
		"negative ttl": {
			capacity:    5,
			ttl:         -time.Second,
			expectError: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache, err := NewExpirable[string, int](tc.capacity, tc.ttl)
			if tc.expectError {
				r.Error(err)
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
		capacity     int
		ttl          time.Duration
		expectPanic  bool
		panicMessage string
	}{
		"valid parameters": {
			capacity:    5,
			ttl:         time.Minute,
			expectPanic: false,
		},
		"zero capacity": {
			capacity:     0,
			ttl:          time.Minute,
			expectPanic:  true,
			panicMessage: "capacity must be greater than zero",
		},
		"negative capacity": {
			capacity:     -1,
			ttl:          time.Minute,
			expectPanic:  true,
			panicMessage: "capacity must be greater than zero",
		},
		"zero ttl": {
			capacity:     5,
			ttl:          0,
			expectPanic:  true,
			panicMessage: "TTL must be greater than zero",
		},
		"negative ttl": {
			capacity:     5,
			ttl:          -time.Second,
			expectPanic:  true,
			panicMessage: "TTL must be greater than zero",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			if tc.expectPanic {
				r.PanicsWithError(tc.panicMessage, func() {
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
	r.False(cache.Contains("a"))
	r.False(cache.Contains("b"))
	r.False(cache.Contains("c"))
	r.Equal([]string{}, cache.Keys())
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

	// Get with TTL
	val, ttl, found := cache.GetWithTTL("a")
	r.True(found)
	r.Equal(1, val)
	r.InDelta(time.Minute, ttl, float64(time.Second))

	// Advance time a bit
	mockClock.Add(30 * time.Second)

	// Get with TTL again, should show reduced TTL
	val, ttl, found = cache.GetWithTTL("a")
	r.True(found)
	r.Equal(1, val)
	r.InDelta(30*time.Second, ttl, float64(time.Second))

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

	r.Error(cache.StartJanitor(0))
	r.Error(cache.StartJanitor(-time.Second))

	r.NoError(cache.StartJanitor(5 * time.Millisecond))
	r.NoError(cache.StartJanitor(5*time.Millisecond), "starting an already running janitor should be a no-op")

	cache.StopJanitor()
	cache.StopJanitor()

	r.NoError(cache.StartJanitor(5 * time.Millisecond))
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
		r.NoError(err)
	}
}

func waitForExpirablePhysicalLen[K comparable, V any](t *testing.T, cache *Expirable[K, V], want int) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		cache.mu.RLock()
		got := len(cache.items)
		cache.mu.RUnlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}

	cache.mu.RLock()
	got := len(cache.items)
	cache.mu.RUnlock()
	t.Fatalf("expected physical len %d, got %d", want, got)
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
	r.Error(err)
	r.Equal(30*time.Second, cache.TTL()) // should not change

	// Add an item with the new TTL
	cache.Set("a", 1)

	// Advance time past the new TTL
	mockClock.Add(40 * time.Second)

	// Item should be expired
	r.False(cache.Contains("a"))
}

func TestExpirable_SetTTLConcurrentWrites(t *testing.T) {
	cache := MustNewExpirable[int, int](128, time.Minute)

	const iterations = 1000

	start := make(chan struct{})
	done := make(chan struct{})

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
				panic(err)
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
						panic(err)
					}
				default:
					if _, err := cache.GetOrSetSingleflight(key, func() (int, error) {
						return i, nil
					}); err != nil {
						panic(err)
					}
				}
			}
		}(worker)
	}

	close(start)
	writerWG.Wait()
	close(done)
	ttlWG.Wait()
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
	r.Error(err)
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
	r.ElementsMatch([]string{"expired-middle", "expired-tail", "live-tail"}, evictedKeys)
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

	r.Empty(cache.Values())

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

	r.Equal([]string{"a", "c", "b"}, cache.Keys())
	r.Equal([]int{1, 3, 2}, cache.Values())
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

	// entry should still be in items map (not removed by Peek)
	// we can verify by checking that Len() still counts it as 0 (expired)
	r.Equal(0, cache.Len())

	// but Get() should remove it
	_, found = cache.Get("b")
	r.False(found)
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
	results := make([]int, goroutines)

	// all goroutines try to get the same key concurrently
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			val, err := cache.GetOrSetSingleflight("shared", func() (int, error) {
				atomic.AddInt32(&computeCount, 1)
				return 42, nil
			})
			r.NoError(err)
			results[idx] = val
		}(i)
	}
	wg.Wait()

	// compute should have been called exactly once
	r.Equal(int32(1), atomic.LoadInt32(&computeCount), "compute should be called exactly once")

	// all results should be the same
	for i, result := range results {
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
			t.Fatalf("expected both distinct keys to compute; started computes: %v", started)
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

func TestExpirable_WithTTL_ZeroUsesDefault(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)
	cache.SetTimeNowFunc(mockClock.Now)

	// WithTTL(0) should use default TTL
	cache.Set("key", 42, WithTTL(0))

	// should still be there at 55 seconds
	mockClock.Add(55 * time.Second)
	r.True(cache.Contains("key"))

	// should be gone at 65 seconds (past 1 minute default)
	mockClock.Add(10 * time.Second)
	r.False(cache.Contains("key"))
}

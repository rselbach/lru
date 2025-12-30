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
type mockTime struct {
	currentTime time.Time
}

func newMockTime() *mockTime {
	return &mockTime{
		currentTime: time.Now(),
	}
}

func (m *mockTime) Now() time.Time {
	return m.currentTime
}

func (m *mockTime) Add(d time.Duration) {
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

	// Override the timeNow function to use our mock
	cache.timeNow = mockClock.Now

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

func TestExpirable_GetWithTTL(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)

	// Override the timeNow function to use our mock
	cache.timeNow = mockClock.Now

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

	// Override the timeNow function to use our mock
	cache.timeNow = mockClock.Now

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

func TestExpirable_RemoveExpired(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)

	// Override the timeNow function to use our mock
	cache.timeNow = mockClock.Now

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

func TestExpirable_SetTTL(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)

	// Override the timeNow function to use our mock
	cache.timeNow = mockClock.Now

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

func TestExpirable_LRUEviction(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](3, time.Minute)
	r.NoError(err)

	// Override the timeNow function to use our mock
	cache.timeNow = mockClock.Now

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

func TestExpirable_Peek(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)
	cache.timeNow = mockClock.Now

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
	cache.timeNow = mockClock.Now

	// basic functionality: compute is called when key doesn't exist
	var computeCount atomic.Int32
	val, err := cache.GetOrSetSingleflight("a", func() (int, error) {
		computeCount.Add(1)
		return 42, nil
	})
	r.NoError(err)
	r.Equal(42, val)
	r.Equal(int32(1), computeCount.Load())

	// second call should use cached value, compute not called
	val, err = cache.GetOrSetSingleflight("a", func() (int, error) {
		computeCount.Add(1)
		return 99, nil
	})
	r.NoError(err)
	r.Equal(42, val)
	r.Equal(int32(1), computeCount.Load())

	// expire the entry
	mockClock.Add(time.Minute + time.Second)

	// now compute should be called again
	val, err = cache.GetOrSetSingleflight("a", func() (int, error) {
		computeCount.Add(1)
		return 100, nil
	})
	r.NoError(err)
	r.Equal(100, val)
	r.Equal(int32(2), computeCount.Load())

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
	var computeCount atomic.Int32
	var wg sync.WaitGroup
	results := make([]int, goroutines)

	// all goroutines try to get the same key concurrently
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			val, err := cache.GetOrSetSingleflight("shared", func() (int, error) {
				computeCount.Add(1)
				return 42, nil
			})
			r.NoError(err)
			results[idx] = val
		}(i)
	}
	wg.Wait()

	// compute should have been called exactly once
	r.Equal(int32(1), computeCount.Load(), "compute should be called exactly once")

	// all results should be the same
	for i, result := range results {
		r.Equal(42, result, "goroutine %d got wrong result", i)
	}
}

func TestExpirable_WithTTL(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](5, time.Minute)
	r.NoError(err)
	cache.timeNow = mockClock.Now

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
	cache.timeNow = mockClock.Now

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
	cache.timeNow = mockClock.Now

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
	cache.timeNow = mockClock.Now

	// WithTTL(0) should use default TTL
	cache.Set("key", 42, WithTTL(0))

	// should still be there at 55 seconds
	mockClock.Add(55 * time.Second)
	r.True(cache.Contains("key"))

	// should be gone at 65 seconds (past 1 minute default)
	mockClock.Add(10 * time.Second)
	r.False(cache.Contains("key"))
}

package lru

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCache_OnEvict(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](3)

	evicted := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted[key] = value
	})

	// Add items to the cache
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// No evictions yet
	r.Empty(evicted)

	// This should evict "a" since it's the least recently used
	cache.Set("d", 4)
	r.Equal(map[string]int{"a": 1}, evicted)

	// Test explicit removal
	cache.Remove("b")
	r.Equal(map[string]int{"a": 1, "b": 2}, evicted)

	// Update "c" - should not trigger eviction
	cache.Set("c", 30)
	r.Equal(map[string]int{"a": 1, "b": 2}, evicted)

	// Clear the cache - should evict all remaining items
	cache.Clear()
	r.Equal(map[string]int{"a": 1, "b": 2, "c": 30, "d": 4}, evicted)
}

func TestCache_OnEvictReplacement(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](3)

	evicted1 := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted1[key] = value
	})

	// Add items and cause an eviction
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)
	cache.Set("d", 4) // should evict "a"

	r.Equal(map[string]int{"a": 1}, evicted1)

	// Replace the callback
	evicted2 := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted2[key] = value
	})

	// Cause another eviction
	cache.Set("e", 5) // should evict "b"

	// The new callback should be called, not the old one
	r.Equal(map[string]int{"a": 1}, evicted1)
	r.Equal(map[string]int{"b": 2}, evicted2)

	// Set callback to nil
	cache.OnEvict(nil)

	// Cause another eviction
	cache.Set("f", 6) // should evict "c"

	// No callback should be called
	r.Equal(map[string]int{"a": 1}, evicted1)
	r.Equal(map[string]int{"b": 2}, evicted2)
}

func TestExpirable_OnEvict(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](3, time.Minute)
	r.NoError(err)

	// Override the timeNow function to use our mock
	cache.SetTimeNowFunc(mockClock.Now)

	evicted := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted[key] = value
	})

	// Add items to the cache
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// No evictions yet
	r.Empty(evicted)

	// This should evict "a" since it's the least recently used
	cache.Set("d", 4)
	r.Equal(map[string]int{"a": 1}, evicted)

	// Test explicit removal
	cache.Remove("b")
	r.Equal(map[string]int{"a": 1, "b": 2}, evicted)

	// Advance time past expiration
	mockClock.Add(time.Minute + time.Second)

	// The expired items won't be automatically evicted until a write operation
	r.Equal(map[string]int{"a": 1, "b": 2}, evicted)

	// This should not call the callback for expired items removed implicitly
	cache.Set("e", 5)
	r.Equal(map[string]int{"a": 1, "b": 2}, evicted)

	// Add new items to test RemoveExpired with callbacks
	evicted = make(map[string]int) // Reset the eviction map
	cache.Set("f", 6)
	cache.Set("g", 7)

	// Advance time past expiration again
	mockClock.Add(time.Minute + time.Second)

	// Explicit removal should call callbacks
	removed := cache.RemoveExpired()
	r.Equal(3, removed) // should remove e, f, g
	r.Equal(map[string]int{"e": 5, "f": 6, "g": 7}, evicted)
}

func TestExpirable_Clear(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](3, time.Minute)
	r.NoError(err)

	// Override the timeNow function to use our mock
	cache.SetTimeNowFunc(mockClock.Now)

	evicted := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted[key] = value
	})

	// Add items to the cache
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// No evictions yet
	r.Empty(evicted)

	// Advance time past expiration for "a" and "b" but not "c"
	mockClock.Add(30 * time.Second)
	cache.Set("c", 30)              // update c's TTL
	mockClock.Add(31 * time.Second) // now a and b are expired but c is not

	// Clear should only call callback for non-expired items
	cache.Clear()
	r.Equal(map[string]int{"c": 30}, evicted)
}

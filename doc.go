// Package lru provides generic, thread-safe LRU cache implementations.
//
// Three cache types are provided:
//
//   - [Cache]: A standard LRU cache with fixed capacity
//   - [Expirable]: An LRU cache with per-entry TTL expiration
//   - [Sharded]: A sharded LRU cache for reduced lock contention under high concurrency
//
// All are safe for concurrent use and support eviction callbacks.
//
// # Basic Usage
//
// Create a cache and store values:
//
//	cache := lru.MustNew[string, int](100)
//	cache.Set("key", 42)
//	value, found := cache.Get("key")
//
// # Memoization with GetOrSet
//
// Compute values on cache miss:
//
//	result, err := cache.GetOrSet("key", func() (int, error) {
//	    return expensiveComputation()
//	})
//
// For expensive computations where concurrent cache misses for the same key should
// only trigger a single computation, use [Cache.GetOrSetSingleflight]:
//
//	result, err := cache.GetOrSetSingleflight("key", func() (int, error) {
//	    return expensiveAPICall()
//	})
//
// # Expirable Cache
//
// Create a cache where entries expire after a duration:
//
//	cache := lru.MustNewExpirable[string, int](100, 5*time.Minute)
//	cache.Set("key", 42)
//	value, ttl, found := cache.GetWithTTL("key")
//
// TTL is fixed per write; reads do not reset the TTL (no sliding expiration).
// Each entry's expiration time is set when written via [Expirable.Set] or
// [Expirable.GetOrSet] and is not extended by subsequent reads.
//
// Per-entry TTL can be set using the [WithTTL] option:
//
//	cache.Set("shortLived", 42, lru.WithTTL(30*time.Second))
//	cache.Set("longLived", 100, lru.WithTTL(1*time.Hour))
//
// Expired entries are removed lazily on access or during write operations.
// Call [Expirable.RemoveExpired] to explicitly purge all expired entries.
//
// # Eviction Callbacks
//
// Register a callback to be notified when entries are evicted:
//
//	cache.OnEvict(func(key string, value int) {
//	    fmt.Printf("evicted: %s=%d\n", key, value)
//	})
//
// Callbacks are invoked for capacity evictions, explicit removals via
// [Cache.Remove], and [Cache.Clear]. For [Expirable.Clear], callbacks are
// only invoked for entries that have not yet expired. However, capacity-based
// evictions trigger the callback even if the evicted entry has already expired.
//
// Callbacks are invoked after the cache's internal lock is released and may be
// called concurrently from multiple goroutines. Callback implementations must
// be safe for concurrent use.
package lru

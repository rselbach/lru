// Package lru provides generic, thread-safe LRU cache implementations.
//
// Two cache types are provided:
//
//   - [Cache]: A standard LRU cache with fixed capacity
//   - [Expirable]: An LRU cache with per-entry TTL expiration
//
// Both are safe for concurrent use and support eviction callbacks.
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
// # Expirable Cache
//
// Create a cache where entries expire after a duration:
//
//	cache := lru.MustNewExpirable[string, int](100, 5*time.Minute)
//	cache.Set("key", 42)
//	value, ttl, found := cache.GetWithTTL("key")
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
// only invoked for entries that have not yet expired.
package lru

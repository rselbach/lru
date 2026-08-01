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
// If another goroutine inserts the key while compute runs, that stored value is
// returned and the result of compute is discarded. compute must be safe to
// abandon (no unreclaimed side effects), or use GetOrSetSingleflight.
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
// Concurrent [Expirable.GetOrSetSingleflight] callers that share an in-flight key
// share the leader's computed value and the leader's effective TTL; a waiter's
// [WithTTL] option is not applied.
//
// Expired entries are removed lazily on access. They still occupy capacity until
// purged, so a cache full of expired entries must purge on write (automatic when
// a Set needs a slot), via [Expirable.RemoveExpired], or via the optional janitor.
// [Expirable.Len] reports only non-expired entries.
//
// When a write needs capacity, expired entries are purged before evicting a
// non-expired LRU entry. Applications that want periodic background cleanup can
// opt in with [Expirable.StartJanitor] and stop it with [Expirable.StopJanitor].
//
// # Sharded Cache
//
// A [Sharded] cache splits total capacity across independent [Cache] instances
// to reduce lock contention. It is not a global LRU: each shard enforces its own
// capacity and recency order. Methods such as [Sharded.Keys] return a
// point-in-time snapshot grouped by shard, not global recency order.
// [Sharded.Len], [Sharded.Clear], and [Sharded.OnEvict] are likewise applied
// per shard and are not atomic across the whole cache.
//
// Shard selection uses a fast path for common key types (strings and integers).
// Other comparable keys fall back to fmt formatting; prefer string or integer
// keys on hot paths. Types with identical fmt output can share a shard.
//
// # Eviction Callbacks
//
// Register a callback to be notified when entries are evicted:
//
//	cache.OnEvict(func(key string, value int) {
//	    fmt.Printf("evicted: %s=%d\n", key, value)
//	})
//
// When OnEvict runs:
//
//   - Capacity eviction: Cache, Expirable, and Sharded
//   - [Cache.Remove] / [Expirable.Remove] / [Sharded.Remove]: yes (Expirable
//     includes already-expired entries still present in storage)
//   - [Cache.RemoveOldest] / [Expirable.RemoveOldest]: yes
//   - [Cache.Clear]: every entry, least- to most-recently used
//   - [Expirable.Clear]: non-expired entries only, least- to most-recently used
//   - [Expirable.RemoveExpired], janitor, and capacity expiry cleanup: yes
//   - [Expirable.Set] replacing an already-expired entry: yes for the old value
//   - [Cache.Resize] / [Expirable.Resize] / [Sharded.Resize]: yes for live
//     evictions (Expirable also reports expired entries purged during resize)
//
// Resize and Clear report evicted entries in order from least recently used to
// most recently used within the cache (or within each shard for [Sharded]).
//
// Callbacks are invoked after the cache's internal lock is released and may be
// called concurrently from multiple goroutines. Callback implementations must
// be safe for concurrent use. Calling OnEvict again replaces the callback for
// future removals; passing nil clears it. A removal already in progress may use
// the callback that was current when that removal released the cache lock.
package lru

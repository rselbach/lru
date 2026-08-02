// Package lru provides generic, thread-safe LRU cache implementations.
//
// Four cache types are provided:
//
//   - [Cache]: A standard LRU cache with fixed capacity
//   - [Expirable]: An LRU cache with per-entry TTL expiration
//   - [Sharded]: A sharded LRU cache for reduced lock contention under high concurrency
//   - [Clock]: A sharded cache that approximates LRU so reads scale with cores
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
// # Keys
//
// Keys must be dynamically comparable and equal to themselves. In particular,
// floating-point NaN values and composites containing NaN cannot be stored.
// Set methods panic with [ErrInvalidKey], GetOrSet methods return it, and lookup
// and removal methods treat invalid keys as misses.
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
// Each entry's expiration time is set when written via [Expirable.Set],
// [Expirable.GetOrSet], or [Expirable.GetOrSetSingleflight] and is not extended
// by subsequent reads.
//
// Per-entry TTL can be set using the [WithTTL] option:
//
//	cache.Set("shortLived", 42, lru.WithTTL(30*time.Second))
//	cache.Set("longLived", 100, lru.WithTTL(1*time.Hour))
//
// Concurrent [Expirable.GetOrSetSingleflight] callers that share an in-flight key
// share the leader's computed value and the leader's effective TTL; a waiter's
// [WithTTL] option is not applied. A singleflight compute function must not call
// the same cache's GetOrSetSingleflight method recursively for the same key.
//
// Expired entries are removed lazily on access. They still occupy capacity until
// purged, so a cache full of expired entries must purge on write (automatic when
// a Set needs a slot), via [Expirable.RemoveExpired], or via the optional janitor.
// [Expirable.Len] reports only non-expired entries (O(n)); [Expirable.PhysicalLen]
// reports entries still stored, including expired ones not yet purged (O(1)).
//
// When a write needs capacity and the earliest possible expiry is due, expired
// entries are purged before evicting a non-expired LRU entry. The purge is O(n),
// but full writes remain O(1) while no expiry is due. Applications that want
// periodic background cleanup can opt in with [Expirable.StartJanitor] and stop
// it with [Expirable.StopJanitor]
// (or [Expirable.SignalStopJanitor] from a janitor-driven eviction callback).
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
// Shard selection uses a fast path for built-in strings, integers, floats,
// complex numbers, and bool. Other comparable keys are hashed recursively
// according to Go equality semantics without invoking user-defined formatting.
//
// Sharding only helps when concurrent keys spread across shards. A workload
// dominated by one key sends every operation to the same shard, where the
// hashing is pure overhead and throughput falls below an unsharded [Cache].
// Sharding also costs on a single goroutine, since there is no contention to
// offset the hashing; it starts to pay from roughly two concurrent callers.
//
// [DefaultShardCount] suits moderate concurrency. Throughput keeps improving
// with more shards well past it on machines with many cores, so consider
// [NewShardedWithCount] with a higher count when many goroutines share one
// cache. Shards divide the total capacity, so keep enough capacity per shard
// for the working set; very small shards evict entries a global LRU would
// have kept.
//
// # Clock Cache
//
// A [Clock] cache trades exact LRU order for read throughput. Entries carry a
// reference bit that a hit sets, and eviction advances a hand that clears bits
// and evicts the first entry it finds already clear, so a read reorders nothing
// and needs only a read lock. Like [Sharded] it splits capacity across shards,
// which is what keeps the lock word itself from becoming the bottleneck.
//
// In exchange it keeps no recency order: there is no oldest-entry accessor,
// [Clock.Keys] and [Clock.Values] return entries in an unspecified order, and
// eviction picks an entry that has not been referenced recently rather than
// strictly the least recently used one. Choose [Cache] when order matters and
// Clock when read throughput does.
//
// # Concurrency
//
// Get updates recency, so on [Cache], [Expirable], and [Sharded] it takes the
// exclusive lock and concurrent Get calls serialize against each other.
// [Cache.Peek], [Expirable.Peek], and [Sharded.Peek] take a read lock instead,
// at the cost of not refreshing recency, so reads that do not need recency
// updates scale considerably better.
//
// [Clock.Get] takes only a read lock because it has no order to maintain, which
// is why a Clock cache is the one type whose read throughput rises rather than
// falls as cores are added.
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
//   - Capacity eviction: Cache, Expirable, Sharded, and Clock
//   - [Cache.Remove] / [Expirable.Remove] / [Sharded.Remove] / [Clock.Remove]:
//     yes (Expirable includes already-expired entries still present in storage)
//   - [Cache.RemoveOldest] / [Expirable.RemoveOldest]: yes
//   - [Cache.Clear] / [Expirable.Clear] / [Clock.Clear]: every stored entry,
//     least- to most-recently used for the LRU types, unspecified order for
//     Clock, including unpurged expired entries for Expirable
//   - [Expirable.RemoveExpired], janitor, and capacity expiry cleanup: yes
//   - [Expirable.Set] replacing an already-expired entry: yes for the old value
//   - [Cache.Set] / [Expirable.Set] / [Sharded.Set] / [Clock.Set] replacing a
//     live entry: no; the previous value is discarded without a callback
//   - [Cache.Resize] / [Expirable.Resize] / [Sharded.Resize] / [Clock.Resize]:
//     yes for live evictions (Expirable also reports expired entries purged
//     during resize)
//
// Resize and Clear report evicted entries in order from least recently used to
// most recently used within the cache (or within each shard for [Sharded]);
// [Clock] keeps no such order and reports them in an unspecified one.
// They collect the entries to report while holding the cache lock so the
// callbacks can run without it, which costs one buffered key/value pair per
// evicted entry; clearing a large cache with a callback set allocates in
// proportion to its length.
//
// Callbacks run synchronously after the cache's internal lock is released and
// before the removing method returns. They may be called concurrently from
// multiple goroutines, so callback implementations must be safe for concurrent
// use. Calling OnEvict again replaces the callback for
// future removals; passing nil clears it. A removal already in progress may use
// the callback that was current when that removal released the cache lock.
package lru

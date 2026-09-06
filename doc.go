// Package lru provides generic, thread-safe LRU cache implementations.
//
// Five cache types are provided:
//
//   - [Cache]: A standard LRU cache with fixed capacity
//   - [Expirable]: An LRU cache with per-entry TTL expiration
//   - [Sharded]: A sharded LRU cache for reduced lock contention under high concurrency
//   - [Clock]: A sharded cache that approximates LRU so reads scale with cores
//   - [TinyLFU]: A W-TinyLFU admission cache that resists scans and loops
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
// Set methods panic with [ErrInvalidKey], SetErr and GetOrSet methods return it,
// and lookup and removal methods treat invalid keys as misses.
// Use SetErr when keys come from dynamically typed or otherwise untrusted input:
//
//	if err := cache.SetErr(key, value); err != nil {
//	    // handle ErrInvalidKey
//	}
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
// A compute error is returned to all current callers and is not cached. A panic
// or runtime.Goexit from compute is propagated to all current callers, and a
// later call can retry. Context-aware variants such as
// [Cache.GetOrSetSingleflightContext] let a canceled follower stop waiting
// without canceling the shared computation. The caller that starts a computation
// supplies its context to the compute function.
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
// [Expirable.Get] and [Expirable.GetWithTTL] remove an expired entry encountered
// for their key. Inspection methods such as [Expirable.Peek], [Expirable.Contains],
// [Expirable.Len], [Expirable.Keys], [Expirable.Values], and [Expirable.GetOldest]
// filter or skip expired entries without purging them. Expired entries still
// occupy capacity until purged, so a cache full of expired entries must purge on
// write (automatic when a Set needs a slot), via [Expirable.RemoveExpired], or
// via the optional janitor.
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
// capacity and recency order. Methods such as [Sharded.Keys] return a detached
// copy collected shard by shard, not an atomic snapshot or global recency order.
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
// # TinyLFU Cache
//
// A [TinyLFU] cache puts an admission policy in front of a segmented LRU. New
// entries pass through a small window; when the window overflows, the candidate
// is admitted to the main area only if a compact frequency sketch ranks it
// above the entry it would displace. One-shot keys therefore cannot flush the
// working set, which lifts hit rates on skewed traffic and makes the cache
// resistant to the scans and loops that degrade LRU-family policies, including
// [Clock]. Hits are sampled into a lossy per-shard buffer. Writes and readers
// that acquire the write lock without waiting apply the buffered accesses.
// Applying them never evicts entries or invokes eviction callbacks.
//
// # Choosing a Cache Type
//
// Choose [Cache] for exact LRU order, [Expirable] for TTL expiration, and
// [Sharded] for exact LRU within independent shards. Oldest-entry accessors
// are available on Cache and Expirable.
//
// [Clock] and [TinyLFU] trade exact recency ordering for less work under an
// exclusive lock. Choose Clock for simple approximate eviction. Consider
// TinyLFU when scans or repeated loops evict useful entries: its admission
// policy can preserve them by rejecting less frequently accessed candidates.
// Compare hit rate and total request cost on your workload before choosing
// between them. Neither policy guarantees survival across subsequent writes.
//
// # Concurrency
//
// Get updates recency, so on [Cache], [Expirable], and [Sharded] it takes the
// exclusive lock and concurrent Get calls serialize against each other.
// [Cache.Peek], [Expirable.Peek], and [Sharded.Peek] take a read lock instead,
// at the cost of not refreshing recency, so reads that do not need recency
// updates scale considerably better.
//
// [Clock.Get] uses a read lock. [TinyLFU.Get] looks up entries under a read
// lock and may acquire the write lock without waiting to drain buffered
// accesses. Both avoid updating exact LRU order on every hit.
//
// # Removal Callbacks
//
// Register a callback to be notified when entries leave the cache:
//
//	cache.OnRemove(func(key string, value int, reason lru.RemovalReason) {
//	    fmt.Printf("removed (%s): %s=%d\n", reason, key, value)
//	})
//
// OnEvict is retained for compatibility and reports every entry leaving the
// cache without identifying why. OnRemove reports the same events with a
// [RemovalReason]. If both are registered, OnEvict runs first. Passing nil to
// either registration method clears that callback.
//
// Removal reasons are classified as follows:
//
//   - [RemovalReasonCapacity]: insertion displaced a resident entry because a
//     cache or shard was full. A TinyLFU shard whose main area has zero capacity
//     also uses this reason when its window overflows.
//   - [RemovalReasonExplicit]: Remove, [Cache.RemoveOldest], or
//     [Expirable.RemoveOldest] selected the entry. Removing an already-expired
//     entry by key is still explicit.
//   - [RemovalReasonExpired]: an expiry-aware operation or the janitor purged
//     an expired entry, including [Expirable.Set] replacing an expired value.
//   - [RemovalReasonClear]: Clear removed the entry. Clear reports every stored
//     entry, including unpurged expired entries in Expirable.
//   - [RemovalReasonResize]: shrinking a cache displaced a live entry.
//   - [RemovalReasonAdmission]: TinyLFU rejected the candidate rather than
//     displacing the resident victim.
//
// Callbacks report every stored entry removed by an operation. Clear reports
// entries least- to most-recently used for the LRU types, in unspecified order
// for Clock and TinyLFU, and including unpurged expired entries for Expirable.
// Resize reports live entries evicted by shrinking and, for Expirable, expired
// entries purged by the operation. Set replacing a live value is an update, not
// a removal, and does not invoke either callback.
//
// Resize and Clear report evicted entries in order from least recently used to
// most recently used within the cache (or within each shard for [Sharded]);
// [Clock] and [TinyLFU] keep no such order and report them in an unspecified
// one. Applying [TinyLFU]'s buffered access records never evicts entries,
// so reads never invoke the callback.
// They collect the entries to report while holding the cache lock so the
// callbacks can run without it, which costs one buffered key/value pair per
// evicted entry; clearing a large cache with a callback set allocates in
// proportion to its length.
//
// Callbacks run synchronously after the cache's internal lock is released and
// before the removing method returns. They may be called concurrently from
// multiple goroutines, so callback implementations must be safe for concurrent
// use. Calling either registration method again replaces that callback for
// future removals. A removal already in progress may use the callback that was
// current when that removal released the cache lock.
package lru

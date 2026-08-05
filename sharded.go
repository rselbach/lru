package lru

import (
	"fmt"
	"sync"
)

func wrapShardCountExceedsCapacity(shardCount, capacity int) error {
	return fmt.Errorf("%w: shard count (%d) cannot exceed capacity (%d)",
		ErrShardCountExceedsCapacity, shardCount, capacity)
}

func wrapCapacityBelowShardCount(shardCount int) error {
	return fmt.Errorf("%w (%d)", ErrCapacityBelowShardCount, shardCount)
}

// DefaultShardCount is the default number of shards for a Sharded cache.
//
// It suits moderate concurrency. Throughput keeps improving with more shards
// well past this value on machines with many cores, so use
// [NewShardedWithCount] to raise it when many goroutines share one cache and
// the capacity allows each shard a useful number of entries.
const DefaultShardCount = 16

// Sharded represents a thread-safe, sharded LRU cache.
// It distributes keys across multiple Cache instances to reduce lock contention
// under high concurrency. Each shard is an independent LRU cache with its own lock,
// allowing concurrent operations on different shards.
//
// Sharded is not a global LRU cache. Capacity and recency are enforced per
// shard, so a hot shard can evict entries while another shard has spare room.
// Methods that return collections process shards in order and do not preserve
// global recency across shards.
//
// Sharding pays off only when concurrent keys spread across shards. Traffic
// concentrated on a single key reaches one shard, leaving the hashing as pure
// overhead on top of the same contention an unsharded [Cache] would see, and a
// single-goroutine workload pays that overhead with no contention to offset
// it.
//
// A Sharded must be created with [NewSharded], [MustNewSharded], [NewShardedWithCount],
// or [MustNewShardedWithCount]; the zero value is not ready for use. A Sharded
// must not be copied after first use.
type Sharded[K comparable, V any] struct {
	shards   []*Cache[K, V]
	hasher   shardHasher[K]
	mu       sync.RWMutex // protects capacity updates and serializes Resize
	capacity int          // total capacity across all shards
}

// NewSharded creates a new sharded LRU cache with the given total capacity.
// The capacity is distributed evenly across up to DefaultShardCount shards.
// Smaller caches use one shard per entry so every shard has at least one slot.
// The capacity must be greater than zero.
func NewSharded[K comparable, V any](capacity int) (*Sharded[K, V], error) {
	shardCount := DefaultShardCount
	if capacity > 0 && capacity < shardCount {
		shardCount = capacity
	}
	return NewShardedWithCount[K, V](capacity, shardCount)
}

// MustNewSharded creates a new sharded LRU cache with the given total capacity.
// It panics if the capacity is less than or equal to zero.
func MustNewSharded[K comparable, V any](capacity int) *Sharded[K, V] {
	cache, err := NewSharded[K, V](capacity)
	if err != nil {
		panic(err)
	}
	return cache
}

// NewShardedWithCount creates a new sharded LRU cache with the given total capacity
// and number of shards. The capacity is distributed evenly across all shards.
// Both capacity and shardCount must be greater than zero, and shardCount cannot
// exceed capacity.
//
// Shard selection uses a fast path for built-in strings, integers, floats,
// complex numbers, and bool. Other comparable keys are hashed recursively
// without invoking String or Format methods. Equal keys are always assigned to
// the same shard.
func NewShardedWithCount[K comparable, V any](capacity, shardCount int) (*Sharded[K, V], error) {
	if capacity <= 0 {
		return nil, ErrInvalidCapacity
	}
	if shardCount <= 0 {
		return nil, ErrInvalidShardCount
	}
	if shardCount > capacity {
		return nil, wrapShardCountExceedsCapacity(shardCount, capacity)
	}

	// distribute capacity evenly, with remainder going to first shards
	perShard := capacity / shardCount
	remainder := capacity % shardCount

	shards := make([]*Cache[K, V], shardCount)
	for i := range shards {
		shardCap := perShard
		if i < remainder {
			shardCap++
		}
		shard, err := New[K, V](shardCap)
		if err != nil {
			return nil, err
		}
		shards[i] = shard
	}

	return &Sharded[K, V]{
		shards:   shards,
		hasher:   newShardHasher[K](),
		capacity: capacity,
	}, nil
}

// MustNewShardedWithCount creates a new sharded LRU cache with the given total capacity
// and number of shards. It panics if either value is non-positive or shardCount exceeds capacity.
func MustNewShardedWithCount[K comparable, V any](capacity, shardCount int) *Sharded[K, V] {
	cache, err := NewShardedWithCount[K, V](capacity, shardCount)
	if err != nil {
		panic(err)
	}
	return cache
}

// getShard returns the shard for the given key.
func (s *Sharded[K, V]) getShard(key K) *Cache[K, V] {
	return s.shards[s.hasher.index(key, len(s.shards))]
}

// Get retrieves a value from the cache by key.
// It returns the value and a boolean indicating whether the key was found.
// This method also updates the item's position in the LRU list within its shard.
func (s *Sharded[K, V]) Get(key K) (V, bool) {
	return s.getShard(key).Get(key)
}

// Peek retrieves a value from the cache by key without updating its position
// in the LRU list. This is useful for checking a value without affecting
// eviction order. Returns the value and a boolean indicating whether the key was found.
func (s *Sharded[K, V]) Peek(key K) (V, bool) {
	return s.getShard(key).Peek(key)
}

// GetOrSet retrieves a value from the cache by key, or computes and sets it if not present.
// The compute function is only called if the key is not present in the cache.
// Note: if multiple goroutines call GetOrSet concurrently for the same missing key,
// compute may be called multiple times but only one result will be cached.
// If another goroutine inserts the key while compute runs, that stored value is
// returned and the result of compute is discarded. compute must be safe to abandon
// (no unreclaimed side effects), or use [Sharded.GetOrSetSingleflight].
func (s *Sharded[K, V]) GetOrSet(key K, compute func() (V, error)) (V, error) {
	return s.getShard(key).GetOrSet(key, compute)
}

// GetOrSetSingleflight retrieves a value from the cache by key, or computes and sets it if not present.
// Unlike [Sharded.GetOrSet], if multiple goroutines call GetOrSetSingleflight concurrently for the same
// missing key, the compute function is called exactly once and all callers receive the same result.
// This is useful when the compute function is expensive (e.g., database queries, API calls).
//
// The singleflight deduplication only applies to concurrent in-flight calls; once a value is cached,
// subsequent calls return the cached value without invoking singleflight. compute must not call
// GetOrSetSingleflight recursively for the same key because it would wait on its own call.
func (s *Sharded[K, V]) GetOrSetSingleflight(key K, compute func() (V, error)) (V, error) {
	return s.getShard(key).GetOrSetSingleflight(key, compute)
}

// Set adds or updates an item in the cache.
// If the key already exists, its value is updated without invoking the eviction
// callback for the previous value. If the shard is at capacity, the least
// recently used item in that shard is evicted.
// Set panics with [ErrInvalidKey] if key cannot be represented safely by the cache.
func (s *Sharded[K, V]) Set(key K, value V) {
	s.getShard(key).Set(key, value)
}

// Remove deletes an item from the cache by key.
// It returns whether the key was found and removed.
func (s *Sharded[K, V]) Remove(key K) bool {
	return s.getShard(key).Remove(key)
}

// Len returns the current number of items in the cache across all shards.
// The result is collected shard by shard and is not atomic with respect to
// concurrent updates across shards.
func (s *Sharded[K, V]) Len() int {
	total := 0
	for _, shard := range s.shards {
		total += shard.Len()
	}
	return total
}

// Clear removes all items from all shards.
// Shards are cleared one at a time; the operation is not atomic across shards.
func (s *Sharded[K, V]) Clear() {
	for _, shard := range s.shards {
		shard.Clear()
	}
}

// Contains checks if a key exists in the cache.
func (s *Sharded[K, V]) Contains(key K) bool {
	return s.getShard(key).Contains(key)
}

// Keys returns a slice of all keys in the cache.
// The order is from most recently used to least recently used within each shard,
// with shards processed in order. Note that the global LRU order is not preserved
// across shards.
//
// The result is a detached copy collected shard by shard and is not atomic with
// respect to concurrent updates.
func (s *Sharded[K, V]) Keys() []K {
	keys := make([]K, 0, s.Len())
	for _, shard := range s.shards {
		keys = shard.appendKeys(keys)
	}
	return keys
}

// Values returns a slice of all values in the cache.
// The order matches [Sharded.Keys]: most recently used to least recently used
// within each shard, with shards processed in order. Note that the global LRU
// order is not preserved across shards.
//
// The result is a detached copy collected shard by shard and is not atomic with
// respect to concurrent updates. Keys and Values are separate copies, so their
// elements line up only when no other goroutine writes to the cache between the
// two calls.
func (s *Sharded[K, V]) Values() []V {
	values := make([]V, 0, s.Len())
	for _, shard := range s.shards {
		values = shard.appendValues(values)
	}
	return values
}

// Items returns key/value pairs in most-recently-used to least-recently-used
// order within each shard, with shards processed in order. Each pair is captured
// under one shard lock, but the aggregate is not an atomic cache-wide snapshot.
func (s *Sharded[K, V]) Items() []Item[K, V] {
	items := make([]Item[K, V], 0, s.Len())
	for _, shard := range s.shards {
		items = shard.appendItems(items)
	}
	return items
}

// Capacity returns the maximum total capacity of the cache.
func (s *Sharded[K, V]) Capacity() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.capacity
}

// ShardCount returns the number of shards in the cache.
func (s *Sharded[K, V]) ShardCount() int {
	return len(s.shards)
}

type shardedEviction[K comparable, V any] struct {
	onEvict OnEvictFunc[K, V]
	key     K
	value   V
}

// Resize changes the maximum total capacity of the cache while preserving the
// existing shard count. The capacity is redistributed across shards using the
// same even distribution as construction, with any remainder assigned to the
// first shards. The new capacity must be at least the current shard count so
// every shard keeps at least one slot.
func (s *Sharded[K, V]) Resize(capacity int) (int, error) {
	if capacity <= 0 {
		return 0, ErrInvalidCapacity
	}

	shardCount := len(s.shards)
	if capacity < shardCount {
		return 0, wrapCapacityBelowShardCount(shardCount)
	}

	s.mu.Lock()
	perShard := capacity / shardCount
	remainder := capacity % shardCount
	evicted := 0
	var evictions []shardedEviction[K, V]

	for i, shard := range s.shards {
		shardCap := perShard
		if i < remainder {
			shardCap++
		}
		shard.mu.Lock()
		onEvict := shard.onEvict
		removed, shardEvicted := shard.resizeLocked(shardCap, onEvict != nil)
		shard.mu.Unlock()

		evicted += shardEvicted
		if onEvict != nil {
			for _, e := range removed {
				evictions = append(evictions, shardedEviction[K, V]{
					onEvict: onEvict,
					key:     e.key,
					value:   e.val,
				})
			}
		}
	}

	s.capacity = capacity
	s.mu.Unlock()

	for _, eviction := range evictions {
		eviction.onEvict(eviction.key, eviction.value)
	}

	return evicted, nil
}

// OnEvict sets a callback function that will be called when an entry is evicted
// from any shard. The callback will receive the key and value of the evicted entry.
//
// Calling OnEvict again replaces the callback used by all shards for future
// removals. Passing nil clears the callback. A removal already in progress may
// still invoke the callback that was current when that shard released its lock.
//
// OnEvict serializes against [Sharded.Resize] and concurrent OnEvict calls, and
// updates shards one at a time while holding the sharded cache lock, so a swap
// under load can briefly stall shard operations.
//
// The callback runs synchronously after the relevant shard lock is released and
// before the removing method returns. It may be invoked concurrently from multiple
// shards and goroutines, so it must be safe for concurrent use.
func (s *Sharded[K, V]) OnEvict(f OnEvictFunc[K, V]) {
	// Serialize against Resize and concurrent OnEvict so all shards observe
	// the same callback; in-flight evictions may still use a prior callback.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, shard := range s.shards {
		shard.OnEvict(f)
	}
}

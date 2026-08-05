package lru

import (
	"context"
	"sync"
	"sync/atomic"
)

// Clock is a thread-safe, fixed-size cache that approximates LRU eviction with
// the CLOCK (second-chance) policy and is sharded to reduce lock contention.
//
// Unlike [Cache], a read does not reorder anything. Each entry carries a
// reference bit that a hit sets, and eviction advances a hand that clears bits
// and evicts the first entry it finds already clear. Reads therefore take only
// a read lock, and an entry whose bit is already set is not written at all,
// which is what lets read throughput rise as cores are added instead of
// collapsing under an exclusive lock.
//
// The cost is that Clock keeps no recency order. It cannot report a least
// recently used entry, [Clock.Keys] and [Clock.Values] return entries in an
// unspecified order, and eviction picks an entry that has not been referenced
// recently rather than strictly the oldest one. Use [Cache] when exact LRU
// order or [Cache.GetOldest] matters, and Clock when read throughput does.
//
// Capacity and eviction are enforced per shard, as in [Sharded]: a hot shard
// can evict while another has spare room, and [Clock.Len], [Clock.Clear] and
// [Clock.OnEvict] apply shard by shard rather than atomically across the cache.
//
// A Clock must be created with [NewClock], [MustNewClock], [NewClockWithCount],
// or [MustNewClockWithCount]; the zero value is not ready for use. A Clock must
// not be copied after first use.
type Clock[K comparable, V any] struct {
	shards   []*clockShard[K, V]
	hasher   shardHasher[K]
	mu       sync.RWMutex // protects capacity updates and serializes Resize
	capacity int          // total capacity across all shards
}

// clockEntry is one slot in a shard's ring. ref is accessed atomically because
// concurrent readers holding the shard's read lock may set it at the same time.
type clockEntry[K comparable, V any] struct {
	key K
	val V
	ref int32
	idx int // position in the shard ring, for O(1) removal
}

type clockShard[K comparable, V any] struct {
	mu       sync.RWMutex
	items    map[K]*clockEntry[K, V]
	ring     []*clockEntry[K, V] // slots, nil where an entry was removed
	free     []int               // indexes of nil slots available for reuse
	hand     int
	capacity int
	onEvict  OnEvictFunc[K, V]
	sfGroup  flightGroup[K, V]
}

// NewClock creates a new CLOCK cache with the given total capacity.
// The capacity is distributed evenly across up to DefaultShardCount shards.
// Smaller caches use one shard per entry so every shard has at least one slot.
// The capacity must be greater than zero.
func NewClock[K comparable, V any](capacity int) (*Clock[K, V], error) {
	shardCount := DefaultShardCount
	if capacity > 0 && capacity < shardCount {
		shardCount = capacity
	}
	return NewClockWithCount[K, V](capacity, shardCount)
}

// MustNewClock creates a new CLOCK cache with the given total capacity.
// It panics if the capacity is less than or equal to zero.
func MustNewClock[K comparable, V any](capacity int) *Clock[K, V] {
	cache, err := NewClock[K, V](capacity)
	if err != nil {
		panic(err)
	}
	return cache
}

// NewClockWithCount creates a new CLOCK cache with the given total capacity and
// number of shards. Both must be greater than zero, and shardCount cannot
// exceed capacity.
//
// Read throughput improves with more shards well past [DefaultShardCount] on
// machines with many cores, so raising it is worthwhile when many goroutines
// share one cache and the capacity leaves each shard a useful number of slots.
func NewClockWithCount[K comparable, V any](capacity, shardCount int) (*Clock[K, V], error) {
	if capacity <= 0 {
		return nil, ErrInvalidCapacity
	}
	if shardCount <= 0 {
		return nil, ErrInvalidShardCount
	}
	if shardCount > capacity {
		return nil, wrapShardCountExceedsCapacity(shardCount, capacity)
	}

	perShard := capacity / shardCount
	remainder := capacity % shardCount

	shards := make([]*clockShard[K, V], shardCount)
	skipKeyCheck := !keysNeedValidation[K]()
	for i := range shards {
		shardCap := perShard
		if i < remainder {
			shardCap++
		}
		shards[i] = &clockShard[K, V]{
			items:    make(map[K]*clockEntry[K, V], allocationHint(shardCap)),
			ring:     make([]*clockEntry[K, V], 0, allocationHint(shardCap)),
			capacity: shardCap,
			sfGroup:  flightGroup[K, V]{skipKeyCheck: skipKeyCheck},
		}
	}

	return &Clock[K, V]{
		shards:   shards,
		hasher:   newShardHasher[K](),
		capacity: capacity,
	}, nil
}

// MustNewClockWithCount creates a new CLOCK cache with the given total capacity
// and number of shards. It panics if either value is non-positive or shardCount
// exceeds capacity.
func MustNewClockWithCount[K comparable, V any](capacity, shardCount int) *Clock[K, V] {
	cache, err := NewClockWithCount[K, V](capacity, shardCount)
	if err != nil {
		panic(err)
	}
	return cache
}

func (c *Clock[K, V]) getShard(key K) *clockShard[K, V] {
	return c.shards[c.hasher.index(key, len(c.shards))]
}

// Get retrieves a value from the cache by key. It returns the value and a
// boolean indicating whether the key was found.
//
// Get marks the entry as recently referenced, which protects it from the next
// pass of the eviction hand. It does not reorder anything, so it takes only a
// read lock and concurrent Get calls on different shards do not serialize.
func (c *Clock[K, V]) Get(key K) (V, bool) {
	var zero V
	if !c.hasher.skipKeyCheck && validateKey(key) != nil {
		return zero, false
	}
	return c.getShard(key).get(key)
}

func (s *clockShard[K, V]) get(key K) (V, bool) {
	s.mu.RLock()
	e, found := s.items[key]
	if !found {
		s.mu.RUnlock()
		var zero V
		return zero, false
	}
	val := e.val
	// Read before write: an entry that is already referenced needs no store,
	// which keeps a hot entry's cache line shared instead of bouncing it
	// between cores on every hit.
	if atomic.LoadInt32(&e.ref) == 0 {
		atomic.StoreInt32(&e.ref, 1)
	}
	s.mu.RUnlock()
	return val, true
}

// Peek retrieves a value from the cache by key without marking it as recently
// referenced, leaving it as exposed to the next pass of the eviction hand as it
// was before. Returns the value and whether the key was found.
func (c *Clock[K, V]) Peek(key K) (V, bool) {
	var zero V
	if !c.hasher.skipKeyCheck && validateKey(key) != nil {
		return zero, false
	}

	s := c.getShard(key)
	s.mu.RLock()
	defer s.mu.RUnlock()

	e, found := s.items[key]
	if !found {
		return zero, false
	}
	return e.val, true
}

// Contains reports whether a key is present, without marking it as recently
// referenced.
func (c *Clock[K, V]) Contains(key K) bool {
	if !c.hasher.skipKeyCheck && validateKey(key) != nil {
		return false
	}

	s := c.getShard(key)
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, found := s.items[key]
	return found
}

// Set adds or updates an item in the cache and marks it as recently referenced.
// If the key already exists, its value is updated without invoking the eviction
// callback for the previous value. If the shard is full, an entry that has not
// been referenced since the hand last passed it is evicted.
// Set panics with [ErrInvalidKey] if key cannot be represented safely.
func (c *Clock[K, V]) Set(key K, value V) {
	if !c.hasher.skipKeyCheck {
		if err := validateKey(key); err != nil {
			panic(err)
		}
	}

	s := c.getShard(key)
	s.mu.Lock()
	onEvict := s.onEvict
	evictedKey, evictedVal, evicted := s.setLocked(key, value)
	s.mu.Unlock()

	if evicted && onEvict != nil {
		onEvict(evictedKey, evictedVal)
	}
}

// setLocked inserts or updates key. Caller must hold s.mu.
// Returns the evicted key/value and whether an eviction occurred.
func (s *clockShard[K, V]) setLocked(key K, value V) (K, V, bool) {
	var zeroK K
	var zeroV V

	if e, found := s.items[key]; found {
		e.val = value
		atomic.StoreInt32(&e.ref, 1)
		return zeroK, zeroV, false
	}

	// A free slot means the ring has room without evicting anything.
	if len(s.ring) < s.capacity {
		e := &clockEntry[K, V]{key: key, val: value, ref: 1, idx: len(s.ring)}
		s.ring = append(s.ring, e)
		s.items[key] = e
		return zeroK, zeroV, false
	}
	if n := len(s.free); n > 0 {
		idx := s.free[n-1]
		s.free = s.free[:n-1]
		e := &clockEntry[K, V]{key: key, val: value, ref: 1, idx: idx}
		s.ring[idx] = e
		s.items[key] = e
		return zeroK, zeroV, false
	}

	// Advance the hand until it finds an entry that has not been referenced
	// since it last passed, clearing the bits it steps over. The ring holds no
	// nil slots here, and each pass clears at least one bit, so this terminates
	// within two laps.
	for {
		victim := s.ring[s.hand]
		s.hand++
		if s.hand == len(s.ring) {
			s.hand = 0
		}

		if atomic.LoadInt32(&victim.ref) != 0 {
			atomic.StoreInt32(&victim.ref, 0)
			continue
		}

		evictedKey := victim.key
		evictedVal := victim.val
		delete(s.items, evictedKey)

		victim.key = key
		victim.val = value
		atomic.StoreInt32(&victim.ref, 1)
		s.items[key] = victim
		return evictedKey, evictedVal, true
	}
}

// Remove deletes an item from the cache by key.
// It returns whether the key was found and removed.
func (c *Clock[K, V]) Remove(key K) bool {
	if !c.hasher.skipKeyCheck && validateKey(key) != nil {
		return false
	}

	s := c.getShard(key)
	s.mu.Lock()
	e, found := s.items[key]
	if !found {
		s.mu.Unlock()
		return false
	}

	evictedKey := e.key
	evictedVal := e.val
	onEvict := s.onEvict
	s.removeLocked(e)
	s.mu.Unlock()

	if onEvict != nil {
		onEvict(evictedKey, evictedVal)
	}
	return true
}

// removeLocked drops an entry and returns its ring slot to the free list.
// Caller must hold s.mu.
func (s *clockShard[K, V]) removeLocked(e *clockEntry[K, V]) {
	delete(s.items, e.key)
	s.ring[e.idx] = nil
	s.free = append(s.free, e.idx)
}

// GetOrSet retrieves a value from the cache by key, or computes and sets it if
// not present. The compute function is only called if the key is not present.
// Note: if multiple goroutines call GetOrSet concurrently for the same missing
// key, compute may be called multiple times but only one result will be cached.
// If another goroutine inserts the key while compute runs, that stored value is
// returned and the result of compute is discarded. compute must be safe to
// abandon (no unreclaimed side effects), or use [Clock.GetOrSetSingleflight].
func (c *Clock[K, V]) GetOrSet(key K, compute func() (V, error)) (V, error) {
	if !c.hasher.skipKeyCheck {
		if err := validateKey(key); err != nil {
			var zero V
			return zero, err
		}
	}

	s := c.getShard(key)
	if val, found := s.get(key); found {
		return val, nil
	}
	return s.computeAndSet(key, compute)
}

// GetOrSetSingleflight retrieves a value from the cache by key, or computes and
// sets it if not present. Unlike [Clock.GetOrSet], concurrent callers for the
// same missing key call compute exactly once and all receive the same result.
//
// The deduplication only applies to concurrent in-flight calls; once a value is
// cached, subsequent calls return it without invoking singleflight. compute must
// not call GetOrSetSingleflight recursively for the same key because it would
// wait on its own call.
// A compute error is returned to all current callers and is not cached. A panic
// or runtime.Goexit from compute is propagated to all current callers.
func (c *Clock[K, V]) GetOrSetSingleflight(key K, compute func() (V, error)) (V, error) {
	return c.getOrSetSingleflight(nil, key, compute)
}

// GetOrSetSingleflightContext behaves like [Clock.GetOrSetSingleflight], with
// context cancellation for the computation and its waiters. The caller that
// starts the computation supplies the context passed to compute. A follower can
// stop waiting when its own context is canceled without canceling the shared
// computation, which may still cache its result. The method panics if ctx is nil.
func (c *Clock[K, V]) GetOrSetSingleflightContext(
	ctx context.Context,
	key K,
	compute func(context.Context) (V, error),
) (V, error) {
	if ctx == nil {
		panic("lru: nil Context")
	}
	return c.getOrSetSingleflight(ctx, key, func() (V, error) {
		return compute(ctx)
	})
}

func (c *Clock[K, V]) getOrSetSingleflight(
	ctx context.Context,
	key K,
	compute func() (V, error),
) (V, error) {
	if !c.hasher.skipKeyCheck {
		if err := validateKey(key); err != nil {
			var zero V
			return zero, err
		}
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			var zero V
			return zero, err
		}
	}

	s := c.getShard(key)
	if val, found := s.get(key); found {
		return val, nil
	}

	result, err := s.sfGroup.do(ctx, key, func() (V, error) {
		if val, found := s.get(key); found {
			return val, nil
		}
		return s.computeAndSet(key, compute)
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return result, nil
}

// computeAndSet runs compute outside the shard lock, then stores the result
// unless another goroutine cached the key first.
func (s *clockShard[K, V]) computeAndSet(key K, compute func() (V, error)) (V, error) {
	val, err := compute()
	if err != nil {
		var zero V
		return zero, err
	}

	s.mu.Lock()
	if e, found := s.items[key]; found {
		existing := e.val
		atomic.StoreInt32(&e.ref, 1)
		s.mu.Unlock()
		return existing, nil
	}

	onEvict := s.onEvict
	evictedKey, evictedVal, evicted := s.setLocked(key, val)
	s.mu.Unlock()

	if evicted && onEvict != nil {
		onEvict(evictedKey, evictedVal)
	}
	return val, nil
}

// Len returns the current number of items across all shards.
// The result is collected shard by shard and is not atomic with respect to
// concurrent updates across shards.
func (c *Clock[K, V]) Len() int {
	total := 0
	for _, s := range c.shards {
		s.mu.RLock()
		total += len(s.items)
		s.mu.RUnlock()
	}
	return total
}

// Capacity returns the maximum total capacity of the cache.
func (c *Clock[K, V]) Capacity() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.capacity
}

// ShardCount returns the number of shards in the cache.
func (c *Clock[K, V]) ShardCount() int {
	return len(c.shards)
}

// Keys returns a slice of all keys in the cache.
//
// The order is unspecified: Clock keeps no recency order, and shards are
// processed independently. The result is a detached copy collected shard by
// shard and is not atomic with respect to concurrent updates.
func (c *Clock[K, V]) Keys() []K {
	keys := make([]K, 0, c.Len())
	for _, s := range c.shards {
		s.mu.RLock()
		for _, e := range s.ring {
			if e != nil {
				keys = append(keys, e.key)
			}
		}
		s.mu.RUnlock()
	}
	return keys
}

// Values returns a slice of all values in the cache.
//
// The order is unspecified, as for [Clock.Keys]. Keys and Values are separate
// copies collected under separate lock holds, so their elements line up only
// when no other goroutine writes to the cache between the two calls.
func (c *Clock[K, V]) Values() []V {
	values := make([]V, 0, c.Len())
	for _, s := range c.shards {
		s.mu.RLock()
		for _, e := range s.ring {
			if e != nil {
				values = append(values, e.val)
			}
		}
		s.mu.RUnlock()
	}
	return values
}

// Items returns key/value pairs in unspecified order. Each pair is captured
// under one shard lock, but the aggregate is not an atomic cache-wide snapshot.
func (c *Clock[K, V]) Items() []Item[K, V] {
	items := make([]Item[K, V], 0, c.Len())
	for _, s := range c.shards {
		s.mu.RLock()
		for _, e := range s.ring {
			if e != nil {
				items = append(items, Item[K, V]{Key: e.key, Value: e.val})
			}
		}
		s.mu.RUnlock()
	}
	return items
}

// Clear removes all items from all shards.
// Shards are cleared one at a time; the operation is not atomic across shards.
//
// If an eviction callback is set, it is called for every stored entry in
// unspecified order. The entries are buffered while the lock is held so the
// callbacks can run without it, so clearing a large cache with a callback set
// allocates one key/value pair per entry.
func (c *Clock[K, V]) Clear() {
	for _, s := range c.shards {
		s.mu.Lock()
		onEvict := s.onEvict

		var evicted []evictedItem[K, V]
		if onEvict != nil {
			evicted = make([]evictedItem[K, V], 0, len(s.items))
			for _, e := range s.ring {
				if e != nil {
					evicted = append(evicted, evictedItem[K, V]{key: e.key, val: e.val})
				}
			}
		}

		s.items = make(map[K]*clockEntry[K, V], allocationHint(s.capacity))
		// The ring keeps its backing array for reuse, so clear its pointer slots
		// before truncating it. Otherwise every removed key and value remains
		// reachable until a later insertion overwrites the corresponding slot.
		for i := range s.ring {
			s.ring[i] = nil
		}
		s.ring = s.ring[:0]
		s.free = s.free[:0]
		s.hand = 0
		s.mu.Unlock()

		for _, e := range evicted {
			onEvict(e.key, e.val)
		}
	}
}

// Resize changes the maximum total capacity of the cache while preserving the
// existing shard count. The capacity is redistributed across shards using the
// same even distribution as construction, with any remainder assigned to the
// first shards. The new capacity must be at least the current shard count so
// every shard keeps at least one slot.
//
// Shrinking evicts entries that have not been referenced recently, in
// unspecified order, and reports them to the eviction callback. It returns the
// number of entries evicted.
func (c *Clock[K, V]) Resize(capacity int) (int, error) {
	if capacity <= 0 {
		return 0, ErrInvalidCapacity
	}

	shardCount := len(c.shards)
	if capacity < shardCount {
		return 0, wrapCapacityBelowShardCount(shardCount)
	}

	c.mu.Lock()
	perShard := capacity / shardCount
	remainder := capacity % shardCount
	evicted := 0
	var evictions []shardedEviction[K, V]

	for i, s := range c.shards {
		shardCap := perShard
		if i < remainder {
			shardCap++
		}

		s.mu.Lock()
		onEvict := s.onEvict
		removed, count := s.resizeLocked(shardCap, onEvict != nil)
		s.mu.Unlock()

		evicted += count
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

	c.capacity = capacity
	c.mu.Unlock()

	for _, eviction := range evictions {
		eviction.onEvict(eviction.key, eviction.value)
	}

	return evicted, nil
}

// resizeLocked shrinks the shard to capacity and compacts its ring. It returns
// the entries collected for the eviction callback and the eviction count, which
// is tracked separately so the count stays right when no callback is set.
// Caller must hold s.mu.
func (s *clockShard[K, V]) resizeLocked(capacity int, collect bool) ([]evictedItem[K, V], int) {
	var evicted []evictedItem[K, V]
	count := 0

	for len(s.items) > capacity {
		victim := s.ring[s.hand]
		s.hand++
		if s.hand == len(s.ring) {
			s.hand = 0
		}
		if victim == nil {
			continue
		}
		if atomic.LoadInt32(&victim.ref) != 0 {
			atomic.StoreInt32(&victim.ref, 0)
			continue
		}
		if collect {
			evicted = append(evicted, evictedItem[K, V]{key: victim.key, val: victim.val})
		}
		s.removeLocked(victim)
		count++
	}

	// Compact the survivors so the ring never exceeds the new capacity and the
	// free list stays consistent with it.
	compacted := s.ring[:0]
	for _, e := range s.ring {
		if e != nil {
			e.idx = len(compacted)
			compacted = append(compacted, e)
		}
	}
	s.ring = compacted
	s.free = s.free[:0]
	s.hand = 0
	s.capacity = capacity
	return evicted, count
}

// OnEvict sets a callback invoked when an entry leaves the cache, receiving its
// key and value. It fires for capacity eviction, [Clock.Remove], [Clock.Clear]
// and shrinking [Clock.Resize], but not when [Clock.Set] replaces the value of
// an existing key.
//
// Calling OnEvict again replaces the callback used by all shards for future
// removals. Passing nil clears it. A removal already in progress may still
// invoke the callback that was current when that shard released its lock.
//
// OnEvict serializes against [Clock.Resize] and concurrent OnEvict calls, and
// updates shards one at a time while holding the cache lock, so a swap under
// load can briefly stall shard operations.
//
// The callback runs synchronously after the relevant shard lock is released and
// before the removing method returns. It may be invoked concurrently from
// multiple shards and goroutines, so it must be safe for concurrent use.
func (c *Clock[K, V]) OnEvict(f OnEvictFunc[K, V]) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, s := range c.shards {
		s.mu.Lock()
		s.onEvict = f
		s.mu.Unlock()
	}
}

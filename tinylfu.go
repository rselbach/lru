package lru

import (
	"sync"
	"sync/atomic"
)

const (
	// tinySamplePeriod thins access recording: an entry records its first hit
	// and then every sixteenth, and misses are sampled at the same rate on the
	// write path. The policy simulator showed this costs under half a point of
	// hit rate while removing sixteen-seventeenths of the shared writes a read
	// would otherwise perform.
	tinySamplePeriod = 16
	tinySampleMask   = tinySamplePeriod - 1

	// tinyReadBufferSize is the per-shard lossy read buffer, a power of two.
	// Records dropped on a full or contended buffer are acceptable losses:
	// access records are hints, and popular entries get many chances.
	tinyReadBufferSize = 128
	tinyReadBufferMask = tinyReadBufferSize - 1

	// tinyDrainThreshold is the buffer depth at which a reader volunteers to
	// drain if the write lock is free.
	tinyDrainThreshold = tinyReadBufferSize / 2
)

// segments of a TinyLFU shard.
const (
	tinyWindow int8 = iota
	tinyProbation
	tinyProtected
)

// TinyLFU is a thread-safe, fixed-size cache using the W-TinyLFU policy
// (Einziger, Friedman, and Manes: "TinyLFU: A Highly Efficient Cache Admission
// Policy"), sharded to reduce lock contention.
//
// W-TinyLFU decides what may enter the cache rather than only what leaves it.
// New entries start in a small LRU window; when the window overflows, the
// evicted candidate is admitted to the main segmented-LRU area only if a
// compact frequency sketch estimates it is accessed more often than the entry
// it would displace. One-shot keys therefore cannot flush the working set,
// which makes TinyLFU resistant to scans and loops that degrade [Cache] and
// [Clock], while also improving hit rates on skewed workloads.
//
// Reads take only a shard read lock. A hit records itself into a small lossy
// per-shard buffer, sampled so most hits perform no shared write at all, and a
// later write (or an uncontended volunteer reader) drains the buffer into the
// policy. Recency and frequency bookkeeping is therefore approximate and
// slightly deferred; eviction decisions and callbacks happen only on writes,
// never inside a read.
//
// Like [Clock], TinyLFU keeps no global recency order: there is no oldest-entry
// accessor, and [TinyLFU.Keys] and [TinyLFU.Values] return entries in an
// unspecified order. Capacity is enforced per shard, so a hot shard can evict
// while another has spare room.
//
// A TinyLFU must be created with [NewTinyLFU], [MustNewTinyLFU],
// [NewTinyLFUWithCount], or [MustNewTinyLFUWithCount]; the zero value is not
// ready for use. A TinyLFU must not be copied after first use.
type TinyLFU[K comparable, V any] struct {
	shards   []*tinyShard[K, V]
	hasher   shardHasher[K]
	mu       sync.RWMutex // protects capacity updates and serializes Resize
	capacity int          // total capacity across all shards
}

// tinyNode is one cache entry. hits is accessed atomically because concurrent
// readers holding the shard's read lock increment it at the same time.
type tinyNode[K comparable, V any] struct {
	key  K
	val  V
	hash uint64
	// hits counts accesses since the entry entered its segment; a reader
	// records the access when the count crosses the sampling boundary.
	hits    uint32
	segment int8
	// dead marks entries removed from the map so a stale read-buffer record
	// cannot resurrect them during a drain.
	dead bool
	prev *tinyNode[K, V]
	next *tinyNode[K, V]
}

// tinyList is an intrusive doubly-linked list; head is most recently used.
type tinyList[K comparable, V any] struct {
	head *tinyNode[K, V]
	tail *tinyNode[K, V]
	len  int
}

func (l *tinyList[K, V]) pushFront(n *tinyNode[K, V]) {
	n.prev = nil
	n.next = l.head
	if l.head != nil {
		l.head.prev = n
	}
	l.head = n
	if l.tail == nil {
		l.tail = n
	}
	l.len++
}

func (l *tinyList[K, V]) remove(n *tinyNode[K, V]) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		l.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		l.tail = n.prev
	}
	n.prev = nil
	n.next = nil
	l.len--
}

func (l *tinyList[K, V]) moveToFront(n *tinyNode[K, V]) {
	if l.head == n {
		return
	}
	l.remove(n)
	l.pushFront(n)
}

type tinyShard[K comparable, V any] struct {
	mu    sync.RWMutex
	items map[K]*tinyNode[K, V]

	window    tinyList[K, V]
	probation tinyList[K, V]
	protected tinyList[K, V]

	sketch *frequencySketch

	windowCap    int
	mainCap      int
	protectedCap int

	// tick samples miss recording on the write path at the same rate as hits,
	// so candidate and victim frequencies stay on one scale.
	tick uint64

	onEvict OnEvictFunc[K, V]
	sfGroup flightGroup[K, V]

	// Lossy read buffer. bufTail is advanced by readers with a single CAS
	// attempt; bufHead is advanced only by drainLocked under the write lock,
	// which readers are excluded from, so each claimed slot is written exactly
	// once before it is drained.
	bufHead uint32
	bufTail uint32
	buffer  [tinyReadBufferSize]*tinyNode[K, V]
}

// NewTinyLFU creates a new W-TinyLFU cache with the given total capacity.
// The capacity is distributed evenly across up to DefaultShardCount shards.
// Smaller caches use one shard per entry so every shard has at least one slot.
// The capacity must be greater than zero.
func NewTinyLFU[K comparable, V any](capacity int) (*TinyLFU[K, V], error) {
	shardCount := DefaultShardCount
	if capacity > 0 && capacity < shardCount {
		shardCount = capacity
	}
	return NewTinyLFUWithCount[K, V](capacity, shardCount)
}

// MustNewTinyLFU creates a new W-TinyLFU cache with the given total capacity.
// It panics if the capacity is less than or equal to zero.
func MustNewTinyLFU[K comparable, V any](capacity int) *TinyLFU[K, V] {
	cache, err := NewTinyLFU[K, V](capacity)
	if err != nil {
		panic(err)
	}
	return cache
}

// NewTinyLFUWithCount creates a new W-TinyLFU cache with the given total
// capacity and number of shards. Both must be greater than zero, and shardCount
// cannot exceed capacity.
//
// Within each shard roughly 1% of the capacity (at least one slot) forms the
// admission window and the rest the main area, of which 80% is the protected
// segment, following the W-TinyLFU paper's defaults. It returns
// [ErrCapacityTooLarge] if the per-shard capacity cannot be represented safely
// by the frequency sketch.
func NewTinyLFUWithCount[K comparable, V any](capacity, shardCount int) (*TinyLFU[K, V], error) {
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
	largestShard := perShard
	if remainder > 0 {
		largestShard++
	}
	if !validFrequencySketchCapacity(largestShard) {
		return nil, ErrCapacityTooLarge
	}

	shards := make([]*tinyShard[K, V], shardCount)
	skipKeyCheck := !keysNeedValidation[K]()
	for i := range shards {
		shardCap := perShard
		if i < remainder {
			shardCap++
		}
		s := &tinyShard[K, V]{
			items:   make(map[K]*tinyNode[K, V], allocationHint(shardCap)),
			sketch:  newFrequencySketch(shardCap),
			sfGroup: flightGroup[K, V]{skipKeyCheck: skipKeyCheck},
		}
		s.setCapsLocked(shardCap)
		shards[i] = s
	}

	return &TinyLFU[K, V]{
		shards:   shards,
		hasher:   newShardHasher[K](),
		capacity: capacity,
	}, nil
}

// MustNewTinyLFUWithCount creates a new W-TinyLFU cache with the given total
// capacity and number of shards. It panics if either value is non-positive or
// shardCount exceeds capacity.
func MustNewTinyLFUWithCount[K comparable, V any](capacity, shardCount int) *TinyLFU[K, V] {
	cache, err := NewTinyLFUWithCount[K, V](capacity, shardCount)
	if err != nil {
		panic(err)
	}
	return cache
}

// setCapsLocked derives the window and segment bounds from a shard capacity.
func (s *tinyShard[K, V]) setCapsLocked(shardCap int) {
	windowCap := shardCap / 100
	if windowCap < 1 {
		windowCap = 1
	}
	s.windowCap = windowCap
	s.mainCap = shardCap - windowCap
	s.protectedCap = s.mainCap * 80 / 100
}

func (s *tinyShard[K, V]) capacity() int {
	return s.windowCap + s.mainCap
}

// shardFor returns the shard for a key along with the key's hash, so callers
// hash exactly once. The key must already have been validated.
func (c *TinyLFU[K, V]) shardFor(key K) (*tinyShard[K, V], uint64) {
	h := c.hasher.hash(key)
	return c.shards[h%uint64(len(c.shards))], h
}

// Get retrieves a value from the cache by key. It returns the value and a
// boolean indicating whether the key was found.
//
// Get takes only a read lock. The access is recorded into a sampled, lossy
// per-shard buffer and applied to the eviction policy by a later write, so a
// hit improves the entry's standing without reordering anything inline. Reads
// never invoke the eviction callback.
func (c *TinyLFU[K, V]) Get(key K) (V, bool) {
	var zero V
	if !c.hasher.skipKeyCheck && validateKey(key) != nil {
		return zero, false
	}
	s, _ := c.shardFor(key)
	return s.get(key)
}

func (s *tinyShard[K, V]) get(key K) (V, bool) {
	s.mu.RLock()
	n, ok := s.items[key]
	if !ok {
		s.mu.RUnlock()
		var zero V
		return zero, false
	}
	val := n.val
	recorded := false
	if atomic.AddUint32(&n.hits, 1)&tinySampleMask == 1 {
		s.appendRead(n)
		recorded = true
	}
	s.mu.RUnlock()

	if recorded &&
		atomic.LoadUint32(&s.bufTail)-atomic.LoadUint32(&s.bufHead) >= tinyDrainThreshold {
		s.tryDrain()
	}
	return val, true
}

// appendRead records n in the lossy read buffer. The caller holds the read
// lock, which excludes drains, so the claimed slot cannot be consumed before it
// is written. A full buffer or a lost CAS drops the record.
func (s *tinyShard[K, V]) appendRead(n *tinyNode[K, V]) {
	head := atomic.LoadUint32(&s.bufHead)
	tail := atomic.LoadUint32(&s.bufTail)
	if tail-head >= tinyReadBufferSize {
		return
	}
	if !atomic.CompareAndSwapUint32(&s.bufTail, tail, tail+1) {
		return
	}
	s.buffer[tail&tinyReadBufferMask] = n
}

// tryDrain applies buffered reads if the write lock is free, and otherwise
// leaves them for the next writer.
func (s *tinyShard[K, V]) tryDrain() {
	if s.mu.TryLock() {
		s.drainLocked()
		s.mu.Unlock()
	}
}

// drainLocked applies buffered access records to the policy. Records feed the
// sketch and reorder segments; draining never evicts, so reads never trigger
// eviction callbacks. Caller must hold the write lock.
func (s *tinyShard[K, V]) drainLocked() {
	tail := atomic.LoadUint32(&s.bufTail)
	for i := atomic.LoadUint32(&s.bufHead); i != tail; i++ {
		n := s.buffer[i&tinyReadBufferMask]
		s.buffer[i&tinyReadBufferMask] = nil
		if n == nil || n.dead {
			continue
		}
		s.sketch.increment(n.hash)
		s.onAccessLocked(n)
	}
	atomic.StoreUint32(&s.bufHead, tail)
}

// onAccessLocked applies one access to the policy: window and protected hits
// refresh recency, and a probation hit earns promotion, demoting the coldest
// protected entry if the segment is full. Caller must hold the write lock.
func (s *tinyShard[K, V]) onAccessLocked(n *tinyNode[K, V]) {
	switch n.segment {
	case tinyWindow:
		s.window.moveToFront(n)
	case tinyProtected:
		s.protected.moveToFront(n)
	case tinyProbation:
		s.probation.remove(n)
		n.segment = tinyProtected
		s.protected.pushFront(n)
		if s.protected.len > s.protectedCap {
			demoted := s.protected.tail
			s.protected.remove(demoted)
			demoted.segment = tinyProbation
			// reset so the demoted entry's next hit records immediately and
			// can re-promote it
			atomic.StoreUint32(&demoted.hits, 0)
			s.probation.pushFront(demoted)
		}
	}
}

func (s *tinyShard[K, V]) listFor(segment int8) *tinyList[K, V] {
	switch segment {
	case tinyWindow:
		return &s.window
	case tinyProbation:
		return &s.probation
	default:
		return &s.protected
	}
}

// Peek retrieves a value from the cache by key without recording the access,
// so the entry gains no frequency or recency credit from it. Returns the value
// and whether the key was found.
func (c *TinyLFU[K, V]) Peek(key K) (V, bool) {
	var zero V
	if !c.hasher.skipKeyCheck && validateKey(key) != nil {
		return zero, false
	}

	s, _ := c.shardFor(key)
	s.mu.RLock()
	defer s.mu.RUnlock()

	n, ok := s.items[key]
	if !ok {
		return zero, false
	}
	return n.val, true
}

// Contains reports whether a key is present, without recording an access.
func (c *TinyLFU[K, V]) Contains(key K) bool {
	if !c.hasher.skipKeyCheck && validateKey(key) != nil {
		return false
	}

	s, _ := c.shardFor(key)
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, ok := s.items[key]
	return ok
}

// Set adds or updates an item in the cache. Updating an existing key counts as
// an access and does not invoke the eviction callback for the previous value.
//
// A new key always enters the shard's admission window. If the window is full,
// its coldest entry becomes a candidate for the main area: it is admitted if
// the frequency sketch ranks it above the entry it would displace, and
// otherwise the candidate itself is evicted. Either way at most one entry
// leaves the cache, and the eviction callback receives it.
//
// A consequence is that on a full cache a just-written cold key can be evicted
// within the next few writes, before it is ever read. That is the admission
// policy protecting the working set, not a bug; code that stores a value and
// relies on reading that same key back immediately should use a cache type
// without an admission policy, such as [Clock] or [Cache].
// Set panics with [ErrInvalidKey] if key cannot be represented safely.
func (c *TinyLFU[K, V]) Set(key K, value V) {
	if !c.hasher.skipKeyCheck {
		if err := validateKey(key); err != nil {
			panic(err)
		}
	}

	s, h := c.shardFor(key)
	s.mu.Lock()
	s.drainLocked()
	onEvict := s.onEvict
	evictedKey, evictedVal, evicted := s.setLocked(key, value, h)
	s.mu.Unlock()

	if evicted && onEvict != nil {
		onEvict(evictedKey, evictedVal)
	}
}

// setLocked inserts or updates key and returns the evicted entry, if any.
// Caller must hold the write lock, with the read buffer already drained.
func (s *tinyShard[K, V]) setLocked(key K, value V, hash uint64) (K, V, bool) {
	var zeroK K
	var zeroV V

	if n, ok := s.items[key]; ok {
		n.val = value
		if atomic.AddUint32(&n.hits, 1)&tinySampleMask == 1 {
			s.sketch.increment(n.hash)
		}
		s.onAccessLocked(n)
		return zeroK, zeroV, false
	}

	// sampled miss recording keeps candidate frequencies on the same scale as
	// sampled hit recording
	s.tick++
	if s.tick&tinySampleMask == 0 {
		s.sketch.increment(hash)
	}

	n := &tinyNode[K, V]{key: key, val: value, hash: hash, segment: tinyWindow}
	s.items[key] = n
	s.window.pushFront(n)
	if s.window.len <= s.windowCap {
		return zeroK, zeroV, false
	}
	return s.overflowWindowLocked()
}

// overflowWindowLocked moves the window's coldest entry toward the main area,
// applying the admission test when main is full. It returns the entry evicted
// from the cache, if any. Caller must hold the write lock.
func (s *tinyShard[K, V]) overflowWindowLocked() (K, V, bool) {
	var zeroK K
	var zeroV V

	candidate := s.window.tail
	s.window.remove(candidate)

	if s.probation.len+s.protected.len < s.mainCap {
		s.moveToProbationLocked(candidate)
		return zeroK, zeroV, false
	}

	victim := s.probation.tail
	if victim == nil {
		victim = s.protected.tail
	}
	if victim == nil {
		// mainCap is zero: the window is the whole shard, so the candidate
		// simply leaves.
		return s.dropLocked(candidate)
	}

	// The admission test: a candidate that is not estimated to be hotter than
	// the victim is dropped outright, which is what stops one-shot keys from
	// displacing the working set.
	if s.sketch.frequency(candidate.hash) > s.sketch.frequency(victim.hash) {
		s.listFor(victim.segment).remove(victim)
		evictedKey, evictedVal, _ := s.dropLocked(victim)
		s.moveToProbationLocked(candidate)
		return evictedKey, evictedVal, true
	}
	return s.dropLocked(candidate)
}

// moveToProbationLocked places a detached node into probation, resetting its
// hit counter so its first re-reference records immediately and promotes it.
func (s *tinyShard[K, V]) moveToProbationLocked(n *tinyNode[K, V]) {
	n.segment = tinyProbation
	atomic.StoreUint32(&n.hits, 0)
	s.probation.pushFront(n)
}

// dropLocked removes a detached node from the cache. Caller must have already
// removed it from its list.
func (s *tinyShard[K, V]) dropLocked(n *tinyNode[K, V]) (K, V, bool) {
	n.dead = true
	delete(s.items, n.key)
	return n.key, n.val, true
}

// Remove deletes an item from the cache by key.
// It returns whether the key was found and removed.
func (c *TinyLFU[K, V]) Remove(key K) bool {
	if !c.hasher.skipKeyCheck && validateKey(key) != nil {
		return false
	}

	s, _ := c.shardFor(key)
	s.mu.Lock()
	s.drainLocked()
	n, ok := s.items[key]
	if !ok {
		s.mu.Unlock()
		return false
	}

	s.listFor(n.segment).remove(n)
	evictedKey, evictedVal, _ := s.dropLocked(n)
	onEvict := s.onEvict
	s.mu.Unlock()

	if onEvict != nil {
		onEvict(evictedKey, evictedVal)
	}
	return true
}

// GetOrSet retrieves a value from the cache by key, or computes and sets it if
// not present. The compute function is only called if the key is not present.
// Note: if multiple goroutines call GetOrSet concurrently for the same missing
// key, compute may be called multiple times but only one result will be cached.
// If another goroutine inserts the key while compute runs, that stored value is
// returned and the result of compute is discarded. compute must be safe to
// abandon (no unreclaimed side effects), or use [TinyLFU.GetOrSetSingleflight].
func (c *TinyLFU[K, V]) GetOrSet(key K, compute func() (V, error)) (V, error) {
	if !c.hasher.skipKeyCheck {
		if err := validateKey(key); err != nil {
			var zero V
			return zero, err
		}
	}

	s, h := c.shardFor(key)
	if val, ok := s.get(key); ok {
		return val, nil
	}
	return s.computeAndSet(key, h, compute)
}

// GetOrSetSingleflight retrieves a value from the cache by key, or computes and
// sets it if not present. Unlike [TinyLFU.GetOrSet], concurrent callers for the
// same missing key call compute exactly once and all receive the same result.
//
// The deduplication only applies to concurrent in-flight calls; once a value is
// cached, subsequent calls return it without invoking singleflight. compute
// must not call GetOrSetSingleflight recursively for the same key because it
// would wait on its own call.
func (c *TinyLFU[K, V]) GetOrSetSingleflight(key K, compute func() (V, error)) (V, error) {
	if !c.hasher.skipKeyCheck {
		if err := validateKey(key); err != nil {
			var zero V
			return zero, err
		}
	}

	s, h := c.shardFor(key)
	if val, ok := s.get(key); ok {
		return val, nil
	}

	result, err := s.sfGroup.Do(key, func() (V, error) {
		if val, ok := s.get(key); ok {
			return val, nil
		}
		return s.computeAndSet(key, h, compute)
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return result, nil
}

// computeAndSet runs compute outside the shard lock, then stores the result
// unless another goroutine cached the key first.
func (s *tinyShard[K, V]) computeAndSet(key K, hash uint64, compute func() (V, error)) (V, error) {
	val, err := compute()
	if err != nil {
		var zero V
		return zero, err
	}

	s.mu.Lock()
	s.drainLocked()
	if n, ok := s.items[key]; ok {
		existing := n.val
		if atomic.AddUint32(&n.hits, 1)&tinySampleMask == 1 {
			s.sketch.increment(n.hash)
		}
		s.onAccessLocked(n)
		s.mu.Unlock()
		return existing, nil
	}

	onEvict := s.onEvict
	evictedKey, evictedVal, evicted := s.setLocked(key, val, hash)
	s.mu.Unlock()

	if evicted && onEvict != nil {
		onEvict(evictedKey, evictedVal)
	}
	return val, nil
}

// Len returns the current number of items across all shards.
// The result is collected shard by shard and is not atomic with respect to
// concurrent updates across shards.
func (c *TinyLFU[K, V]) Len() int {
	total := 0
	for _, s := range c.shards {
		s.mu.RLock()
		total += len(s.items)
		s.mu.RUnlock()
	}
	return total
}

// Capacity returns the maximum total capacity of the cache.
func (c *TinyLFU[K, V]) Capacity() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.capacity
}

// ShardCount returns the number of shards in the cache.
func (c *TinyLFU[K, V]) ShardCount() int {
	return len(c.shards)
}

// appendNodesLocked appends every node of a shard, window first, then
// probation, then protected. Caller must hold at least a read lock.
func (s *tinyShard[K, V]) appendNodesLocked(nodes []*tinyNode[K, V]) []*tinyNode[K, V] {
	for _, l := range []*tinyList[K, V]{&s.window, &s.probation, &s.protected} {
		for n := l.head; n != nil; n = n.next {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// Keys returns a slice of all keys in the cache.
//
// The order is unspecified: TinyLFU keeps no global recency order, and shards
// are processed independently. The result is a detached copy collected shard by
// shard and is not atomic with respect to concurrent updates.
func (c *TinyLFU[K, V]) Keys() []K {
	keys := make([]K, 0, c.Len())
	var nodes []*tinyNode[K, V]
	for _, s := range c.shards {
		s.mu.RLock()
		nodes = s.appendNodesLocked(nodes[:0])
		for _, n := range nodes {
			keys = append(keys, n.key)
		}
		s.mu.RUnlock()
	}
	return keys
}

// Values returns a slice of all values in the cache.
//
// The order is unspecified, as for [TinyLFU.Keys]. Keys and Values are separate
// copies collected under separate lock holds, so their elements line up only
// when no other goroutine writes to the cache between the two calls.
func (c *TinyLFU[K, V]) Values() []V {
	values := make([]V, 0, c.Len())
	var nodes []*tinyNode[K, V]
	for _, s := range c.shards {
		s.mu.RLock()
		nodes = s.appendNodesLocked(nodes[:0])
		for _, n := range nodes {
			values = append(values, n.val)
		}
		s.mu.RUnlock()
	}
	return values
}

// Clear removes all items from all shards and resets the frequency sketch.
// Shards are cleared one at a time; the operation is not atomic across shards.
//
// If an eviction callback is set, it is called for every stored entry in
// unspecified order. The entries are buffered while the lock is held so the
// callbacks can run without it, so clearing a large cache with a callback set
// allocates one key/value pair per entry.
func (c *TinyLFU[K, V]) Clear() {
	for _, s := range c.shards {
		s.mu.Lock()
		onEvict := s.onEvict

		var evicted []evictedItem[K, V]
		if onEvict != nil {
			evicted = make([]evictedItem[K, V], 0, len(s.items))
			for _, n := range s.appendNodesLocked(nil) {
				evicted = append(evicted, evictedItem[K, V]{key: n.key, val: n.val})
			}
		}

		s.items = make(map[K]*tinyNode[K, V], allocationHint(s.capacity()))
		s.window = tinyList[K, V]{}
		s.probation = tinyList[K, V]{}
		s.protected = tinyList[K, V]{}
		s.sketch.clear()
		for i := range s.buffer {
			s.buffer[i] = nil
		}
		atomic.StoreUint32(&s.bufHead, 0)
		atomic.StoreUint32(&s.bufTail, 0)
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
// Shrinking evicts entries the policy ranks lowest, in unspecified order, and
// reports them to the eviction callback. It returns the number of entries
// evicted. Resize returns [ErrCapacityTooLarge] if the resulting per-shard
// capacity cannot be represented safely by the frequency sketch.
func (c *TinyLFU[K, V]) Resize(capacity int) (int, error) {
	if capacity <= 0 {
		return 0, ErrInvalidCapacity
	}

	shardCount := len(c.shards)
	if capacity < shardCount {
		return 0, wrapCapacityBelowShardCount(shardCount)
	}
	largestShard := capacity / shardCount
	if capacity%shardCount > 0 {
		largestShard++
	}
	if !validFrequencySketchCapacity(largestShard) {
		return 0, ErrCapacityTooLarge
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
		s.drainLocked()
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

// resizeLocked applies a new shard capacity: it rebounds the segments, demotes
// protected overflow, evicts until the shard fits, and rebalances the window.
// It returns entries collected for the eviction callback and the eviction
// count, tracked separately so the count stays right without a callback.
// Caller must hold the write lock.
func (s *tinyShard[K, V]) resizeLocked(shardCap int, collect bool) ([]evictedItem[K, V], int) {
	if shardCap != s.capacity() {
		// The sketch's table and aging threshold are both derived from capacity.
		// Rebuild it rather than carrying collision rates and decay timing tuned
		// for a different-sized cache. Packed counters cannot be rehashed exactly.
		s.sketch = newFrequencySketch(shardCap)
	}
	s.setCapsLocked(shardCap)

	for s.protected.len > s.protectedCap {
		demoted := s.protected.tail
		s.protected.remove(demoted)
		demoted.segment = tinyProbation
		atomic.StoreUint32(&demoted.hits, 0)
		s.probation.pushFront(demoted)
	}

	var evicted []evictedItem[K, V]
	count := 0
	for len(s.items) > shardCap {
		victim := s.probation.tail
		if victim == nil {
			victim = s.protected.tail
		}
		if victim == nil {
			victim = s.window.tail
		}
		s.listFor(victim.segment).remove(victim)
		evictedKey, evictedVal, _ := s.dropLocked(victim)
		if collect {
			evicted = append(evicted, evictedItem[K, V]{key: evictedKey, val: evictedVal})
		}
		count++
	}

	// A shrunken window feeds its overflow through normal admission. The shard
	// already fits, so main has room and no admission test can evict here.
	for s.window.len > s.windowCap {
		s.overflowWindowLocked()
	}

	return evicted, count
}

// OnEvict sets a callback invoked when an entry leaves the cache, receiving its
// key and value. It fires for capacity eviction, including a candidate rejected
// by the admission test, and for [TinyLFU.Remove], [TinyLFU.Clear] and
// shrinking [TinyLFU.Resize]. It does not fire when [TinyLFU.Set] replaces the
// value of an existing key, and it is never invoked by a read.
//
// Calling OnEvict again replaces the callback used by all shards for future
// removals. Passing nil clears it. A removal already in progress may still
// invoke the callback that was current when that shard released its lock.
//
// OnEvict serializes against [TinyLFU.Resize] and concurrent OnEvict calls, and
// updates shards one at a time while holding the cache lock, so a swap under
// load can briefly stall shard operations.
//
// The callback runs synchronously after the relevant shard lock is released and
// before the removing method returns. It may be invoked concurrently from
// multiple shards and goroutines, so it must be safe for concurrent use.
func (c *TinyLFU[K, V]) OnEvict(f OnEvictFunc[K, V]) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, s := range c.shards {
		s.mu.Lock()
		s.onEvict = f
		s.mu.Unlock()
	}
}

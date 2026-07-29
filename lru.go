package lru

import (
	"errors"
	"sync"
)

// OnEvictFunc is a function that is called when an entry is evicted from the cache.
type OnEvictFunc[K comparable, V any] func(key K, value V)

// Cache represents a thread-safe, fixed-size LRU cache.
// A Cache must be created with [New] or [MustNew]; the zero value is not ready for use.
type Cache[K comparable, V any] struct {
	capacity int
	items    map[K]*entry[K, V]
	head     *entry[K, V] // most recently used
	tail     *entry[K, V] // least recently used
	mu       sync.RWMutex
	onEvict  OnEvictFunc[K, V] // callback for evictions
	sfGroup  flightGroup[K, V]
}

// entry is an intrusive doubly-linked list node.
type entry[K comparable, V any] struct {
	key  K
	val  V
	prev *entry[K, V]
	next *entry[K, V]
}

// evictedItem holds a key/value pair captured for eviction callbacks that run
// after the cache lock is released, without retaining list pointers.
type evictedItem[K comparable, V any] struct {
	key K
	val V
}

// New creates a new LRU cache with the given capacity.
// The capacity must be greater than zero.
func New[K comparable, V any](capacity int) (*Cache[K, V], error) {
	if capacity <= 0 {
		return nil, errors.New("capacity must be greater than zero")
	}

	return &Cache[K, V]{
		capacity: capacity,
		items:    make(map[K]*entry[K, V], capacity),
	}, nil
}

// MustNew creates a new LRU cache with the given capacity.
// It panics if the capacity is less than or equal to zero.
func MustNew[K comparable, V any](capacity int) *Cache[K, V] {
	cache, err := New[K, V](capacity)
	if err != nil {
		panic(err)
	}
	return cache
}

// Get retrieves a value from the cache by key.
// It returns the value and a boolean indicating whether the key was found.
// This method also updates the item's position in the LRU list.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()

	var zero V

	e, found := c.items[key]
	if !found {
		c.mu.Unlock()
		return zero, false
	}

	c.moveToFront(e)
	val := e.val
	c.mu.Unlock()

	return val, true
}

// Peek retrieves a value from the cache by key without updating its position
// in the LRU list. This is useful for checking a value without affecting
// eviction order. Returns the value and a boolean indicating whether the key was found.
func (c *Cache[K, V]) Peek(key K) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var zero V

	e, found := c.items[key]
	if !found {
		return zero, false
	}

	return e.val, true
}

// GetOrSet retrieves a value from the cache by key, or computes and sets it if not present.
// The compute function is only called if the key is not present in the cache.
// Note: if multiple goroutines call GetOrSet concurrently for the same missing key,
// compute may be called multiple times but only one result will be cached.
func (c *Cache[K, V]) GetOrSet(key K, compute func() (V, error)) (V, error) {
	// fast path: check if item exists
	if val, found := c.Get(key); found {
		return val, nil
	}

	// compute the value outside the lock to avoid deadlock if compute
	// calls back into the cache
	val, err := compute()
	if err != nil {
		var zero V
		return zero, err
	}

	c.mu.Lock()
	// check again in case it was added while we were computing
	if e, found := c.items[key]; found {
		c.moveToFront(e)
		val := e.val
		c.mu.Unlock()
		return val, nil
	}

	// add to cache
	evictedKey, evictedVal, hasEvicted := c.setLocked(key, val)
	onEvict := c.onEvict
	c.mu.Unlock()

	if hasEvicted && onEvict != nil {
		onEvict(evictedKey, evictedVal)
	}
	return val, nil
}

// GetOrSetSingleflight retrieves a value from the cache by key, or computes and sets it if not present.
// Unlike [Cache.GetOrSet], if multiple goroutines call GetOrSetSingleflight concurrently for the same
// missing key, the compute function is called exactly once and all callers receive the same result.
// This is useful when the compute function is expensive (e.g., database queries, API calls).
//
// The singleflight deduplication only applies to concurrent in-flight calls; once a value is cached,
// subsequent calls return the cached value without invoking singleflight.
func (c *Cache[K, V]) GetOrSetSingleflight(key K, compute func() (V, error)) (V, error) {
	// fast path: check if item exists
	if val, found := c.Get(key); found {
		return val, nil
	}

	// use singleflight to deduplicate concurrent computes for the same typed key
	result, err := c.sfGroup.Do(key, func() (V, error) {
		// check again inside singleflight in case another goroutine just cached it
		if val, found := c.Get(key); found {
			return val, nil
		}

		val, err := compute()
		if err != nil {
			var zero V
			return zero, err
		}

		c.mu.Lock()
		// check again in case it was added while we were computing
		if e, found := c.items[key]; found {
			c.moveToFront(e)
			existingVal := e.val
			c.mu.Unlock()
			return existingVal, nil
		}

		evictedKey, evictedVal, hasEvicted := c.setLocked(key, val)
		onEvict := c.onEvict
		c.mu.Unlock()

		if hasEvicted && onEvict != nil {
			onEvict(evictedKey, evictedVal)
		}
		return val, nil
	})

	if err != nil {
		var zero V
		return zero, err
	}
	return result, nil
}

// Set adds or updates an item in the cache.
// If the key already exists, its value is updated.
// If the cache is at capacity, the least recently used item is evicted.
func (c *Cache[K, V]) Set(key K, value V) {
	var evictedKey K
	var evictedVal V
	var hasEvicted bool

	c.mu.Lock()
	evictedKey, evictedVal, hasEvicted = c.setLocked(key, value)
	onEvict := c.onEvict
	c.mu.Unlock()

	if hasEvicted && onEvict != nil {
		onEvict(evictedKey, evictedVal)
	}
}

// Resize changes the maximum capacity of the cache.
// The new capacity must be greater than zero. Increasing capacity does not evict
// entries. Decreasing capacity evicts least recently used entries until the
// cache length is less than or equal to capacity.
func (c *Cache[K, V]) Resize(capacity int) (int, error) {
	if capacity <= 0 {
		return 0, errors.New("capacity must be greater than zero")
	}

	c.mu.Lock()
	onEvict := c.onEvict
	evicted, evictedCount := c.resizeLocked(capacity, onEvict != nil)
	c.mu.Unlock()

	for _, e := range evicted {
		onEvict(e.key, e.val)
	}

	return evictedCount, nil
}

func (c *Cache[K, V]) resizeLocked(capacity int, collectEvicted bool) ([]evictedItem[K, V], int) {
	var evicted []evictedItem[K, V]
	evictedCount := 0

	for len(c.items) > capacity {
		oldest := c.tail
		if oldest == nil {
			break
		}
		if collectEvicted {
			evicted = append(evicted, evictedItem[K, V]{key: oldest.key, val: oldest.val})
		}
		delete(c.items, oldest.key)
		c.remove(oldest)
		evictedCount++
	}

	c.capacity = capacity
	return evicted, evictedCount
}

// setLocked is an internal method that adds or updates an item in the cache.
// it assumes the mutex is already locked.
// Returns the evicted key/value and whether an eviction occurred.
func (c *Cache[K, V]) setLocked(key K, value V) (K, V, bool) {
	// if key exists, update value and move to front
	if e, found := c.items[key]; found {
		c.moveToFront(e)
		e.val = value
		var zeroK K
		var zeroV V
		return zeroK, zeroV, false
	}

	var evictedKey K
	var evictedVal V
	var evicted bool

	// if we're at capacity, remove the least recently used item
	if len(c.items) >= c.capacity {
		oldest := c.tail
		if oldest != nil {
			evictedKey = oldest.key
			evictedVal = oldest.val
			evicted = true
			c.remove(oldest)
			delete(c.items, oldest.key)
		}
	}

	// add new item
	e := &entry[K, V]{
		key: key,
		val: value,
	}
	c.pushFront(e)
	c.items[key] = e
	return evictedKey, evictedVal, evicted
}

// moveToFront moves an entry to the front of the list.
func (c *Cache[K, V]) moveToFront(e *entry[K, V]) {
	if c.head == e {
		return
	}
	c.remove(e)
	c.pushFront(e)
}

// pushFront adds an entry to the front of the list.
func (c *Cache[K, V]) pushFront(e *entry[K, V]) {
	e.prev = nil
	e.next = c.head
	if c.head != nil {
		c.head.prev = e
	}
	c.head = e
	if c.tail == nil {
		c.tail = e
	}
}

// remove removes an entry from the list.
func (c *Cache[K, V]) remove(e *entry[K, V]) {
	if e.prev != nil {
		e.prev.next = e.next
	} else {
		c.head = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	} else {
		c.tail = e.prev
	}
	e.prev = nil
	e.next = nil
}

// Remove deletes an item from the cache by key.
// It returns whether the key was found and removed.
func (c *Cache[K, V]) Remove(key K) bool {
	c.mu.Lock()
	e, found := c.items[key]
	if !found {
		c.mu.Unlock()
		return false
	}

	evictedKey := e.key
	evictedVal := e.val
	onEvict := c.onEvict

	delete(c.items, key)
	c.remove(e)
	c.mu.Unlock()

	if onEvict != nil {
		onEvict(evictedKey, evictedVal)
	}
	return true
}

// GetOldest returns the least recently used entry without updating recency.
func (c *Cache[K, V]) GetOldest() (K, V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var zeroKey K
	var zeroVal V
	if c.tail == nil {
		return zeroKey, zeroVal, false
	}

	return c.tail.key, c.tail.val, true
}

// RemoveOldest removes and returns the least recently used entry.
func (c *Cache[K, V]) RemoveOldest() (K, V, bool) {
	c.mu.Lock()
	oldest := c.tail
	if oldest == nil {
		var zeroKey K
		var zeroVal V
		c.mu.Unlock()
		return zeroKey, zeroVal, false
	}

	key := oldest.key
	val := oldest.val
	onEvict := c.onEvict

	delete(c.items, key)
	c.remove(oldest)
	c.mu.Unlock()

	if onEvict != nil {
		onEvict(key, val)
	}
	return key, val, true
}

// Len returns the current number of items in the cache.
func (c *Cache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.items)
}

// Clear removes all items from the cache.
func (c *Cache[K, V]) Clear() {
	c.mu.Lock()
	onEvict := c.onEvict

	var evicted []evictedItem[K, V]
	if onEvict != nil {
		evicted = make([]evictedItem[K, V], 0, len(c.items))
		for e := c.head; e != nil; e = e.next {
			evicted = append(evicted, evictedItem[K, V]{key: e.key, val: e.val})
		}
	}

	c.items = make(map[K]*entry[K, V], c.capacity)
	c.head = nil
	c.tail = nil
	c.mu.Unlock()

	for _, e := range evicted {
		onEvict(e.key, e.val)
	}
}

// Contains checks if a key exists in the cache.
func (c *Cache[K, V]) Contains(key K) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, found := c.items[key]
	return found
}

// Keys returns a slice of all keys in the cache.
// The order is from most recently used to least recently used.
func (c *Cache[K, V]) Keys() []K {
	c.mu.RLock()
	defer c.mu.RUnlock()

	keys := make([]K, 0, len(c.items))
	for e := c.head; e != nil; e = e.next {
		keys = append(keys, e.key)
	}

	return keys
}

// Values returns a slice of all values in the cache.
// The order matches [Cache.Keys]: most recently used to least recently used.
func (c *Cache[K, V]) Values() []V {
	c.mu.RLock()
	defer c.mu.RUnlock()

	values := make([]V, 0, len(c.items))
	for e := c.head; e != nil; e = e.next {
		values = append(values, e.val)
	}

	return values
}

// Capacity returns the maximum capacity of the cache.
func (c *Cache[K, V]) Capacity() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.capacity
}

// OnEvict sets a callback function that will be called when an entry is evicted from the cache.
// The callback will receive the key and value of the evicted entry.
//
// Calling OnEvict again replaces the callback used for future removals. Passing
// nil clears the callback. If an eviction is already in progress, it may still
// invoke the callback that was current when that eviction released the cache lock.
//
// The callback is invoked after the cache's internal lock is released and may be called
// concurrently from multiple goroutines. It must be safe for concurrent use.
func (c *Cache[K, V]) OnEvict(f OnEvictFunc[K, V]) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onEvict = f
}

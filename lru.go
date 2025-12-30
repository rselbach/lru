package lru

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sync/singleflight"
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
	sfGroup  singleflight.Group
}

// entry is an intrusive doubly-linked list node.
type entry[K comparable, V any] struct {
	key  K
	val  V
	prev *entry[K, V]
	next *entry[K, V]
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

	// use singleflight to deduplicate concurrent computes for the same key
	sfKey := fmt.Sprintf("%v", key)
	result, err, _ := c.sfGroup.Do(sfKey, func() (any, error) {
		// check again inside singleflight in case another goroutine just cached it
		if val, found := c.Get(key); found {
			return val, nil
		}

		val, err := compute()
		if err != nil {
			return nil, err
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
	return result.(V), nil
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

// setLocked is an internal method that adds or updates an item in the cache.
// it assumes the mutex is already locked.
// Returns the evicted key/value and whether an eviction occurred.
func (c *Cache[K, V]) setLocked(key K, value V) (evictedKey K, evictedVal V, evicted bool) {
	// if key exists, update value and move to front
	if e, found := c.items[key]; found {
		c.moveToFront(e)
		e.val = value
		return
	}

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
	return
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

	var evicted []entry[K, V]
	if onEvict != nil {
		evicted = make([]entry[K, V], 0, len(c.items))
		for e := c.head; e != nil; e = e.next {
			evicted = append(evicted, *e)
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

// Capacity returns the maximum capacity of the cache.
func (c *Cache[K, V]) Capacity() int {
	return c.capacity
}

// OnEvict sets a callback function that will be called when an entry is evicted from the cache.
// The callback will receive the key and value of the evicted entry.
//
// The callback is invoked after the cache's internal lock is released and may be called
// concurrently from multiple goroutines. It must be safe for concurrent use.
func (c *Cache[K, V]) OnEvict(f OnEvictFunc[K, V]) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onEvict = f
}

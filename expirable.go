package lru

import (
	"errors"
	"sync"
	"time"
)

// expirableEntry is an intrusive doubly-linked list node with expiry.
type expirableEntry[K comparable, V any] struct {
	key    K
	val    V
	expiry time.Time
	prev   *expirableEntry[K, V]
	next   *expirableEntry[K, V]
}

// Expirable represents a thread-safe, fixed-size LRU cache with expiry functionality.
type Expirable[K comparable, V any] struct {
	capacity int
	items    map[K]*expirableEntry[K, V]
	head     *expirableEntry[K, V] // most recently used
	tail     *expirableEntry[K, V] // least recently used
	mu       sync.RWMutex
	ttl      time.Duration
	timeNow  func() time.Time  // for testing
	onEvict  OnEvictFunc[K, V] // callback for evictions
}

// NewExpirable creates a new LRU cache with the given capacity and TTL.
// Items will be automatically removed from the cache when they expire.
// The capacity must be greater than zero, and the TTL must be greater than zero.
func NewExpirable[K comparable, V any](capacity int, ttl time.Duration) (*Expirable[K, V], error) {
	if capacity <= 0 {
		return nil, errors.New("capacity must be greater than zero")
	}
	if ttl <= 0 {
		return nil, errors.New("TTL must be greater than zero")
	}

	return &Expirable[K, V]{
		capacity: capacity,
		items:    make(map[K]*expirableEntry[K, V], capacity),
		ttl:      ttl,
		timeNow:  time.Now,
	}, nil
}

// MustNewExpirable creates a new LRU cache with the given capacity and TTL.
// It panics if the capacity or TTL is less than or equal to zero.
func MustNewExpirable[K comparable, V any](capacity int, ttl time.Duration) *Expirable[K, V] {
	cache, err := NewExpirable[K, V](capacity, ttl)
	if err != nil {
		panic(err)
	}
	return cache
}

// Get retrieves a value from the cache by key.
// It returns the value and a boolean indicating whether the key was found and not expired.
// This method also updates the item's position in the LRU list.
// Expired items are removed when accessed.
func (c *Expirable[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()

	var zero V

	e, found := c.items[key]
	if !found {
		c.mu.Unlock()
		return zero, false
	}

	// check if the entry has expired
	if c.timeNow().After(e.expiry) {
		evictedKey := e.key
		evictedVal := e.val
		onEvict := c.onEvict
		delete(c.items, e.key)
		c.removeEntry(e)
		c.mu.Unlock()

		if onEvict != nil {
			onEvict(evictedKey, evictedVal)
		}
		return zero, false
	}

	c.moveToFront(e)
	val := e.val
	c.mu.Unlock()

	return val, true
}

// Peek retrieves a value from the cache by key without updating its position
// in the LRU list. This is useful for checking a value without affecting
// eviction order. Returns the value and a boolean indicating whether the key
// was found and not expired. Unlike Get, expired items are not removed.
func (c *Expirable[K, V]) Peek(key K) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var zero V

	e, found := c.items[key]
	if !found {
		return zero, false
	}

	if c.timeNow().After(e.expiry) {
		return zero, false
	}

	return e.val, true
}

// GetWithTTL retrieves a value and its remaining TTL from the cache by key.
// It returns the value, remaining TTL, and a boolean indicating whether the key was found and not expired.
// Expired items are removed when accessed.
func (c *Expirable[K, V]) GetWithTTL(key K) (V, time.Duration, bool) {
	c.mu.Lock()

	var zero V

	e, found := c.items[key]
	if !found {
		c.mu.Unlock()
		return zero, 0, false
	}

	now := c.timeNow()
	// check if the entry has expired
	if now.After(e.expiry) {
		evictedKey := e.key
		evictedVal := e.val
		onEvict := c.onEvict
		delete(c.items, e.key)
		c.removeEntry(e)
		c.mu.Unlock()

		if onEvict != nil {
			onEvict(evictedKey, evictedVal)
		}
		return zero, 0, false
	}

	c.moveToFront(e)

	// calculate remaining TTL
	ttl := e.expiry.Sub(now)
	if ttl < 0 {
		ttl = 0
	}
	val := e.val
	c.mu.Unlock()

	return val, ttl, true
}

// GetOrSet retrieves a value from the cache by key, or computes and sets it if not present or expired.
// The compute function is only called if the key is not present in the cache or is expired.
// Note: if multiple goroutines call GetOrSet concurrently for the same missing/expired key,
// compute may be called multiple times but only one result will be cached.
func (c *Expirable[K, V]) GetOrSet(key K, compute func() (V, error)) (V, error) {
	// first try to get the item without a write lock
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
	e, found := c.items[key]
	var expiredEntry *expirableEntry[K, V]
	if found {
		if !c.timeNow().After(e.expiry) {
			c.moveToFront(e)
			val := e.val
			c.mu.Unlock()
			return val, nil
		}
		// expired entry, remove it and save for callback
		expiredEntry = e
		delete(c.items, key)
		c.removeEntry(e)
	}

	// add to cache
	evictedKey, evictedVal, hasEvicted := c.setLocked(key, val)
	onEvict := c.onEvict
	c.mu.Unlock()

	if onEvict != nil {
		if expiredEntry != nil {
			onEvict(expiredEntry.key, expiredEntry.val)
		}
		if hasEvicted {
			onEvict(evictedKey, evictedVal)
		}
	}
	return val, nil
}

// Set adds or updates an item in the cache.
// If the key already exists, its value is updated.
// If the cache is at capacity, the least recently used item is evicted.
// Expired items are removed lazily on access or via RemoveExpired.
func (c *Expirable[K, V]) Set(key K, value V) {
	c.mu.Lock()
	evictedKey, evictedVal, hasEvicted := c.setLocked(key, value)
	onEvict := c.onEvict
	c.mu.Unlock()

	if hasEvicted && onEvict != nil {
		onEvict(evictedKey, evictedVal)
	}
}

// setLocked is an internal method that adds or updates an item in the cache.
// it assumes the mutex is already locked.
// Returns the evicted key/value and whether an eviction occurred.
func (c *Expirable[K, V]) setLocked(key K, value V) (evictedKey K, evictedVal V, evicted bool) {
	// if key exists, update value and expiry and move to front
	if e, found := c.items[key]; found {
		c.moveToFront(e)
		e.val = value
		e.expiry = c.timeNow().Add(c.ttl)
		return
	}

	// if we're at capacity, remove the least recently used item
	if len(c.items) >= c.capacity {
		oldest := c.tail
		if oldest != nil {
			evictedKey = oldest.key
			evictedVal = oldest.val
			evicted = true
			delete(c.items, oldest.key)
			c.removeEntry(oldest)
		}
	}

	// add new item
	e := &expirableEntry[K, V]{
		key:    key,
		val:    value,
		expiry: c.timeNow().Add(c.ttl),
	}
	c.pushFront(e)
	c.items[key] = e
	return
}

// moveToFront moves an entry to the front of the list.
func (c *Expirable[K, V]) moveToFront(e *expirableEntry[K, V]) {
	if c.head == e {
		return
	}
	c.removeEntry(e)
	c.pushFront(e)
}

// pushFront adds an entry to the front of the list.
func (c *Expirable[K, V]) pushFront(e *expirableEntry[K, V]) {
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

// removeEntry removes an entry from the list.
func (c *Expirable[K, V]) removeEntry(e *expirableEntry[K, V]) {
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
func (c *Expirable[K, V]) Remove(key K) bool {
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
	c.removeEntry(e)
	c.mu.Unlock()

	if onEvict != nil {
		onEvict(evictedKey, evictedVal)
	}
	return true
}

// Len returns the current number of non-expired items in the cache.
func (c *Expirable[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	count := 0
	now := c.timeNow()

	for _, e := range c.items {
		if !now.After(e.expiry) {
			count++
		}
	}

	return count
}

// Clear removes all items from the cache.
func (c *Expirable[K, V]) Clear() {
	c.mu.Lock()
	onEvict := c.onEvict

	var evicted []expirableEntry[K, V]
	if onEvict != nil {
		now := c.timeNow()
		evicted = make([]expirableEntry[K, V], 0, len(c.items))
		for e := c.head; e != nil; e = e.next {
			if !now.After(e.expiry) {
				evicted = append(evicted, *e)
			}
		}
	}

	c.items = make(map[K]*expirableEntry[K, V], c.capacity)
	c.head = nil
	c.tail = nil
	c.mu.Unlock()

	for _, e := range evicted {
		onEvict(e.key, e.val)
	}
}

// Contains checks if a key exists in the cache and is not expired.
func (c *Expirable[K, V]) Contains(key K) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, found := c.items[key]
	if !found {
		return false
	}

	return !c.timeNow().After(e.expiry)
}

// Keys returns a slice of all keys in the cache that haven't expired.
// The order is from most recently used to least recently used.
func (c *Expirable[K, V]) Keys() []K {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := c.timeNow()
	keys := make([]K, 0, len(c.items))

	for e := c.head; e != nil; e = e.next {
		if !now.After(e.expiry) {
			keys = append(keys, e.key)
		}
	}

	return keys
}

// Capacity returns the maximum capacity of the cache.
func (c *Expirable[K, V]) Capacity() int {
	return c.capacity
}

// TTL returns the time-to-live duration for cache entries.
func (c *Expirable[K, V]) TTL() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ttl
}

// SetTTL updates the TTL for future cache entries.
// It does not affect existing entries.
func (c *Expirable[K, V]) SetTTL(ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("TTL must be greater than zero")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.ttl = ttl
	return nil
}

// OnEvict sets a callback function that will be called when an entry is evicted from the cache.
// The callback will receive the key and value of the evicted entry.
// This includes both manual removals and automatic evictions due to capacity or expiry.
func (c *Expirable[K, V]) OnEvict(f OnEvictFunc[K, V]) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onEvict = f
}

// SetTimeNowFunc replaces the function used to get the current time.
// This is primarily useful for testing. Passing nil resets to time.Now.
func (c *Expirable[K, V]) SetTimeNowFunc(f func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if f == nil {
		f = time.Now
	}
	c.timeNow = f
}

// RemoveExpired explicitly removes all expired items from the cache.
// Returns the number of items removed.
// This method will call the eviction callback for each expired item if one is set.
func (c *Expirable[K, V]) RemoveExpired() int {
	c.mu.Lock()

	now := c.timeNow()
	removed := 0

	expiredItems := make([]K, 0)
	expiredValues := make([]V, 0)

	for e := c.head; e != nil; {
		next := e.next
		if now.After(e.expiry) {
			expiredItems = append(expiredItems, e.key)
			expiredValues = append(expiredValues, e.val)
			delete(c.items, e.key)
			c.removeEntry(e)
			removed++
		}
		e = next
	}

	onEvict := c.onEvict
	c.mu.Unlock()

	if onEvict != nil {
		for i := range expiredItems {
			onEvict(expiredItems[i], expiredValues[i])
		}
	}

	return removed
}

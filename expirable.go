// Package lru provides a generic, thread-safe LRU cache implementation.
package lru

import (
	"container/list"
	"errors"
	"sync"
	"time"
)

// expirableEntry extends cacheEntry with an expiry time.
type expirableEntry[K comparable, V any] struct {
	key    K
	val    V
	expiry time.Time
}

// Expirable represents a thread-safe, fixed-size LRU cache with expiry functionality.
type Expirable[K comparable, V any] struct {
	capacity int
	items    map[K]*list.Element
	lruList  *list.List
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
		items:    make(map[K]*list.Element),
		lruList:  list.New(),
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
func (c *Expirable[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var zero V

	element, found := c.items[key]
	if !found {
		return zero, false
	}

	entry := element.Value.(*expirableEntry[K, V])

	// check if the entry has expired
	if c.timeNow().After(entry.expiry) {
		return zero, false
	}

	// move to front of list to mark as recently used
	c.lruList.MoveToFront(element)

	return entry.val, true
}

// GetWithTTL retrieves a value and its remaining TTL from the cache by key.
// It returns the value, remaining TTL, and a boolean indicating whether the key was found and not expired.
func (c *Expirable[K, V]) GetWithTTL(key K) (V, time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var zero V

	element, found := c.items[key]
	if !found {
		return zero, 0, false
	}

	entry := element.Value.(*expirableEntry[K, V])

	now := c.timeNow()
	// check if the entry has expired
	if now.After(entry.expiry) {
		return zero, 0, false
	}

	// move to front of list to mark as recently used
	c.lruList.MoveToFront(element)

	// calculate remaining TTL
	ttl := entry.expiry.Sub(now)
	if ttl < 0 {
		ttl = 0
	}

	return entry.val, ttl, true
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
	defer c.mu.Unlock()

	// check again in case it was added while we were computing
	element, found := c.items[key]
	if found {
		entry := element.Value.(*expirableEntry[K, V])
		if !c.timeNow().After(entry.expiry) {
			c.lruList.MoveToFront(element)
			return entry.val, nil
		}
		// expired entry, remove it
		delete(c.items, key)
		c.lruList.Remove(element)
	}

	// add to cache
	c.setLocked(key, val)
	return val, nil
}

// Set adds or updates an item in the cache.
// If the key already exists, its value is updated.
// If the cache is at capacity, the least recently used item is evicted.
// Any expired items are automatically removed when adding a new item.
func (c *Expirable[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Remove expired items before setting new ones
	c.removeExpiredLocked()
	c.setLocked(key, value)
}

// setLocked is an internal method that adds or updates an item in the cache.
// it assumes the mutex is already locked and expired items have been removed.
func (c *Expirable[K, V]) setLocked(key K, value V) {
	// if key exists, update value and expiry and move to front
	if element, found := c.items[key]; found {
		c.lruList.MoveToFront(element)
		entry := element.Value.(*expirableEntry[K, V])
		entry.val = value
		entry.expiry = c.timeNow().Add(c.ttl)
		return
	}

	// if we're at capacity, remove the least recently used item
	if c.lruList.Len() >= c.capacity {
		oldest := c.lruList.Back()
		if oldest != nil {
			entry := oldest.Value.(*expirableEntry[K, V])
			// call eviction callback if set
			if c.onEvict != nil {
				c.onEvict(entry.key, entry.val)
			}
			delete(c.items, entry.key)
			c.lruList.Remove(oldest)
		}
	}

	// add new item
	entry := &expirableEntry[K, V]{
		key:    key,
		val:    value,
		expiry: c.timeNow().Add(c.ttl),
	}
	element := c.lruList.PushFront(entry)
	c.items[key] = element
}

// Remove deletes an item from the cache by key.
// It returns whether the key was found and removed.
func (c *Expirable[K, V]) Remove(key K) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	// first remove any expired items
	c.removeExpiredLocked()

	element, found := c.items[key]
	if !found {
		return false
	}

	entry := element.Value.(*expirableEntry[K, V])

	// call eviction callback if set
	if c.onEvict != nil {
		c.onEvict(entry.key, entry.val)
	}

	delete(c.items, key)
	c.lruList.Remove(element)
	return true
}

// Len returns the current number of non-expired items in the cache.
func (c *Expirable[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// we'll need to count non-expired entries
	count := 0
	now := c.timeNow()

	for _, element := range c.items {
		entry := element.Value.(*expirableEntry[K, V])
		if !now.After(entry.expiry) {
			count++
		}
	}

	return count
}

// Clear removes all items from the cache.
func (c *Expirable[K, V]) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// call eviction callback for each non-expired item if set
	if c.onEvict != nil {
		now := c.timeNow()
		for _, element := range c.items {
			entry := element.Value.(*expirableEntry[K, V])
			// only call callback for non-expired entries
			if !now.After(entry.expiry) {
				c.onEvict(entry.key, entry.val)
			}
		}
	}

	c.items = make(map[K]*list.Element)
	c.lruList = list.New()
}

// Contains checks if a key exists in the cache and is not expired.
func (c *Expirable[K, V]) Contains(key K) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	element, found := c.items[key]
	if !found {
		return false
	}

	entry := element.Value.(*expirableEntry[K, V])
	return !c.timeNow().After(entry.expiry)
}

// Keys returns a slice of all keys in the cache that haven't expired.
// The order is from most recently used to least recently used.
func (c *Expirable[K, V]) Keys() []K {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := c.timeNow()
	keys := make([]K, 0, len(c.items))

	for element := c.lruList.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*expirableEntry[K, V])
		if !now.After(entry.expiry) {
			keys = append(keys, entry.key)
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

// SetTimeNowFunc allows replacing the function used to get the current time.
// This is primarily used for testing.
func (c *Expirable[K, V]) SetTimeNowFunc(f func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.timeNow = f
}

// OnEvict sets a callback function that will be called when an entry is evicted from the cache.
// The callback will receive the key and value of the evicted entry.
// This includes both manual removals and automatic evictions due to capacity or expiry.
func (c *Expirable[K, V]) OnEvict(f OnEvictFunc[K, V]) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onEvict = f
}

// removeExpiredLocked removes all expired items from the cache.
// it assumes the mutex is already locked.
func (c *Expirable[K, V]) removeExpiredLocked() {
	now := c.timeNow()

	for element := c.lruList.Front(); element != nil; {
		nextElement := element.Next() // save next element before potentially removing current one

		entry := element.Value.(*expirableEntry[K, V])
		if now.After(entry.expiry) {
			// we choose NOT to call callback for expired items in lazy cleanup
			// as they are automatically evicted by time, not by user action
			delete(c.items, entry.key)
			c.lruList.Remove(element)
		}

		element = nextElement
	}
}

// RemoveExpired explicitly removes all expired items from the cache.
// Returns the number of items removed.
// This method will call the eviction callback for each expired item if one is set.
func (c *Expirable[K, V]) RemoveExpired() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.timeNow()
	removed := 0

	// Count items to be removed
	expiredItems := make([]K, 0)
	expiredValues := make([]V, 0)

	for element := c.lruList.Front(); element != nil; {
		nextElement := element.Next() // save next element before potentially removing current one

		entry := element.Value.(*expirableEntry[K, V])
		if now.After(entry.expiry) {
			expiredItems = append(expiredItems, entry.key)
			expiredValues = append(expiredValues, entry.val)
			delete(c.items, entry.key)
			c.lruList.Remove(element)
			removed++
		}

		element = nextElement
	}

	// Call eviction callbacks if set
	if c.onEvict != nil {
		for i := range expiredItems {
			c.onEvict(expiredItems[i], expiredValues[i])
		}
	}

	return removed
}

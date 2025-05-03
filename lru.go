// Package lru provides a generic, thread-safe LRU cache implementation.
package lru

import (
	"container/list"
	"errors"
	"sync"
)

// Cache errors
var (
	ErrKeyNotFound = errors.New("key not found in cache")
	ErrNilValue    = errors.New("nil value not allowed")
)

// Cache represents a thread-safe, fixed-size LRU cache.
type Cache[K comparable, V any] struct {
	capacity int
	items    map[K]*list.Element
	lruList  *list.List
	mu       sync.RWMutex
}

// cacheEntry is an internal representation of a cache entry.
type cacheEntry[K comparable, V any] struct {
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
		items:    make(map[K]*list.Element),
		lruList:  list.New(),
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
	c.mu.RLock()
	defer c.mu.RUnlock()

	var zero V

	element, found := c.items[key]
	if !found {
		return zero, false
	}

	// move to front of list to mark as recently used
	c.lruList.MoveToFront(element)
	entry := element.Value.(*cacheEntry[K, V])

	return entry.val, true
}

// GetOrSet retrieves a value from the cache by key, or computes and sets it if not present.
// The compute function is only called if the key is not present in the cache.
func (c *Cache[K, V]) GetOrSet(key K, compute func() (V, error)) (V, error) {
	// first try to get the item without a write lock
	if val, found := c.Get(key); found {
		return val, nil
	}

	// item not in cache, need to compute it
	c.mu.Lock()
	defer c.mu.Unlock()

	// check again in case it was added between the Get and acquiring the write lock
	if element, found := c.items[key]; found {
		c.lruList.MoveToFront(element)
		entry := element.Value.(*cacheEntry[K, V])
		return entry.val, nil
	}

	// compute the value
	val, err := compute()

	// don't add to cache if computation failed
	if err != nil {
		var zero V
		return zero, err
	}

	// add to cache
	c.setLocked(key, val)
	return val, nil
}

// Set adds or updates an item in the cache.
// If the key already exists, its value is updated.
// If the cache is at capacity, the least recently used item is evicted.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.setLocked(key, value)
}

// setLocked is an internal method that adds or updates an item in the cache.
// it assumes the mutex is already locked.
func (c *Cache[K, V]) setLocked(key K, value V) {
	// if key exists, update value and move to front
	if element, found := c.items[key]; found {
		c.lruList.MoveToFront(element)
		entry := element.Value.(*cacheEntry[K, V])
		entry.val = value
		return
	}

	// if we're at capacity, remove the least recently used item
	if c.lruList.Len() >= c.capacity {
		oldest := c.lruList.Back()
		if oldest != nil {
			entry := oldest.Value.(*cacheEntry[K, V])
			delete(c.items, entry.key)
			c.lruList.Remove(oldest)
		}
	}

	// add new item
	entry := &cacheEntry[K, V]{
		key: key,
		val: value,
	}
	element := c.lruList.PushFront(entry)
	c.items[key] = element
}

// Remove deletes an item from the cache by key.
// It returns whether the key was found and removed.
func (c *Cache[K, V]) Remove(key K) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	element, found := c.items[key]
	if !found {
		return false
	}

	delete(c.items, key)
	c.lruList.Remove(element)
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
	defer c.mu.Unlock()

	c.items = make(map[K]*list.Element)
	c.lruList = list.New()
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
	for element := c.lruList.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*cacheEntry[K, V])
		keys = append(keys, entry.key)
	}

	return keys
}

// Capacity returns the maximum capacity of the cache.
func (c *Cache[K, V]) Capacity() int {
	return c.capacity
}

package lru

import (
	"errors"
	"reflect"
	"sync"
)

// ErrInvalidKey is returned when a key is not dynamically comparable or is
// not equal to itself, such as a floating-point NaN.
var ErrInvalidKey = errors.New("lru: key must be dynamically comparable and equal to itself")

func validateKey[K comparable](key K) error {
	dynamicType := reflect.TypeOf(key)
	if dynamicType != nil && !dynamicType.Comparable() {
		return ErrInvalidKey
	}
	if key != key {
		return ErrInvalidKey
	}
	return nil
}

// OnEvictFunc is a function that is called when an entry is evicted from the cache.
type OnEvictFunc[K comparable, V any] func(key K, value V)

// evictedItem holds a key/value pair captured for eviction callbacks that run
// after the cache lock is released, without retaining list pointers.
type evictedItem[K comparable, V any] struct {
	key K
	val V
}

// entry is an intrusive doubly-linked list node. M carries type-specific
// metadata (empty for Cache, expiry for Expirable).
type entry[K comparable, V any, M any] struct {
	key  K
	val  V
	meta M
	prev *entry[K, V, M]
	next *entry[K, V, M]
}

// base is the shared map + intrusive LRU list used by Cache and Expirable.
type base[K comparable, V any, M any] struct {
	capacity int
	items    map[K]*entry[K, V, M]
	head     *entry[K, V, M] // most recently used
	tail     *entry[K, V, M] // least recently used
	mu       sync.RWMutex
	onEvict  OnEvictFunc[K, V]
	sfGroup  flightGroup[K, V]
}

func newBase[K comparable, V any, M any](capacity int) base[K, V, M] {
	return base[K, V, M]{
		capacity: capacity,
		items:    make(map[K]*entry[K, V, M], capacity),
	}
}

// moveToFront moves an entry to the front of the list.
func (c *base[K, V, M]) moveToFront(e *entry[K, V, M]) {
	if c.head == e {
		return
	}
	c.unlink(e)
	c.pushFront(e)
}

// pushFront adds an entry to the front of the list.
func (c *base[K, V, M]) pushFront(e *entry[K, V, M]) {
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

// unlink removes an entry from the list without touching the map.
func (c *base[K, V, M]) unlink(e *entry[K, V, M]) {
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

// deleteEntry removes an entry from both the map and the list.
func (c *base[K, V, M]) deleteEntry(e *entry[K, V, M]) {
	delete(c.items, e.key)
	c.unlink(e)
}

// resizeLocked shrinks the cache to capacity by evicting from the tail.
// It assumes the mutex is already locked.
func (c *base[K, V, M]) resizeLocked(capacity int, collectEvicted bool) ([]evictedItem[K, V], int) {
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
		c.deleteEntry(oldest)
		evictedCount++
	}

	c.capacity = capacity
	return evicted, evictedCount
}

// resetLocked clears all entries. Caller must hold c.mu.
func (c *base[K, V, M]) resetLocked() {
	c.items = make(map[K]*entry[K, V, M], c.capacity)
	c.head = nil
	c.tail = nil
}

// Capacity returns the maximum capacity of the cache.
func (c *base[K, V, M]) Capacity() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.capacity
}

// OnEvict sets a callback function that will be called when an entry is evicted
// from the cache. The callback will receive the key and value of the evicted entry.
//
// Calling OnEvict again replaces the callback used for future removals. Passing
// nil clears the callback. If an eviction is already in progress, it may still
// invoke the callback that was current when that eviction released the cache lock.
//
// The callback is invoked after the cache's internal lock is released and may be
// called concurrently from multiple goroutines. It must be safe for concurrent use.
func (c *base[K, V, M]) OnEvict(f OnEvictFunc[K, V]) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onEvict = f
}

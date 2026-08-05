package lru

import (
	"errors"
	"math"
	"reflect"
	"sync"
)

var (
	// ErrInvalidKey is returned when a key is not dynamically comparable or is
	// not equal to itself, such as a floating-point NaN.
	ErrInvalidKey = errors.New("lru: key must be dynamically comparable and equal to itself")

	// ErrInvalidCapacity is returned when a cache capacity is zero or negative.
	ErrInvalidCapacity = errors.New("lru: capacity must be greater than zero")

	// ErrCapacityTooLarge is returned when a cache capacity cannot be represented
	// safely by the selected eviction policy's internal data structures.
	ErrCapacityTooLarge = errors.New("lru: capacity is too large")

	// ErrInvalidShardCount is returned when a shard count is zero or negative.
	ErrInvalidShardCount = errors.New("lru: shard count must be greater than zero")

	// ErrShardCountExceedsCapacity is returned when an explicit shard count is
	// greater than the total capacity.
	ErrShardCountExceedsCapacity = errors.New("lru: shard count cannot exceed capacity")

	// ErrCapacityBelowShardCount is returned when Resize would leave a sharded
	// cache with fewer slots than shards.
	ErrCapacityBelowShardCount = errors.New("lru: capacity must be at least shard count")

	// ErrInvalidJanitorInterval is returned when a janitor interval is zero or
	// negative.
	ErrInvalidJanitorInterval = errors.New("lru: janitor interval must be greater than zero")
)

// initialAllocationLimit avoids capacity-sized allocations for sparse caches.
const initialAllocationLimit = 1024

func allocationHint(size int) int {
	if size > initialAllocationLimit {
		return initialAllocationLimit
	}
	return size
}

// keysNeedValidation reports whether keys of type K can ever fail validateKey.
// Only floating-point components, which can be NaN, and interface components,
// which can hold dynamically uncomparable values, can fail. The answer depends
// only on K, so caches resolve it once at construction instead of reflecting on
// every operation.
func keysNeedValidation[K comparable]() bool {
	return typeNeedsValidation(reflect.TypeOf((*K)(nil)).Elem())
}

func typeNeedsValidation(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Float32, reflect.Float64,
		reflect.Complex64, reflect.Complex128,
		reflect.Interface:
		return true
	case reflect.Array:
		return typeNeedsValidation(t.Elem())
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if typeNeedsValidation(t.Field(i).Type) {
				return true
			}
		}
	}
	return false
}

func validateKey[K comparable](key K) (err error) {
	switch value := any(key).(type) {
	case string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr:
		return nil
	case float32:
		if math.IsNaN(float64(value)) {
			return ErrInvalidKey
		}
		return nil
	case float64:
		if math.IsNaN(value) {
			return ErrInvalidKey
		}
		return nil
	case complex64:
		if math.IsNaN(float64(real(value))) || math.IsNaN(float64(imag(value))) {
			return ErrInvalidKey
		}
		return nil
	case complex128:
		if math.IsNaN(real(value)) || math.IsNaN(imag(value)) {
			return ErrInvalidKey
		}
		return nil
	}

	if !dynamicallyComparable(reflect.ValueOf(key)) {
		return ErrInvalidKey
	}

	// Keep comparison failures at the API boundary even for comparable types
	// containing interface values supplied by newer Go callers.
	defer func() {
		if recover() != nil {
			err = ErrInvalidKey
		}
	}()
	if key != key {
		return ErrInvalidKey
	}
	return nil
}

func dynamicallyComparable(value reflect.Value) bool {
	if !value.IsValid() {
		return true
	}
	if !value.Type().Comparable() {
		return false
	}

	switch value.Kind() {
	case reflect.Interface:
		return value.IsNil() || dynamicallyComparable(value.Elem())
	case reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if !dynamicallyComparable(value.Index(i)) {
				return false
			}
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).Name == "_" {
				continue
			}
			if !dynamicallyComparable(value.Field(i)) {
				return false
			}
		}
	}
	return true
}

// OnEvictFunc is a function that is called when an entry is evicted from the cache.
type OnEvictFunc[K comparable, V any] func(key K, value V)

// Item is a key/value pair returned by a cache's Items method.
type Item[K, V any] struct {
	Key   K
	Value V
}

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
	// skipKeyCheck is set when K can never produce an invalid key. The zero
	// value validates, so an uninitialized base stays safe.
	skipKeyCheck bool
}

func newBase[K comparable, V any, M any](capacity int) base[K, V, M] {
	skipKeyCheck := !keysNeedValidation[K]()
	return base[K, V, M]{
		capacity:     capacity,
		items:        make(map[K]*entry[K, V, M], allocationHint(capacity)),
		sfGroup:      flightGroup[K, V]{skipKeyCheck: skipKeyCheck},
		skipKeyCheck: skipKeyCheck,
	}
}

// checkKey validates key unless K is a type whose values are always valid keys.
func (c *base[K, V, M]) checkKey(key K) error {
	if c.skipKeyCheck {
		return nil
	}
	return validateKey(key)
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
	c.items = make(map[K]*entry[K, V, M], allocationHint(c.capacity))
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
// The callback is invoked synchronously after the cache's internal lock is
// released and before the removing method returns. It may be called concurrently
// from multiple goroutines and must be safe for concurrent use.
func (c *base[K, V, M]) OnEvict(f OnEvictFunc[K, V]) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.onEvict = f
}

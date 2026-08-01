package lru

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/maphash"
	"math"
	"reflect"
	"sync"
)

// DefaultShardCount is the default number of shards for a Sharded cache.
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
// A Sharded must be created with [NewSharded], [MustNewSharded], [NewShardedWithCount],
// or [MustNewShardedWithCount]; the zero value is not ready for use.
type Sharded[K comparable, V any] struct {
	shards   []*Cache[K, V]
	seed     maphash.Seed
	mu       sync.RWMutex // protects capacity updates and serializes Resize
	capacity int          // total capacity across all shards
}

// NewSharded creates a new sharded LRU cache with the given total capacity.
// The capacity is distributed evenly across DefaultShardCount shards.
// The capacity must be greater than zero.
func NewSharded[K comparable, V any](capacity int) (*Sharded[K, V], error) {
	return NewShardedWithCount[K, V](capacity, DefaultShardCount)
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
// Both capacity and shardCount must be greater than zero.
//
// Shard selection uses a fast path for built-in strings, integers, floats,
// and bool. Other comparable keys are hashed recursively without invoking
// String or Format methods. Equal keys are always assigned to the same shard.
func NewShardedWithCount[K comparable, V any](capacity, shardCount int) (*Sharded[K, V], error) {
	if capacity <= 0 {
		return nil, errors.New("capacity must be greater than zero")
	}
	if shardCount <= 0 {
		return nil, errors.New("shard count must be greater than zero")
	}

	// clamp shard count to capacity so each shard has at least 1 slot
	if shardCount > capacity {
		shardCount = capacity
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
		seed:     maphash.MakeSeed(),
		capacity: capacity,
	}, nil
}

// MustNewShardedWithCount creates a new sharded LRU cache with the given total capacity
// and number of shards. It panics if the capacity or shard count is less than or equal to zero.
func MustNewShardedWithCount[K comparable, V any](capacity, shardCount int) *Sharded[K, V] {
	cache, err := NewShardedWithCount[K, V](capacity, shardCount)
	if err != nil {
		panic(err)
	}
	return cache
}

// getShard returns the shard for the given key.
func (s *Sharded[K, V]) getShard(key K) *Cache[K, V] {
	idx := s.shardIndex(key)
	return s.shards[idx]
}

// shardIndex returns the shard index for the given key.
func (s *Sharded[K, V]) shardIndex(key K) int {
	if validateKey(key) != nil {
		return 0
	}
	return int(s.hashKey(key) % uint64(len(s.shards)))
}

func (s *Sharded[K, V]) hashKey(key K) uint64 {
	var h maphash.Hash
	h.SetSeed(s.seed)

	// fast path for common built-in types using binary encoding
	var buf [8]byte
	switch k := any(key).(type) {
	case string:
		h.WriteString(k)
	case int:
		writeHashUint64(&h, &buf, uint64(int64(k)))
	case int64:
		writeHashUint64(&h, &buf, uint64(k))
	case int32:
		writeHashUint64(&h, &buf, uint64(int64(k)))
	case int16:
		writeHashUint64(&h, &buf, uint64(int64(k)))
	case int8:
		writeHashUint64(&h, &buf, uint64(int64(k)))
	case uint:
		writeHashUint64(&h, &buf, uint64(k))
	case uint64:
		writeHashUint64(&h, &buf, k)
	case uint32:
		writeHashUint64(&h, &buf, uint64(k))
	case uint16:
		writeHashUint64(&h, &buf, uint64(k))
	case uint8:
		writeHashUint64(&h, &buf, uint64(k))
	case uintptr:
		writeHashUint64(&h, &buf, uint64(k))
	case float64:
		writeHashUint64(&h, &buf, normalizedFloat64Bits(k))
	case float32:
		writeHashUint64(&h, &buf, uint64(normalizedFloat32Bits(k)))
	case complex128:
		writeHashUint64(&h, &buf, normalizedFloat64Bits(real(k)))
		writeHashUint64(&h, &buf, normalizedFloat64Bits(imag(k)))
	case complex64:
		writeHashUint64(&h, &buf, uint64(normalizedFloat32Bits(real(k))))
		writeHashUint64(&h, &buf, uint64(normalizedFloat32Bits(imag(k))))
	case bool:
		if k {
			buf[0] = 1
		}
		h.Write(buf[:1])
	default:
		writeComparableHash(&h, reflect.ValueOf(key), &buf)
	}

	return h.Sum64()
}

func writeHashUint64(h *maphash.Hash, buf *[8]byte, value uint64) {
	binary.LittleEndian.PutUint64(buf[:], value)
	h.Write(buf[:])
}

func normalizedFloat64Bits(value float64) uint64 {
	if value == 0 {
		return 0
	}
	return math.Float64bits(value)
}

func normalizedFloat32Bits(value float32) uint32 {
	if value == 0 {
		return 0
	}
	return math.Float32bits(value)
}

// writeComparableHash hashes values according to Go equality semantics without
// invoking user-defined formatting methods. Collisions between unequal values
// are harmless; equal values must always produce identical bytes.
func writeComparableHash(h *maphash.Hash, value reflect.Value, buf *[8]byte) {
	if !value.IsValid() {
		buf[0] = 0
		h.Write(buf[:1])
		return
	}

	buf[0] = byte(value.Kind()) + 1
	h.Write(buf[:1])

	switch value.Kind() {
	case reflect.Bool:
		buf[0] = 0
		if value.Bool() {
			buf[0] = 1
		}
		h.Write(buf[:1])
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		writeHashUint64(h, buf, uint64(value.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32,
		reflect.Uint64, reflect.Uintptr:
		writeHashUint64(h, buf, value.Uint())
	case reflect.Float32:
		writeHashUint64(h, buf, uint64(normalizedFloat32Bits(float32(value.Float()))))
	case reflect.Float64:
		writeHashUint64(h, buf, normalizedFloat64Bits(value.Float()))
	case reflect.Complex64:
		complexValue := complex64(value.Complex())
		writeHashUint64(h, buf, uint64(normalizedFloat32Bits(real(complexValue))))
		writeHashUint64(h, buf, uint64(normalizedFloat32Bits(imag(complexValue))))
	case reflect.Complex128:
		complexValue := value.Complex()
		writeHashUint64(h, buf, normalizedFloat64Bits(real(complexValue)))
		writeHashUint64(h, buf, normalizedFloat64Bits(imag(complexValue)))
	case reflect.String:
		writeHashUint64(h, buf, uint64(value.Len()))
		h.WriteString(value.String())
	case reflect.Array:
		writeHashUint64(h, buf, uint64(value.Len()))
		for i := 0; i < value.Len(); i++ {
			writeComparableHash(h, value.Index(i), buf)
		}
	case reflect.Struct:
		writeHashUint64(h, buf, uint64(value.NumField()))
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).Name == "_" {
				continue
			}
			writeComparableHash(h, value.Field(i), buf)
		}
	case reflect.Interface:
		if value.IsNil() {
			writeComparableHash(h, reflect.Value{}, buf)
			return
		}
		writeComparableHash(h, value.Elem(), buf)
	case reflect.Chan, reflect.Ptr, reflect.UnsafePointer:
		writeHashUint64(h, buf, uint64(value.Pointer()))
	default:
		panic("lru: unsupported comparable key kind: " + value.Kind().String())
	}
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
// subsequent calls return the cached value without invoking singleflight.
func (s *Sharded[K, V]) GetOrSetSingleflight(key K, compute func() (V, error)) (V, error) {
	return s.getShard(key).GetOrSetSingleflight(key, compute)
}

// Set adds or updates an item in the cache.
// If the key already exists, its value is updated.
// If the shard is at capacity, the least recently used item in that shard is evicted.
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
// The result is a point-in-time snapshot taken per shard and is not atomic with
// respect to concurrent updates across shards.
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
// The result is a point-in-time snapshot and is not atomic with respect to
// concurrent updates.
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
// The result is a point-in-time snapshot and is not atomic with respect to
// concurrent updates.
func (s *Sharded[K, V]) Values() []V {
	values := make([]V, 0, s.Len())
	for _, shard := range s.shards {
		values = shard.appendValues(values)
	}
	return values
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
		return 0, errors.New("capacity must be greater than zero")
	}

	shardCount := len(s.shards)
	if capacity < shardCount {
		return 0, fmt.Errorf("capacity must be at least shard count (%d)", shardCount)
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
// Warning: The callback may be invoked concurrently from multiple shards and
// from multiple goroutines operating on the same shard. It must be safe for
// concurrent use.
func (s *Sharded[K, V]) OnEvict(f OnEvictFunc[K, V]) {
	// Serialize against Resize and concurrent OnEvict so all shards observe
	// the same callback; in-flight evictions may still use a prior callback.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, shard := range s.shards {
		shard.OnEvict(f)
	}
}

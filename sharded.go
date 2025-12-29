package lru

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/maphash"
)

// DefaultShardCount is the default number of shards for a Sharded cache.
const DefaultShardCount = 16

// Sharded represents a thread-safe, sharded LRU cache.
// It distributes keys across multiple Cache instances to reduce lock contention
// under high concurrency. Each shard is an independent LRU cache with its own lock,
// allowing concurrent operations on different shards.
type Sharded[K comparable, V any] struct {
	shards   []*Cache[K, V]
	seed     maphash.Seed
	capacity int // total capacity across all shards
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
func NewShardedWithCount[K comparable, V any](capacity, shardCount int) (*Sharded[K, V], error) {
	if capacity <= 0 {
		return nil, errors.New("capacity must be greater than zero")
	}
	if shardCount <= 0 {
		return nil, errors.New("shard count must be greater than zero")
	}

	// distribute capacity evenly, with remainder going to first shards
	perShard := capacity / shardCount
	remainder := capacity % shardCount
	if perShard < 1 {
		// each shard needs at least capacity 1, so we'll exceed requested capacity
		perShard = 1
		remainder = 0
	}

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
	var h maphash.Hash
	h.SetSeed(s.seed)

	// fast path for common types using binary encoding (avoids fmt.Sprint allocations)
	var buf [8]byte
	switch k := any(key).(type) {
	case string:
		h.WriteString(k)
	case int:
		binary.LittleEndian.PutUint64(buf[:], uint64(int64(k)))
		h.Write(buf[:])
	case int64:
		binary.LittleEndian.PutUint64(buf[:], uint64(k))
		h.Write(buf[:])
	case int32:
		binary.LittleEndian.PutUint64(buf[:], uint64(int64(k)))
		h.Write(buf[:])
	case uint:
		binary.LittleEndian.PutUint64(buf[:], uint64(k))
		h.Write(buf[:])
	case uint64:
		binary.LittleEndian.PutUint64(buf[:], k)
		h.Write(buf[:])
	case uint32:
		binary.LittleEndian.PutUint64(buf[:], uint64(k))
		h.Write(buf[:])
	default:
		// fallback for other comparable types
		h.WriteString(fmt.Sprint(key))
	}

	return int(h.Sum64() % uint64(len(s.shards)))
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
func (s *Sharded[K, V]) GetOrSet(key K, compute func() (V, error)) (V, error) {
	return s.getShard(key).GetOrSet(key, compute)
}

// Set adds or updates an item in the cache.
// If the key already exists, its value is updated.
// If the shard is at capacity, the least recently used item in that shard is evicted.
func (s *Sharded[K, V]) Set(key K, value V) {
	s.getShard(key).Set(key, value)
}

// Remove deletes an item from the cache by key.
// It returns whether the key was found and removed.
func (s *Sharded[K, V]) Remove(key K) bool {
	return s.getShard(key).Remove(key)
}

// Len returns the current number of items in the cache across all shards.
func (s *Sharded[K, V]) Len() int {
	total := 0
	for _, shard := range s.shards {
		total += shard.Len()
	}
	return total
}

// Clear removes all items from all shards.
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
func (s *Sharded[K, V]) Keys() []K {
	keys := make([]K, 0, s.Len())
	for _, shard := range s.shards {
		keys = append(keys, shard.Keys()...)
	}
	return keys
}

// Capacity returns the maximum total capacity of the cache.
func (s *Sharded[K, V]) Capacity() int {
	return s.capacity
}

// ShardCount returns the number of shards in the cache.
func (s *Sharded[K, V]) ShardCount() int {
	return len(s.shards)
}

// OnEvict sets a callback function that will be called when an entry is evicted
// from any shard. The callback will receive the key and value of the evicted entry.
func (s *Sharded[K, V]) OnEvict(f OnEvictFunc[K, V]) {
	for _, shard := range s.shards {
		shard.OnEvict(f)
	}
}

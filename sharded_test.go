package lru

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSharded_New(t *testing.T) {
	tests := map[string]struct {
		capacity int
		wantErr  bool
	}{
		"valid capacity": {
			capacity: 100,
			wantErr:  false,
		},
		"zero capacity": {
			capacity: 0,
			wantErr:  true,
		},
		"negative capacity": {
			capacity: -1,
			wantErr:  true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache, err := NewSharded[string, int](tc.capacity)
			if tc.wantErr {
				r.Error(err)
				r.Nil(cache)
			} else {
				r.NoError(err)
				r.NotNil(cache)
				r.Equal(tc.capacity, cache.Capacity())
				r.Equal(DefaultShardCount, cache.ShardCount())
			}
		})
	}
}

func TestSharded_NewWithCount(t *testing.T) {
	tests := map[string]struct {
		capacity       int
		shardCount     int
		wantErr        bool
		wantShardCount int // want shard count after clamping (0 means use shardCount)
	}{
		"valid capacity and shard count": {
			capacity:   100,
			shardCount: 8,
			wantErr:    false,
		},
		"zero capacity": {
			capacity:   0,
			shardCount: 8,
			wantErr:    true,
		},
		"zero shard count": {
			capacity:   100,
			shardCount: 0,
			wantErr:    true,
		},
		"negative shard count": {
			capacity:   100,
			shardCount: -1,
			wantErr:    true,
		},
		"more shards than capacity": {
			capacity:       4,
			shardCount:     16,
			wantErr:        false,
			wantShardCount: 4, // clamped to capacity
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache, err := NewShardedWithCount[string, int](tc.capacity, tc.shardCount)
			if tc.wantErr {
				r.Error(err)
				r.Nil(cache)
			} else {
				r.NoError(err)
				r.NotNil(cache)
				r.Equal(tc.capacity, cache.Capacity())
				wantShards := tc.shardCount
				if tc.wantShardCount > 0 {
					wantShards = tc.wantShardCount
				}
				r.Equal(wantShards, cache.ShardCount())
			}
		})
	}
}

func TestSharded_MustNew(t *testing.T) {
	tests := map[string]struct {
		capacity     int
		wantPanic    bool
		wantPanicMsg string
	}{
		"valid capacity": {
			capacity:  100,
			wantPanic: false,
		},
		"zero capacity": {
			capacity:     0,
			wantPanic:    true,
			wantPanicMsg: "capacity must be greater than zero",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			if tc.wantPanic {
				r.PanicsWithError(tc.wantPanicMsg, func() {
					MustNewSharded[string, int](tc.capacity)
				})
			} else {
				cache := MustNewSharded[string, int](tc.capacity)
				r.NotNil(cache)
				r.Equal(tc.capacity, cache.Capacity())
			}
		})
	}
}

func TestSharded_MustNewWithCount(t *testing.T) {
	tests := map[string]struct {
		capacity     int
		shardCount   int
		wantPanic    bool
		wantPanicMsg string
	}{
		"valid": {
			capacity:   100,
			shardCount: 8,
			wantPanic:  false,
		},
		"zero shard count": {
			capacity:     100,
			shardCount:   0,
			wantPanic:    true,
			wantPanicMsg: "shard count must be greater than zero",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			if tc.wantPanic {
				r.PanicsWithError(tc.wantPanicMsg, func() {
					MustNewShardedWithCount[string, int](tc.capacity, tc.shardCount)
				})
			} else {
				cache := MustNewShardedWithCount[string, int](tc.capacity, tc.shardCount)
				r.NotNil(cache)
				r.Equal(tc.capacity, cache.Capacity())
				r.Equal(tc.shardCount, cache.ShardCount())
			}
		})
	}
}

func TestSharded_GetSet(t *testing.T) {
	tests := map[string]struct {
		operations []func(c *Sharded[string, int])
		want       map[string]int
	}{
		"basic set and get": {
			operations: []func(c *Sharded[string, int]){
				func(c *Sharded[string, int]) { c.Set("a", 1) },
				func(c *Sharded[string, int]) { c.Set("b", 2) },
				func(c *Sharded[string, int]) { c.Set("c", 3) },
			},
			want: map[string]int{
				"a": 1,
				"b": 2,
				"c": 3,
			},
		},
		"overwrite value": {
			operations: []func(c *Sharded[string, int]){
				func(c *Sharded[string, int]) { c.Set("a", 1) },
				func(c *Sharded[string, int]) { c.Set("a", 5) },
			},
			want: map[string]int{
				"a": 5,
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache := MustNewSharded[string, int](100)
			for _, op := range tc.operations {
				op(cache)
			}

			for k, v := range tc.want {
				got, found := cache.Get(k)
				r.True(found, "key %s should be in cache", k)
				r.Equal(v, got, "value for key %s should be %d", k, v)
			}

			r.Equal(len(tc.want), cache.Len())
		})
	}
}

func TestSharded_Remove(t *testing.T) {
	tests := map[string]struct {
		setup    map[string]int
		toRemove string
		want     bool
	}{
		"remove existing key": {
			setup: map[string]int{
				"a": 1,
				"b": 2,
				"c": 3,
			},
			toRemove: "b",
			want:     true,
		},
		"remove non-existent key": {
			setup: map[string]int{
				"a": 1,
				"b": 2,
				"c": 3,
			},
			toRemove: "z",
			want:     false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache := MustNewSharded[string, int](100)
			for k, v := range tc.setup {
				cache.Set(k, v)
			}

			got := cache.Remove(tc.toRemove)
			r.Equal(tc.want, got)

			_, found := cache.Get(tc.toRemove)
			r.False(found)

			wantLen := len(tc.setup)
			if tc.want {
				wantLen--
			}
			r.Equal(wantLen, cache.Len())
		})
	}
}

func TestSharded_GetOrSet(t *testing.T) {
	tests := map[string]struct {
		setup        map[string]int
		key          string
		computeFunc  func() (int, error)
		want         int
		wantErr      bool
		wantComputed bool
	}{
		"key exists": {
			setup: map[string]int{
				"a": 1,
			},
			key:          "a",
			computeFunc:  func() (int, error) { return 10, nil },
			want:         1,
			wantComputed: false,
		},
		"key doesn't exist, compute succeeds": {
			setup:        map[string]int{},
			key:          "a",
			computeFunc:  func() (int, error) { return 10, nil },
			want:         10,
			wantComputed: true,
		},
		"key doesn't exist, compute fails": {
			setup:        map[string]int{},
			key:          "a",
			computeFunc:  func() (int, error) { return 0, fmt.Errorf("compute error") },
			wantErr:      true,
			wantComputed: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache := MustNewSharded[string, int](100)
			for k, v := range tc.setup {
				cache.Set(k, v)
			}

			computeCalled := false
			wrappedComputeFunc := func() (int, error) {
				computeCalled = true
				return tc.computeFunc()
			}

			got, err := cache.GetOrSet(tc.key, wrappedComputeFunc)

			if tc.wantErr {
				r.Error(err)
			} else {
				r.NoError(err)
				r.Equal(tc.want, got)
			}

			r.Equal(tc.wantComputed, computeCalled)

			if tc.wantComputed && !tc.wantErr {
				v, found := cache.Get(tc.key)
				r.True(found)
				r.Equal(tc.want, v)
			}
		})
	}
}

func TestSharded_Clear(t *testing.T) {
	r := require.New(t)
	cache := MustNewSharded[string, int](100)

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	r.Equal(3, cache.Len())

	cache.Clear()

	r.Equal(0, cache.Len())
	_, found := cache.Get("a")
	r.False(found)
}

func TestSharded_Contains(t *testing.T) {
	tests := map[string]struct {
		setup map[string]int
		key   string
		want  bool
	}{
		"key exists": {
			setup: map[string]int{"a": 1, "b": 2},
			key:   "a",
			want:  true,
		},
		"key doesn't exist": {
			setup: map[string]int{"a": 1, "b": 2},
			key:   "z",
			want:  false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			cache := MustNewSharded[string, int](100)

			for k, v := range tc.setup {
				cache.Set(k, v)
			}

			got := cache.Contains(tc.key)
			r.Equal(tc.want, got)
		})
	}
}

func TestSharded_Keys(t *testing.T) {
	r := require.New(t)
	cache := MustNewSharded[string, int](100)

	r.Empty(cache.Keys())

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	keys := cache.Keys()
	r.Len(keys, 3)
	r.ElementsMatch([]string{"a", "b", "c"}, keys)
}

func TestSharded_Values(t *testing.T) {
	r := require.New(t)
	cache := MustNewSharded[string, int](100)

	r.Empty(cache.Values())

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	values := cache.Values()
	r.Len(values, 3)
	r.ElementsMatch([]int{1, 2, 3}, values)
}

func TestSharded_Peek(t *testing.T) {
	r := require.New(t)
	cache := MustNewSharded[string, int](100)

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	val, found := cache.Peek("a")
	r.True(found)
	r.Equal(1, val)

	_, found = cache.Peek("z")
	r.False(found)
}

func TestSharded_OnEvict(t *testing.T) {
	r := require.New(t)
	// small cache with 1 shard for predictable eviction
	cache := MustNewShardedWithCount[string, int](2, 1)

	var evictedKeys []string
	var mu sync.Mutex
	cache.OnEvict(func(key string, _ int) {
		mu.Lock()
		evictedKeys = append(evictedKeys, key)
		mu.Unlock()
	})

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3) // should evict "a"

	mu.Lock()
	r.Len(evictedKeys, 1)
	r.Equal([]string{"a"}, evictedKeys)
	mu.Unlock()
}

func TestSharded_ConsistentHashing(t *testing.T) {
	r := require.New(t)
	cache := MustNewSharded[string, int](100)

	// a key should always hash to the same shard
	cache.Set("test-key", 42)

	for i := 0; i < 100; i++ {
		val, found := cache.Get("test-key")
		r.True(found)
		r.Equal(42, val)
	}
}

func TestSharded_DifferentKeyTypes(t *testing.T) {
	t.Run("string keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[string, int](100)
		cache.Set("hello", 1)
		val, found := cache.Get("hello")
		r.True(found)
		r.Equal(1, val)
	})

	t.Run("int keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[int, string](100)
		cache.Set(42, "answer")
		val, found := cache.Get(42)
		r.True(found)
		r.Equal("answer", val)
	})

	t.Run("negative int keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[int, string](100)
		cache.Set(-1, "negative one")
		cache.Set(-42, "negative forty-two")
		val, found := cache.Get(-1)
		r.True(found)
		r.Equal("negative one", val)
		val, found = cache.Get(-42)
		r.True(found)
		r.Equal("negative forty-two", val)
	})

	t.Run("int64 keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[int64, string](100)
		cache.Set(int64(42), "answer")
		val, found := cache.Get(int64(42))
		r.True(found)
		r.Equal("answer", val)
	})

	t.Run("int32 keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[int32, string](100)
		cache.Set(int32(-42), "answer")
		val, found := cache.Get(int32(-42))
		r.True(found)
		r.Equal("answer", val)
	})

	t.Run("uint keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[uint, string](100)
		cache.Set(uint(42), "answer")
		val, found := cache.Get(uint(42))
		r.True(found)
		r.Equal("answer", val)
	})

	t.Run("uint32 keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[uint32, string](100)
		cache.Set(uint32(42), "answer")
		val, found := cache.Get(uint32(42))
		r.True(found)
		r.Equal("answer", val)
	})

	t.Run("uint64 keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[uint64, string](100)
		cache.Set(uint64(42), "answer")
		val, found := cache.Get(uint64(42))
		r.True(found)
		r.Equal("answer", val)
	})

	t.Run("int16 keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[int16, string](100)
		cache.Set(int16(-7), "answer")
		val, found := cache.Get(int16(-7))
		r.True(found)
		r.Equal("answer", val)
	})

	t.Run("uint8 keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[uint8, string](100)
		cache.Set(uint8(9), "answer")
		val, found := cache.Get(uint8(9))
		r.True(found)
		r.Equal("answer", val)
	})

	t.Run("float64 keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[float64, string](100)
		cache.Set(3.14, "pi")
		val, found := cache.Get(3.14)
		r.True(found)
		r.Equal("pi", val)
	})

	t.Run("bool keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[bool, string](100)
		cache.Set(true, "yes")
		cache.Set(false, "no")
		val, found := cache.Get(true)
		r.True(found)
		r.Equal("yes", val)
		val, found = cache.Get(false)
		r.True(found)
		r.Equal("no", val)
	})

	type customKey struct {
		a int
		b string
	}

	t.Run("struct keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[customKey, string](100)
		key := customKey{a: 1, b: "test"}
		cache.Set(key, "value")
		val, found := cache.Get(key)
		r.True(found)
		r.Equal("value", val)
	})
}

func TestSharded_ConcurrentAccess(t *testing.T) {
	cache := MustNewSharded[int, int](1000)

	var wg sync.WaitGroup
	numGoroutines := 100
	opsPerGoroutine := 1000

	// concurrent writes
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				cache.Set(base*opsPerGoroutine+j, j)
			}
		}(i)
	}
	wg.Wait()

	// concurrent reads
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				cache.Get(base*opsPerGoroutine + j)
			}
		}(i)
	}
	wg.Wait()

	// mixed reads and writes
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				if j%2 == 0 {
					cache.Set(j%1000, j)
				} else {
					cache.Get(j % 1000)
				}
			}
		}()
	}
	wg.Wait()
}

// Benchmarks comparing Sharded vs regular Cache under contention

func BenchmarkSharded_Parallel_Get(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewSharded[int, int](size)
			for i := 0; i < size; i++ {
				cache.Set(i, i)
			}

			b.ResetTimer()
			b.ReportAllocs()

			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					cache.Get(i % size)
					i++
				}
			})
		})
	}
}

func BenchmarkSharded_Parallel_Set(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewSharded[int, int](size)

			b.ResetTimer()
			b.ReportAllocs()

			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					cache.Set(i%size, i)
					i++
				}
			})
		})
	}
}

func BenchmarkSharded_Parallel_Mixed(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewSharded[int, int](size)
			for i := 0; i < size; i++ {
				cache.Set(i, i)
			}

			b.ResetTimer()
			b.ReportAllocs()

			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					if i%5 == 0 {
						cache.Set(i%size, i)
					} else {
						cache.Get(i % size)
					}
					i++
				}
			})
		})
	}
}

func BenchmarkSharded_Parallel_HighContention(b *testing.B) {
	cache := MustNewSharded[int, int](100)
	for i := 0; i < 100; i++ {
		cache.Set(i, i)
	}

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			hotKey := i % 10
			if i%5 == 0 {
				cache.Set(hotKey, i)
			} else {
				cache.Get(hotKey)
			}
			i++
		}
	})
}

// Direct comparison benchmark
func BenchmarkComparison_HighContention(b *testing.B) {
	b.Run("Cache", func(b *testing.B) {
		cache := MustNew[int, int](100)
		for i := 0; i < 100; i++ {
			cache.Set(i, i)
		}

		b.ResetTimer()
		b.ReportAllocs()

		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				hotKey := i % 10
				if i%5 == 0 {
					cache.Set(hotKey, i)
				} else {
					cache.Get(hotKey)
				}
				i++
			}
		})
	})

	b.Run("Sharded", func(b *testing.B) {
		cache := MustNewSharded[int, int](100)
		for i := 0; i < 100; i++ {
			cache.Set(i, i)
		}

		b.ResetTimer()
		b.ReportAllocs()

		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				hotKey := i % 10
				if i%5 == 0 {
					cache.Set(hotKey, i)
				} else {
					cache.Get(hotKey)
				}
				i++
			}
		})
	})
}

func BenchmarkComparison_ParallelGet(b *testing.B) {
	size := 10000

	b.Run("Cache", func(b *testing.B) {
		cache := MustNew[int, int](size)
		for i := 0; i < size; i++ {
			cache.Set(i, i)
		}

		b.ResetTimer()
		b.ReportAllocs()

		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				cache.Get(i % size)
				i++
			}
		})
	})

	b.Run("Sharded", func(b *testing.B) {
		cache := MustNewSharded[int, int](size)
		for i := 0; i < size; i++ {
			cache.Set(i, i)
		}

		b.ResetTimer()
		b.ReportAllocs()

		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				cache.Get(i % size)
				i++
			}
		})
	})
}

func TestSharded_CapacityDistribution(t *testing.T) {
	tests := map[string]struct {
		capacity   int
		shardCount int
		wantCaps   []int
	}{
		"even distribution": {
			capacity:   100,
			shardCount: 10,
			wantCaps:   []int{10, 10, 10, 10, 10, 10, 10, 10, 10, 10},
		},
		"uneven distribution puts remainder on first shards": {
			capacity:   103,
			shardCount: 10,
			wantCaps:   []int{11, 11, 11, 10, 10, 10, 10, 10, 10, 10},
		},
		"more shards than capacity clamps shard count": {
			capacity:   5,
			shardCount: 10,
			wantCaps:   []int{1, 1, 1, 1, 1},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache, err := NewShardedWithCount[int, int](tc.capacity, tc.shardCount)
			r.NoError(err)

			r.Equal(tc.wantCaps, shardedShardCapacities(cache))
			r.Equal(tc.capacity, cache.Capacity())
		})
	}
}

func TestSharded_Resize(t *testing.T) {
	r := require.New(t)
	cache := MustNewShardedWithCount[int, int](4, 2)

	r.Equal([]int{2, 2}, shardedShardCapacities(cache))

	evicted, err := cache.Resize(6)
	r.NoError(err)
	r.Equal(0, evicted)
	r.Equal(6, cache.Capacity())
	r.Equal(2, cache.ShardCount())
	r.Equal([]int{3, 3}, shardedShardCapacities(cache))

	evicted, err = cache.Resize(5)
	r.NoError(err)
	r.Equal(0, evicted)
	r.Equal(5, cache.Capacity())
	r.Equal(2, cache.ShardCount())
	r.Equal([]int{3, 2}, shardedShardCapacities(cache))

	evicted, err = cache.Resize(1)
	r.Error(err)
	r.Equal(0, evicted)
	r.Equal(5, cache.Capacity())
	r.Equal([]int{3, 2}, shardedShardCapacities(cache))
}

func TestSharded_ResizeGrowPreservesEntries(t *testing.T) {
	r := require.New(t)
	cache := MustNewShardedWithCount[int, int](4, 2)

	// fill each shard to its capacity so growth is exercised on full shards
	var all []int
	for shardIdx := range cache.shards {
		keys := keysForShard(cache, shardIdx, 2)
		for _, key := range keys {
			cache.Set(key, key)
		}
		all = append(all, keys...)
	}

	evicted, err := cache.Resize(8)
	r.NoError(err)
	r.Equal(0, evicted)
	r.Equal(8, cache.Capacity())
	r.Equal([]int{4, 4}, shardedShardCapacities(cache))
	for _, key := range all {
		r.True(cache.Contains(key), "key %d must survive growing the cache", key)
	}
}

func TestSharded_ResizeEvictsPerShard(t *testing.T) {
	r := require.New(t)
	cache := MustNewShardedWithCount[int, int](6, 2)

	// the first key set in each shard is that shard's least recently used
	// entry, so it is the one a shrink must evict
	var wantEvicted []int
	for shardIdx := range cache.shards {
		keys := keysForShard(cache, shardIdx, 3)
		wantEvicted = append(wantEvicted, keys[0])
		for _, key := range keys {
			cache.Set(key, key)
		}
	}

	var evictedKeys []int
	var mu sync.Mutex
	cache.OnEvict(func(key int, _ int) {
		mu.Lock()
		evictedKeys = append(evictedKeys, key)
		mu.Unlock()
		cache.Len()
	})

	evicted, err := cache.Resize(4)
	r.NoError(err)
	r.Equal(2, evicted)
	r.Equal(4, cache.Capacity())
	r.Equal([]int{2, 2}, shardedShardCapacities(cache))

	for _, shard := range cache.shards {
		r.LessOrEqual(shard.Len(), shard.Capacity())
	}

	mu.Lock()
	r.ElementsMatch(wantEvicted, evictedKeys)
	mu.Unlock()
}

func TestSharded_ResizeConcurrentAccess(t *testing.T) {
	r := require.New(t)
	cache := MustNewShardedWithCount[int, int](16, 4)

	var wg sync.WaitGroup
	errs := make(chan error, 20*100)
	for worker := 0; worker < 20; worker++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				key := base*1000 + i
				switch i % 4 {
				case 0:
					cache.Set(key, i)
				case 1:
					cache.Get(key)
				case 2:
					cache.Contains(key)
				default:
					_, err := cache.Resize(8 + i%16)
					if err != nil {
						errs <- err
					}
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		r.NoError(err)
	}

	r.LessOrEqual(cache.Len(), cache.Capacity())
	for _, shard := range cache.shards {
		r.LessOrEqual(shard.Len(), shard.Capacity())
	}
}

func shardedShardCapacities[K comparable, V any](cache *Sharded[K, V]) []int {
	capacities := make([]int, len(cache.shards))
	for i, shard := range cache.shards {
		capacities[i] = shard.Capacity()
	}
	return capacities
}

func keysForShard[V any](cache *Sharded[int, V], shardIdx, count int) []int {
	keys := make([]int, 0, count)
	for key := 0; len(keys) < count; key++ {
		if cache.shardIndex(key) == shardIdx {
			keys = append(keys, key)
		}
	}
	return keys
}

func TestSharded_OnEvictCalledOutsideLock(t *testing.T) {
	r := require.New(t)
	cache := MustNewShardedWithCount[int, int](2, 1)

	var callbackExecuted int32
	cache.OnEvict(func(key int, value int) {
		atomic.StoreInt32(&callbackExecuted, 1)
		// try to access the cache from within callback
		// this would deadlock if callback is called inside the lock
		cache.Contains(key)
		cache.Len()
	})

	cache.Set(1, 1)
	cache.Set(2, 2)
	cache.Set(3, 3) // should evict and call callback

	r.Equal(int32(1), atomic.LoadInt32(&callbackExecuted), "callback should have been executed")
}

func TestSharded_GetOrSetSingleflight(t *testing.T) {
	r := require.New(t)
	cache := MustNewSharded[string, int](100)

	// basic functionality: compute is called when key doesn't exist
	var computeCount int32
	val, err := cache.GetOrSetSingleflight("a", func() (int, error) {
		atomic.AddInt32(&computeCount, 1)
		return 42, nil
	})
	r.NoError(err)
	r.Equal(42, val)
	r.Equal(int32(1), atomic.LoadInt32(&computeCount))

	// second call should use cached value, compute not called
	val, err = cache.GetOrSetSingleflight("a", func() (int, error) {
		atomic.AddInt32(&computeCount, 1)
		return 99, nil
	})
	r.NoError(err)
	r.Equal(42, val)
	r.Equal(int32(1), atomic.LoadInt32(&computeCount))

	// error case
	_, err = cache.GetOrSetSingleflight("error", func() (int, error) {
		return 0, fmt.Errorf("compute error")
	})
	r.Error(err)
	r.False(cache.Contains("error"))
}

func TestSharded_GetOrSetSingleflight_Concurrent(t *testing.T) {
	r := require.New(t)
	cache := MustNewSharded[string, int](100)

	const goroutines = 100
	var computeCount int32
	var wg sync.WaitGroup
	results := make([]int, goroutines)
	errs := make([]error, goroutines)

	// all goroutines try to get the same key concurrently
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			val, err := cache.GetOrSetSingleflight("shared", func() (int, error) {
				atomic.AddInt32(&computeCount, 1)
				return 42, nil
			})
			results[idx] = val
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	// compute should have been called exactly once
	r.Equal(int32(1), atomic.LoadInt32(&computeCount), "compute should be called exactly once")

	// all results should be the same
	for i, result := range results {
		r.NoError(errs[i], "goroutine %d", i)
		r.Equal(42, result, "goroutine %d got wrong result", i)
	}
}

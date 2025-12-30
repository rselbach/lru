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
		capacity    int
		expectError bool
	}{
		"valid capacity": {
			capacity:    100,
			expectError: false,
		},
		"zero capacity": {
			capacity:    0,
			expectError: true,
		},
		"negative capacity": {
			capacity:    -1,
			expectError: true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache, err := NewSharded[string, int](tc.capacity)
			if tc.expectError {
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
		capacity         int
		shardCount       int
		expectError      bool
		wantShardCount   int // expected shard count after clamping (0 means use shardCount)
	}{
		"valid capacity and shard count": {
			capacity:    100,
			shardCount:  8,
			expectError: false,
		},
		"zero capacity": {
			capacity:    0,
			shardCount:  8,
			expectError: true,
		},
		"zero shard count": {
			capacity:    100,
			shardCount:  0,
			expectError: true,
		},
		"negative shard count": {
			capacity:    100,
			shardCount:  -1,
			expectError: true,
		},
		"more shards than capacity": {
			capacity:       4,
			shardCount:     16,
			expectError:    false,
			wantShardCount: 4, // clamped to capacity
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache, err := NewShardedWithCount[string, int](tc.capacity, tc.shardCount)
			if tc.expectError {
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
		expectPanic  bool
		panicMessage string
	}{
		"valid capacity": {
			capacity:    100,
			expectPanic: false,
		},
		"zero capacity": {
			capacity:     0,
			expectPanic:  true,
			panicMessage: "capacity must be greater than zero",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			if tc.expectPanic {
				r.PanicsWithError(tc.panicMessage, func() {
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
		expectPanic  bool
		panicMessage string
	}{
		"valid": {
			capacity:    100,
			shardCount:  8,
			expectPanic: false,
		},
		"zero shard count": {
			capacity:     100,
			shardCount:   0,
			expectPanic:  true,
			panicMessage: "shard count must be greater than zero",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			if tc.expectPanic {
				r.PanicsWithError(tc.panicMessage, func() {
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
				r.True(found, fmt.Sprintf("key %s should be in cache", k))
				r.Equal(v, got, fmt.Sprintf("value for key %s should be %d", k, v))
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

			expectedLen := len(tc.setup)
			if tc.want {
				expectedLen--
			}
			r.Equal(expectedLen, cache.Len())
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

	t.Run("uint64 keys", func(t *testing.T) {
		r := require.New(t)
		cache := MustNewSharded[uint64, string](100)
		cache.Set(uint64(42), "answer")
		val, found := cache.Get(uint64(42))
		r.True(found)
		r.Equal("answer", val)
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
	}{
		"even distribution": {
			capacity:   100,
			shardCount: 10,
		},
		"uneven distribution": {
			capacity:   103,
			shardCount: 10,
		},
		"more shards than capacity": {
			capacity:   5,
			shardCount: 10,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache, err := NewShardedWithCount[int, int](tc.capacity, tc.shardCount)
			r.NoError(err)

			// verify total capacity is preserved
			totalCap := 0
			for _, shard := range cache.shards {
				totalCap += shard.Capacity()
			}
			r.GreaterOrEqual(totalCap, tc.capacity)
		})
	}
}

func TestSharded_OnEvictCalledOutsideLock(t *testing.T) {
	r := require.New(t)
	cache := MustNewShardedWithCount[int, int](2, 1)

	var callbackExecuted atomic.Bool
	cache.OnEvict(func(key int, value int) {
		callbackExecuted.Store(true)
		// try to access the cache from within callback
		// this would deadlock if callback is called inside the lock
		cache.Contains(key)
		cache.Len()
	})

	cache.Set(1, 1)
	cache.Set(2, 2)
	cache.Set(3, 3) // should evict and call callback

	r.True(callbackExecuted.Load(), "callback should have been executed")
}

func TestSharded_GetOrSetSingleflight(t *testing.T) {
	r := require.New(t)
	cache := MustNewSharded[string, int](100)

	// basic functionality: compute is called when key doesn't exist
	var computeCount atomic.Int32
	val, err := cache.GetOrSetSingleflight("a", func() (int, error) {
		computeCount.Add(1)
		return 42, nil
	})
	r.NoError(err)
	r.Equal(42, val)
	r.Equal(int32(1), computeCount.Load())

	// second call should use cached value, compute not called
	val, err = cache.GetOrSetSingleflight("a", func() (int, error) {
		computeCount.Add(1)
		return 99, nil
	})
	r.NoError(err)
	r.Equal(42, val)
	r.Equal(int32(1), computeCount.Load())

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
	var computeCount atomic.Int32
	var wg sync.WaitGroup
	results := make([]int, goroutines)

	// all goroutines try to get the same key concurrently
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			val, err := cache.GetOrSetSingleflight("shared", func() (int, error) {
				computeCount.Add(1)
				return 42, nil
			})
			r.NoError(err)
			results[idx] = val
		}(i)
	}
	wg.Wait()

	// compute should have been called exactly once
	r.Equal(int32(1), computeCount.Load(), "compute should be called exactly once")

	// all results should be the same
	for i, result := range results {
		r.Equal(42, result, "goroutine %d got wrong result", i)
	}
}

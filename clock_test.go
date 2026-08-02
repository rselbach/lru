package lru

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClock_New(t *testing.T) {
	tests := map[string]struct {
		capacity   int
		shardCount int
		wantErr    error
		wantShards int
	}{
		"default shards":         {capacity: 100, shardCount: 0, wantShards: DefaultShardCount},
		"capacity below shards":  {capacity: 4, shardCount: 0, wantShards: 4},
		"zero capacity":          {capacity: 0, shardCount: 0, wantErr: ErrInvalidCapacity},
		"negative capacity":      {capacity: -1, shardCount: 0, wantErr: ErrInvalidCapacity},
		"explicit shards":        {capacity: 100, shardCount: 8, wantShards: 8},
		"zero shards":            {capacity: 100, shardCount: -1, wantErr: ErrInvalidShardCount},
		"shards above capacity":  {capacity: 4, shardCount: 8, wantErr: ErrShardCountExceedsCapacity},
		"shards equal capacity":  {capacity: 8, shardCount: 8, wantShards: 8},
		"single shard requested": {capacity: 100, shardCount: 1, wantShards: 1},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			var cache *Clock[string, int]
			var err error
			if tc.shardCount == 0 {
				cache, err = NewClock[string, int](tc.capacity)
			} else {
				cache, err = NewClockWithCount[string, int](tc.capacity, tc.shardCount)
			}

			if tc.wantErr != nil {
				r.ErrorIs(err, tc.wantErr)
				r.Nil(cache)
				return
			}

			r.NoError(err)
			r.Equal(tc.capacity, cache.Capacity())
			r.Equal(tc.wantShards, cache.ShardCount())
			r.Equal(tc.capacity, totalClockShardCapacity(cache))
		})
	}
}

func totalClockShardCapacity[K comparable, V any](cache *Clock[K, V]) int {
	total := 0
	for _, s := range cache.shards {
		total += s.capacity
	}
	return total
}

func TestClock_MustNew(t *testing.T) {
	r := require.New(t)

	r.NotNil(MustNewClock[string, int](10))
	r.NotNil(MustNewClockWithCount[string, int](10, 2))
	r.PanicsWithError(ErrInvalidCapacity.Error(), func() { MustNewClock[string, int](0) })
	r.Panics(func() { MustNewClockWithCount[string, int](4, 8) })
}

func TestClock_GetSet(t *testing.T) {
	r := require.New(t)
	cache := MustNewClock[string, int](100)

	_, found := cache.Get("missing")
	r.False(found)

	cache.Set("troy", 1)
	cache.Set("abed", 2)

	value, found := cache.Get("troy")
	r.True(found)
	r.Equal(1, value)

	// updating an existing key replaces the value without growing the cache
	cache.Set("troy", 10)
	value, found = cache.Get("troy")
	r.True(found)
	r.Equal(10, value)
	r.Equal(2, cache.Len())

	r.True(cache.Contains("abed"))
	r.False(cache.Contains("britta"))
}

func TestClock_PeekDoesNotProtectFromEviction(t *testing.T) {
	r := require.New(t)
	// One shard so eviction order is deterministic.
	cache := MustNewClockWithCount[int, int](2, 1)

	cache.Set(1, 1)
	cache.Set(2, 2)

	// Both entries were referenced by Set. Peek must not re-mark key 1, so the
	// hand clears its bit and evicts it on the following insert.
	value, found := cache.Peek(1)
	r.True(found)
	r.Equal(1, value)

	cache.Set(3, 3)
	r.False(cache.Contains(1), "Peek must not protect an entry from the hand")
	r.True(cache.Contains(3))
}

func TestClock_GetProtectsFromEviction(t *testing.T) {
	r := require.New(t)
	cache := MustNewClockWithCount[int, int](3, 1)

	cache.Set(1, 1)
	cache.Set(2, 2)
	cache.Set(3, 3)

	// Every entry is referenced, so this insert sweeps the whole ring clearing
	// bits and evicts the first slot. Keys 2 and 3 are left unreferenced.
	cache.Set(4, 4)
	r.False(cache.Contains(1))

	// Referencing 2 must buy it a second chance, so the hand passes over it and
	// takes the still-unreferenced 3 instead.
	_, found := cache.Get(2)
	r.True(found)

	cache.Set(5, 5)
	r.True(cache.Contains(2), "a referenced entry must survive the next sweep")
	r.False(cache.Contains(3), "an unreferenced entry must be evicted first")
}

func TestClock_RespectsCapacity(t *testing.T) {
	r := require.New(t)

	for _, shards := range []int{1, 4, DefaultShardCount} {
		cache := MustNewClockWithCount[int, int](64, shards)
		for i := 0; i < 10000; i++ {
			cache.Set(i, i)
			r.LessOrEqual(cache.Len(), cache.Capacity())
		}

		for _, s := range cache.shards {
			r.LessOrEqual(len(s.items), s.capacity)
			r.LessOrEqual(len(s.ring), s.capacity)
			r.Equal(len(s.items), countClockRing(s))
		}
	}
}

func countClockRing[K comparable, V any](s *clockShard[K, V]) int {
	count := 0
	for _, e := range s.ring {
		if e != nil {
			count++
		}
	}
	return count
}

// Removing entries frees ring slots, which later inserts must reuse instead of
// treating the shard as full.
func TestClock_RemoveFreesSlotsForReuse(t *testing.T) {
	r := require.New(t)
	cache := MustNewClockWithCount[int, int](8, 1)

	for i := 0; i < 8; i++ {
		cache.Set(i, i)
	}
	r.Equal(8, cache.Len())

	for i := 0; i < 4; i++ {
		r.True(cache.Remove(i))
	}
	r.False(cache.Remove(0))
	r.Equal(4, cache.Len())

	for i := 100; i < 104; i++ {
		cache.Set(i, i)
	}
	r.Equal(8, cache.Len())
	for i := 100; i < 104; i++ {
		r.True(cache.Contains(i))
	}
	for i := 4; i < 8; i++ {
		r.True(cache.Contains(i), "removing other keys must not evict %d", i)
	}

	s := cache.shards[0]
	r.Equal(len(s.items), countClockRing(s))
	r.Empty(s.free)
}

func TestClock_KeysAndValues(t *testing.T) {
	r := require.New(t)
	cache := MustNewClock[int, int](100)

	for i := 0; i < 20; i++ {
		cache.Set(i, i*3)
	}

	keys := cache.Keys()
	values := cache.Values()
	r.Len(keys, 20)
	r.Len(values, 20)

	want := make([]int, 0, 20)
	wantValues := make([]int, 0, 20)
	for i := 0; i < 20; i++ {
		want = append(want, i)
		wantValues = append(wantValues, i*3)
	}
	// Order is deliberately unspecified.
	r.ElementsMatch(want, keys)
	r.ElementsMatch(wantValues, values)
}

func TestClock_Clear(t *testing.T) {
	r := require.New(t)
	cache := MustNewClock[int, int](100)

	evicted := make(map[int]int)
	var mu sync.Mutex
	cache.OnEvict(func(key, value int) {
		mu.Lock()
		defer mu.Unlock()
		evicted[key] = value
	})

	for i := 0; i < 20; i++ {
		cache.Set(i, i*3)
	}

	cache.Clear()
	r.Zero(cache.Len())
	r.Empty(cache.Keys())
	r.Len(evicted, 20)

	// The cache must still work after being cleared.
	cache.Set(1, 1)
	value, found := cache.Get(1)
	r.True(found)
	r.Equal(1, value)
}

func TestClock_OnEvict(t *testing.T) {
	r := require.New(t)
	cache := MustNewClockWithCount[int, int](4, 1)

	var mu sync.Mutex
	evicted := make(map[int]int)
	cache.OnEvict(func(key, value int) {
		mu.Lock()
		defer mu.Unlock()
		evicted[key] = value
	})

	for i := 0; i < 4; i++ {
		cache.Set(i, i)
	}
	r.Empty(evicted, "filling to capacity must not evict")

	// Replacing a live value must not fire the callback.
	cache.Set(0, 100)
	r.Empty(evicted)

	cache.Set(99, 99)
	r.Len(evicted, 1)

	// Remove reports the entry it drops.
	key := cache.Keys()[0]
	r.True(cache.Remove(key))
	r.Contains(evicted, key)

	// Clearing the callback stops future reports.
	before := len(evicted)
	cache.OnEvict(nil)
	for i := 200; i < 220; i++ {
		cache.Set(i, i)
	}
	r.Len(evicted, before)
}

func TestClock_Resize(t *testing.T) {
	r := require.New(t)

	t.Run("invalid", func(t *testing.T) {
		cache := MustNewClockWithCount[int, int](16, 4)
		_, err := cache.Resize(0)
		r.ErrorIs(err, ErrInvalidCapacity)
		_, err = cache.Resize(3)
		r.ErrorIs(err, ErrCapacityBelowShardCount)
		r.Equal(16, cache.Capacity())
	})

	t.Run("shrink evicts and reports", func(t *testing.T) {
		cache := MustNewClockWithCount[int, int](64, 4)
		for i := 0; i < 64; i++ {
			cache.Set(i, i)
		}
		// Keys spread unevenly across shards, so a full cache holds at most,
		// but rarely exactly, its capacity.
		before := cache.Len()
		r.LessOrEqual(before, 64)
		r.Greater(before, 16)

		// Register the callback after filling so it counts only resize evictions.
		var evictions int32
		cache.OnEvict(func(int, int) { atomic.AddInt32(&evictions, 1) })

		evicted, err := cache.Resize(16)
		r.NoError(err)
		r.Equal(16, cache.Capacity())
		r.LessOrEqual(cache.Len(), 16)
		r.Equal(before-cache.Len(), evicted)
		r.Equal(int32(evicted), atomic.LoadInt32(&evictions))

		for _, s := range cache.shards {
			r.LessOrEqual(len(s.items), s.capacity)
			r.Equal(len(s.items), countClockRing(s))
			r.Equal(len(s.items), len(s.ring), "ring must be compacted after resize")
			r.Empty(s.free)
		}
	})

	t.Run("shrink counts without a callback", func(t *testing.T) {
		cache := MustNewClockWithCount[int, int](64, 4)
		for i := 0; i < 64; i++ {
			cache.Set(i, i)
		}
		before := cache.Len()

		// No OnEvict registered: the returned count must still be the number
		// of entries evicted, not the number collected for callbacks.
		evicted, err := cache.Resize(16)
		r.NoError(err)
		r.Equal(before-cache.Len(), evicted)
		r.Positive(evicted)
	})

	t.Run("grow keeps entries", func(t *testing.T) {
		cache := MustNewClockWithCount[int, int](16, 4)
		for i := 0; i < 16; i++ {
			cache.Set(i, i)
		}
		before := cache.Len()

		evicted, err := cache.Resize(64)
		r.NoError(err)
		r.Zero(evicted, "growing must not evict")
		r.Equal(64, cache.Capacity())
		r.Equal(before, cache.Len())

		for i := 100; i < 148; i++ {
			cache.Set(i, i)
		}
		r.LessOrEqual(cache.Len(), 64)
		r.Greater(cache.Len(), before, "the grown capacity must be usable")
	})
}

func TestClock_GetOrSet(t *testing.T) {
	r := require.New(t)
	cache := MustNewClock[string, int](100)

	calls := 0
	value, err := cache.GetOrSet("troy", func() (int, error) {
		calls++
		return 42, nil
	})
	r.NoError(err)
	r.Equal(42, value)
	r.Equal(1, calls)

	value, err = cache.GetOrSet("troy", func() (int, error) {
		calls++
		return 99, nil
	})
	r.NoError(err)
	r.Equal(42, value)
	r.Equal(1, calls, "compute must not run on a hit")

	wantErr := errors.New("compute failed")
	_, err = cache.GetOrSet("abed", func() (int, error) { return 0, wantErr })
	r.ErrorIs(err, wantErr)
	r.False(cache.Contains("abed"))
}

func TestClock_GetOrSet_KeyAddedWhileComputing(t *testing.T) {
	r := require.New(t)
	cache := MustNewClock[string, int](100)

	value, err := cache.GetOrSet("troy", func() (int, error) {
		// Another goroutine wins the race and stores a value first.
		cache.Set("troy", 7)
		return 42, nil
	})
	r.NoError(err)
	r.Equal(7, value, "the stored value must win over the discarded compute")

	stored, found := cache.Get("troy")
	r.True(found)
	r.Equal(7, stored)
}

func TestClock_GetOrSet_EvictionFiresCallback(t *testing.T) {
	r := require.New(t)
	cache := MustNewClockWithCount[int, int](2, 1)

	var mu sync.Mutex
	evicted := make(map[int]int)
	cache.OnEvict(func(key, value int) {
		mu.Lock()
		defer mu.Unlock()
		evicted[key] = value
	})

	cache.Set(1, 1)
	cache.Set(2, 2)

	value, err := cache.GetOrSet(3, func() (int, error) { return 3, nil })
	r.NoError(err)
	r.Equal(3, value)
	r.Len(evicted, 1, "filling the last slot through GetOrSet must report the eviction")
	r.LessOrEqual(cache.Len(), 2)
}

func TestClock_GetOrSetSingleflight(t *testing.T) {
	r := require.New(t)
	cache := MustNewClock[string, int](100)

	const goroutines = 16
	var computeCount int32
	computeStarted := make(chan struct{})
	release := make(chan struct{})
	begin := make(chan struct{})

	results := make([]int, goroutines)
	errs := make([]error, goroutines)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-begin
			value, err := cache.GetOrSetSingleflight("shared", func() (int, error) {
				if atomic.AddInt32(&computeCount, 1) == 1 {
					close(computeStarted)
				}
				<-release
				return 42, nil
			})
			results[idx] = value
			errs[idx] = err
		}(i)
	}

	close(begin)
	<-computeStarted
	shard := cache.getShard("shared")
	waitForFlightWaiters(t, &shard.sfGroup, "shared", goroutines-1)
	close(release)
	wg.Wait()

	r.Equal(int32(1), atomic.LoadInt32(&computeCount))
	for i := range results {
		r.NoError(errs[i], "goroutine %d", i)
		r.Equal(42, results[i], "goroutine %d", i)
	}
}

func TestClock_RejectsNonReflexiveKeys(t *testing.T) {
	r := require.New(t)
	cache := MustNewClock[float64, int](16)
	nan := math.NaN()

	r.PanicsWithValue(ErrInvalidKey, func() { cache.Set(nan, 1) })
	r.Zero(cache.Len())

	_, found := cache.Get(nan)
	r.False(found)
	r.False(cache.Contains(nan))
	r.False(cache.Remove(nan))

	_, err := cache.GetOrSet(nan, func() (int, error) { return 1, nil })
	r.ErrorIs(err, ErrInvalidKey)
	_, err = cache.GetOrSetSingleflight(nan, func() (int, error) { return 1, nil })
	r.ErrorIs(err, ErrInvalidKey)
}

func TestClock_ConcurrentAccess(t *testing.T) {
	cache := MustNewClock[int, int](1000)

	var wg sync.WaitGroup
	for worker := 0; worker < 50; worker++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				key := base*1000 + i
				switch i % 8 {
				case 0:
					cache.Set(key, i)
				case 1:
					cache.Get(key)
				case 2:
					cache.Peek(key)
				case 3:
					cache.Contains(key)
				case 4:
					cache.Remove(key)
				case 5:
					cache.Keys()
				case 6:
					_, _ = cache.GetOrSet(key, func() (int, error) { return i, nil })
				default:
					cache.Len()
				}
			}
		}(worker)
	}
	wg.Wait()

	require.LessOrEqual(t, cache.Len(), cache.Capacity())
	for _, s := range cache.shards {
		require.Equal(t, len(s.items), countClockRing(s))
	}
}

// CLOCK approximates LRU rather than reproducing it, so it must stay close to a
// global LRU's hit rate to be worth its weaker ordering guarantees.
func TestClock_HitRateTracksLRU(t *testing.T) {
	const (
		capacity = 1000
		universe = 10000
		ops      = 500_000
		// Absorbs both the approximation and the random hash seed.
		tolerance = 2.5
	)

	r := require.New(t)

	plain := MustNew[int, int](capacity)
	global := zipfHitRate(plain.Get, plain.Set, ops, universe)
	t.Logf("global LRU:        %.2f%% hit rate", global)

	for _, shards := range []int{1, DefaultShardCount, 64} {
		cache := MustNewClockWithCount[int, int](capacity, shards)
		got := zipfHitRate(cache.Get, cache.Set, ops, universe)
		t.Logf("CLOCK shards=%-3d   %.2f%% hit rate (%+.2f)", shards, got, got-global)
		r.InDelta(global, got, tolerance,
			"CLOCK with %d shards fell too far behind a global LRU", shards)
	}
}

func BenchmarkClock_Parallel_Get(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewClock[int, int](size)
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

// BenchmarkComparison_ReadScaling contrasts the three read paths that matter:
// an exclusive-lock LRU read, the same read sharded, and CLOCK's read-locked
// path that never reorders anything.
func BenchmarkComparison_ReadScaling(b *testing.B) {
	const size = 10000

	b.Run("impl=Cache", func(b *testing.B) {
		cache := MustNew[int, int](2 * size)
		for i := 0; i < size; i++ {
			cache.Set(i, i)
		}
		benchReadParallel(b, size, cache.Get)
	})

	b.Run("impl=Sharded", func(b *testing.B) {
		cache := MustNewSharded[int, int](2 * size)
		for i := 0; i < size; i++ {
			cache.Set(i, i)
		}
		benchReadParallel(b, size, cache.Get)
	})

	b.Run("impl=Clock", func(b *testing.B) {
		cache := MustNewClock[int, int](2 * size)
		for i := 0; i < size; i++ {
			cache.Set(i, i)
		}
		benchReadParallel(b, size, cache.Get)
	})

	b.Run("impl=Clock64", func(b *testing.B) {
		cache := MustNewClockWithCount[int, int](2*size, 64)
		for i := 0; i < size; i++ {
			cache.Set(i, i)
		}
		benchReadParallel(b, size, cache.Get)
	})
}

func benchReadParallel(b *testing.B, size int, get func(int) (int, bool)) {
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			get(i % size)
			i++
		}
	})
}

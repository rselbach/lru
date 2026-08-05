package lru

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTinyLFU_New(t *testing.T) {
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
		"negative shards":        {capacity: 100, shardCount: -1, wantErr: ErrInvalidShardCount},
		"shards above capacity":  {capacity: 4, shardCount: 8, wantErr: ErrShardCountExceedsCapacity},
		"shards equal capacity":  {capacity: 8, shardCount: 8, wantShards: 8},
		"single shard requested": {capacity: 100, shardCount: 1, wantShards: 1},
		"capacity too large": {
			capacity:   int(^uint(0)>>1)/10 + 1,
			shardCount: 1,
			wantErr:    ErrCapacityTooLarge,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			var cache *TinyLFU[string, int]
			var err error
			if tc.shardCount == 0 {
				cache, err = NewTinyLFU[string, int](tc.capacity)
			} else {
				cache, err = NewTinyLFUWithCount[string, int](tc.capacity, tc.shardCount)
			}

			if tc.wantErr != nil {
				r.ErrorIs(err, tc.wantErr)
				r.Nil(cache)
				return
			}

			r.NoError(err)
			r.Equal(tc.capacity, cache.Capacity())
			r.Equal(tc.wantShards, cache.ShardCount())

			total := 0
			for _, s := range cache.shards {
				r.GreaterOrEqual(s.windowCap, 1)
				r.Equal(s.windowCap+s.mainCap, s.capacity())
				total += s.capacity()
			}
			r.Equal(tc.capacity, total)
		})
	}
}

func TestTinyLFU_MustNew(t *testing.T) {
	r := require.New(t)

	r.NotNil(MustNewTinyLFU[string, int](10))
	r.NotNil(MustNewTinyLFUWithCount[string, int](10, 2))
	r.PanicsWithError(ErrInvalidCapacity.Error(), func() { MustNewTinyLFU[string, int](0) })
	r.Panics(func() { MustNewTinyLFUWithCount[string, int](4, 8) })
}

func TestTinyLFU_GetSet(t *testing.T) {
	r := require.New(t)
	cache := MustNewTinyLFU[string, int](100)

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

	value, found = cache.Peek("abed")
	r.True(found)
	r.Equal(2, value)
}

// drainTinyShards forces every shard to apply its buffered read records.
func drainTinyShards[K comparable, V any](c *TinyLFU[K, V]) {
	for _, s := range c.shards {
		s.mu.Lock()
		s.drainLocked()
		s.mu.Unlock()
	}
}

// fillTinySingleShard fills a single-shard cache with keys 0..n-1.
func fillTinySingleShard(cache *TinyLFU[int, int], n int) {
	for i := 0; i < n; i++ {
		cache.Set(i, i)
	}
}

func TestTinyLFU_AdmissionRejectsColdCandidates(t *testing.T) {
	r := require.New(t)
	const capacity = 200
	cache := MustNewTinyLFUWithCount[int, int](capacity, 1)
	shard := cache.shards[0]
	r.Equal(2, shard.windowCap)

	fillTinySingleShard(cache, capacity)
	r.Equal(capacity, cache.Len())

	// Give every resident one recorded hit. Sampled miss recording can hand a
	// cold candidate frequency 1, and the admission test only rejects on a tie
	// or worse, so residents need frequency of at least 1 for rejection of
	// every candidate to be deterministic.
	for i := 0; i < capacity; i++ {
		cache.Get(i)
		if i%100 == 0 {
			drainTinyShards(cache)
		}
	}
	drainTinyShards(cache)

	var evicted []int
	cache.OnEvict(func(key, _ int) { evicted = append(evicted, key) })

	// Fresh keys pass through the window; a once-seen candidate cannot rank
	// strictly above any resident, so every candidate is rejected.
	for i := 0; i < 50; i++ {
		cache.Set(1000+i, i)
	}

	r.Equal(capacity, cache.Len(), "rejected candidates must not displace residents")
	for i := 0; i < capacity-2; i++ {
		r.True(cache.Contains(i), "resident key %d must survive the cold stream", i)
	}

	fresh := 0
	for i := 0; i < 50; i++ {
		if cache.Contains(1000 + i) {
			fresh++
		}
	}
	r.Equal(shard.windowCap, fresh, "only the window may hold fresh cold keys")
	r.Len(evicted, 50, "every displaced window entry or rejected candidate is one eviction")
}

func TestTinyLFU_AdmissionAdmitsHotCandidate(t *testing.T) {
	r := require.New(t)
	const capacity = 200
	cache := MustNewTinyLFUWithCount[int, int](capacity, 1)

	fillTinySingleShard(cache, capacity)

	evicted := make(map[int]int)
	cache.OnEvict(func(key, value int) { evicted[key] = value })

	// Make key 1000 measurably hot while it sits in the window.
	cache.Set(1000, 1)
	for i := 0; i < 33; i++ {
		cache.Get(1000)
	}
	drainTinyShards(cache)

	// Two cold inserts push 1000 out of the two-slot window; its recorded
	// frequency must win the admission test against a cold victim.
	cache.Set(2000, 1)
	cache.Set(2001, 1)

	r.True(cache.Contains(1000), "a hot candidate must be admitted to main")
	r.Equal(capacity, cache.Len())
	r.NotEmpty(evicted, "admitting the candidate evicts its victim")
	r.NotContains(evicted, 1000)
}

func TestTinyLFU_ProbationPromotionAndDemotion(t *testing.T) {
	r := require.New(t)
	const capacity = 200
	cache := MustNewTinyLFUWithCount[int, int](capacity, 1)
	shard := cache.shards[0]
	r.Equal(158, shard.protectedCap)

	fillTinySingleShard(cache, capacity)
	r.Zero(shard.protected.len)

	// One recorded hit promotes a probation entry to protected.
	cache.Get(0)
	drainTinyShards(cache)
	r.Equal(int8(tinyProtected), shard.items[0].segment)
	r.Equal(1, shard.protected.len)

	// Filling protected past its bound demotes the coldest entry back.
	for i := 1; i <= shard.protectedCap; i++ {
		cache.Get(i)
		if i%50 == 0 {
			drainTinyShards(cache)
		}
	}
	drainTinyShards(cache)

	r.Equal(shard.protectedCap, shard.protected.len)
	r.Equal(int8(tinyProbation), shard.items[0].segment,
		"the first-promoted entry must be demoted when protected overflows")
}

func TestTinyLFU_WindowOnlyShardEvictsCandidate(t *testing.T) {
	r := require.New(t)
	// capacity 1 with one shard: the window is the whole cache.
	cache := MustNewTinyLFUWithCount[string, int](1, 1)

	evicted := make(map[string]int)
	cache.OnEvict(func(key string, value int) { evicted[key] = value })

	cache.Set("troy", 1)
	cache.Set("abed", 2)

	r.Equal(1, cache.Len())
	r.True(cache.Contains("abed"))
	r.Equal(map[string]int{"troy": 1}, evicted)
}

func TestTinyLFU_Remove(t *testing.T) {
	r := require.New(t)
	cache := MustNewTinyLFUWithCount[int, int](100, 1)

	fillTinySingleShard(cache, 50)

	var evicted []int
	cache.OnEvict(func(key, _ int) { evicted = append(evicted, key) })

	r.True(cache.Remove(10))
	r.False(cache.Remove(10))
	r.False(cache.Contains(10))
	r.Equal(49, cache.Len())
	r.Equal([]int{10}, evicted)

	// A buffered read record for a removed entry must not resurrect it.
	cache.Get(20)
	r.True(cache.Remove(20))
	drainTinyShards(cache)
	r.False(cache.Contains(20))
	r.Equal(48, cache.Len())

	// The cache keeps working after removals.
	cache.Set(10, 100)
	value, found := cache.Get(10)
	r.True(found)
	r.Equal(100, value)
}

func TestTinyLFU_KeysAndValues(t *testing.T) {
	r := require.New(t)
	cache := MustNewTinyLFU[int, int](100)

	want := make([]int, 0, 20)
	wantValues := make([]int, 0, 20)
	for i := 0; i < 20; i++ {
		cache.Set(i, i*3)
		want = append(want, i)
		wantValues = append(wantValues, i*3)
	}

	// Order is deliberately unspecified.
	r.ElementsMatch(want, cache.Keys())
	r.ElementsMatch(wantValues, cache.Values())

	wantItems := make([]Item[int, int], 0, 20)
	for i := 0; i < 20; i++ {
		wantItems = append(wantItems, Item[int, int]{Key: i, Value: i * 3})
	}
	r.ElementsMatch(wantItems, cache.Items())
}

func TestTinyLFU_Clear(t *testing.T) {
	r := require.New(t)
	cache := MustNewTinyLFU[int, int](100)

	var mu sync.Mutex
	evicted := make(map[int]int)
	cache.OnEvict(func(key, value int) {
		mu.Lock()
		defer mu.Unlock()
		evicted[key] = value
	})

	for i := 0; i < 20; i++ {
		cache.Set(i, i*3)
	}
	cache.Get(5)

	cache.Clear()
	r.Zero(cache.Len())
	r.Empty(cache.Keys())
	r.Len(evicted, 20)

	// The cache must still work after being cleared, including its sketch.
	cache.Set(1, 1)
	value, found := cache.Get(1)
	r.True(found)
	r.Equal(1, value)
	for _, s := range cache.shards {
		r.Zero(s.sketch.size)
	}
}

func TestTinyLFU_OnEvict(t *testing.T) {
	r := require.New(t)
	cache := MustNewTinyLFUWithCount[int, int](4, 1)

	var mu sync.Mutex
	evicted := make(map[int]int)
	cache.OnEvict(func(key, value int) {
		mu.Lock()
		defer mu.Unlock()
		evicted[key] = value
	})

	fillTinySingleShard(cache, 4)
	r.Empty(evicted, "filling to capacity must not evict")

	// Replacing a live value must not fire the callback.
	cache.Set(0, 100)
	r.Empty(evicted)

	cache.Set(99, 99)
	r.Len(evicted, 1)

	// Clearing the callback stops future reports.
	before := len(evicted)
	cache.OnEvict(nil)
	for i := 200; i < 220; i++ {
		cache.Set(i, i)
	}
	r.Len(evicted, before)
}

func TestTinyLFU_Resize(t *testing.T) {
	r := require.New(t)

	t.Run("invalid", func(t *testing.T) {
		cache := MustNewTinyLFUWithCount[int, int](16, 4)
		_, err := cache.Resize(0)
		r.ErrorIs(err, ErrInvalidCapacity)
		_, err = cache.Resize(3)
		r.ErrorIs(err, ErrCapacityBelowShardCount)
		_, err = cache.Resize(int(^uint(0)>>1)/10*4 + 1)
		r.ErrorIs(err, ErrCapacityTooLarge)
		r.Equal(16, cache.Capacity())
	})

	t.Run("shrink evicts and reports", func(t *testing.T) {
		cache := MustNewTinyLFUWithCount[int, int](64, 4)
		for i := 0; i < 64; i++ {
			cache.Set(i, i)
		}
		before := cache.Len()
		r.Greater(before, 16)

		var evictions int32
		cache.OnEvict(func(int, int) { atomic.AddInt32(&evictions, 1) })

		evicted, err := cache.Resize(16)
		r.NoError(err)
		r.Equal(16, cache.Capacity())
		r.LessOrEqual(cache.Len(), 16)
		r.Equal(before-cache.Len(), evicted)
		r.Equal(int32(evicted), atomic.LoadInt32(&evictions))
		requireTinyLFUInvariants(t, cache)
	})

	t.Run("shrink counts without a callback", func(t *testing.T) {
		cache := MustNewTinyLFUWithCount[int, int](64, 4)
		for i := 0; i < 64; i++ {
			cache.Set(i, i)
		}
		before := cache.Len()

		evicted, err := cache.Resize(16)
		r.NoError(err)
		r.Equal(before-cache.Len(), evicted)
		r.Positive(evicted)
	})

	t.Run("grow keeps entries", func(t *testing.T) {
		cache := MustNewTinyLFUWithCount[int, int](16, 4)
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
		requireTinyLFUInvariants(t, cache)
	})

	t.Run("rebuilds sketch for new capacity", func(t *testing.T) {
		cache := MustNewTinyLFUWithCount[int, int](16, 1)
		oldSketch := cache.shards[0].sketch
		hash := cache.hasher.hash(1)
		oldSketch.increment(hash)
		r.Equal(1, oldSketch.frequency(hash))

		evicted, err := cache.Resize(64)
		r.NoError(err)
		r.Zero(evicted)
		r.NotSame(oldSketch, cache.shards[0].sketch)
		r.Len(cache.shards[0].sketch.table, 64)
		r.Equal(640, cache.shards[0].sketch.sampleSize)
		r.Zero(cache.shards[0].sketch.frequency(hash), "resize resets stale frequency history")

		grownSketch := cache.shards[0].sketch
		evicted, err = cache.Resize(8)
		r.NoError(err)
		r.Zero(evicted)
		r.NotSame(grownSketch, cache.shards[0].sketch)
		r.Len(cache.shards[0].sketch.table, 8)
		r.Equal(80, cache.shards[0].sketch.sampleSize)
	})
}

func TestTinyLFU_GetOrSet(t *testing.T) {
	r := require.New(t)
	cache := MustNewTinyLFU[string, int](100)

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

func TestTinyLFU_GetOrSet_KeyAddedWhileComputing(t *testing.T) {
	r := require.New(t)
	cache := MustNewTinyLFU[string, int](100)

	value, err := cache.GetOrSet("troy", func() (int, error) {
		// Another goroutine wins the race and stores a value first.
		cache.Set("troy", 7)
		return 42, nil
	})
	r.NoError(err)
	r.Equal(7, value, "the stored value must win over the discarded compute")
}

func TestTinyLFU_GetOrSetSingleflight(t *testing.T) {
	r := require.New(t)
	cache := MustNewTinyLFU[string, int](100)

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
	shard, _ := cache.shardFor("shared")
	waitForFlightWaiters(t, &shard.sfGroup, "shared", goroutines-1)
	close(release)
	wg.Wait()

	r.Equal(int32(1), atomic.LoadInt32(&computeCount))
	for i := range results {
		r.NoError(errs[i], "goroutine %d", i)
		r.Equal(42, results[i], "goroutine %d", i)
	}
}

func TestTinyLFU_RejectsNonReflexiveKeys(t *testing.T) {
	r := require.New(t)
	cache := MustNewTinyLFU[float64, int](16)
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

// requireTinyLFUInvariants drains every shard and checks the structural
// invariants the policy relies on. Violations are collected under the lock and
// asserted after releasing it.
func requireTinyLFUInvariants[K comparable, V any](t *testing.T, c *TinyLFU[K, V]) {
	t.Helper()

	var problems []string
	for i, s := range c.shards {
		s.mu.Lock()
		s.drainLocked()

		segments := []struct {
			name string
			list *tinyList[K, V]
			tag  int8
		}{
			{"window", &s.window, tinyWindow},
			{"probation", &s.probation, tinyProbation},
			{"protected", &s.protected, tinyProtected},
		}

		total := 0
		for _, seg := range segments {
			count := 0
			var prev *tinyNode[K, V]
			for n := seg.list.head; n != nil; n = n.next {
				if count > len(s.items) {
					problems = append(problems, fmt.Sprintf("shard %d %s: cycle", i, seg.name))
					break
				}
				if n.prev != prev {
					problems = append(problems, fmt.Sprintf("shard %d %s: broken prev link", i, seg.name))
				}
				if n.segment != seg.tag {
					problems = append(problems, fmt.Sprintf("shard %d %s: node tagged %d", i, seg.name, n.segment))
				}
				if n.dead {
					problems = append(problems, fmt.Sprintf("shard %d %s: dead node in list", i, seg.name))
				}
				if s.items[n.key] != n {
					problems = append(problems, fmt.Sprintf("shard %d %s: map and list disagree", i, seg.name))
				}
				prev = n
				count++
			}
			if prev != seg.list.tail {
				problems = append(problems, fmt.Sprintf("shard %d %s: tail mismatch", i, seg.name))
			}
			if count != seg.list.len {
				problems = append(problems, fmt.Sprintf("shard %d %s: len %d, counted %d", i, seg.name, seg.list.len, count))
			}
			total += count
		}

		if total != len(s.items) {
			problems = append(problems, fmt.Sprintf("shard %d: lists hold %d, map holds %d", i, total, len(s.items)))
		}
		if s.window.len > s.windowCap {
			problems = append(problems, fmt.Sprintf("shard %d: window %d over cap %d", i, s.window.len, s.windowCap))
		}
		if s.protected.len > s.protectedCap {
			problems = append(problems, fmt.Sprintf("shard %d: protected %d over cap %d", i, s.protected.len, s.protectedCap))
		}
		if s.probation.len+s.protected.len > s.mainCap {
			problems = append(problems, fmt.Sprintf("shard %d: main %d over cap %d", i, s.probation.len+s.protected.len, s.mainCap))
		}
		if len(s.items) > s.capacity() {
			problems = append(problems, fmt.Sprintf("shard %d: %d items over capacity %d", i, len(s.items), s.capacity()))
		}
		s.mu.Unlock()
	}

	require.Empty(t, problems)
}

func TestTinyLFU_ConcurrentAccess(t *testing.T) {
	cache := MustNewTinyLFU[int, int](1000)

	var wg sync.WaitGroup
	for worker := 0; worker < 50; worker++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				key := base*1000 + i
				switch i % 9 {
				case 0:
					cache.Set(key, i)
				case 1:
					cache.Get(key)
				case 2:
					cache.Get(base * 1000) // reread to exercise the buffer
				case 3:
					cache.Peek(key)
				case 4:
					cache.Contains(key)
				case 5:
					cache.Remove(key)
				case 6:
					cache.Keys()
				case 7:
					_, _ = cache.GetOrSet(key, func() (int, error) { return i, nil })
				default:
					cache.Len()
				}
			}
		}(worker)
	}
	wg.Wait()

	require.LessOrEqual(t, cache.Len(), cache.Capacity())
	requireTinyLFUInvariants(t, cache)
}

func TestTinyLFU_ConcurrentHotKey(t *testing.T) {
	// Hammering one key from many goroutines stresses the lossy buffer's CAS
	// path and the sampled per-entry counter.
	cache := MustNewTinyLFU[int, int](100)
	cache.Set(1, 1)

	var wg sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5000; i++ {
				value, found := cache.Get(1)
				if found && value != 1 {
					t.Errorf("got %d, want 1", value)
					return
				}
			}
		}()
	}
	wg.Wait()

	require.True(t, cache.Contains(1))
	requireTinyLFUInvariants(t, cache)
}

// tinyReplay replays a key stream, filling on miss, and returns the hit rate.
func tinyReplay(keys []int, get func(int) (int, bool), set func(int, int)) float64 {
	hits := 0
	for _, key := range keys {
		if _, found := get(key); found {
			hits++
			continue
		}
		set(key, key)
	}
	return 100 * float64(hits) / float64(len(keys))
}

func tinyZipfKeys(ops, universe int, skew float64) []int {
	z := rand.NewZipf(rand.New(rand.NewSource(42)), skew, 1, uint64(universe-1))
	keys := make([]int, ops)
	for i := range keys {
		keys[i] = int(z.Uint64())
	}
	return keys
}

// tinyScanKeys interleaves a reused hot set with a stream of keys never
// requested again, the pattern that flushes an LRU cache.
func tinyScanKeys(ops, hotSet int) []int {
	rng := rand.New(rand.NewSource(7))
	keys := make([]int, ops)
	next := hotSet
	for i := range keys {
		if i%10 < 8 {
			keys[i] = rng.Intn(hotSet)
			continue
		}
		keys[i] = next
		next++
	}
	return keys
}

// tinyLoopKeys cycles through slightly more keys than fit, the pathological
// case for LRU where every entry is evicted just before it is needed again.
func tinyLoopKeys(ops, workingSet int) []int {
	keys := make([]int, ops)
	for i := range keys {
		keys[i] = i % workingSet
	}
	return keys
}

// TestTinyLFU_HitRate holds the policy to the gains that justify it, comparing
// the real implementation, sampled read buffer and all, against exact LRU.
// Floors sit well under measured medians to absorb the randomized hash seed.
func TestTinyLFU_HitRate(t *testing.T) {
	const (
		capacity = 1000
		universe = 10000
		ops      = 200_000
	)

	workloads := []struct {
		name      string
		keys      []int
		floor     float64 // single-shard TinyLFU must reach this
		lruMargin float64 // and beat LRU by at least this
	}{
		{"zipf skew=1.01", tinyZipfKeys(ops, universe, 1.01), 71, 3},
		{"zipf skew=1.2", tinyZipfKeys(ops, universe, 1.2), 87, 1},
		{"scan 80/20", tinyScanKeys(ops, 800), 76, 10},
		{"loop 1.1x cap", tinyLoopKeys(ops, capacity*11/10), 80, 80},
	}

	for _, w := range workloads {
		t.Run(w.name, func(t *testing.T) {
			r := require.New(t)

			lruCache := MustNew[int, int](capacity)
			lru := tinyReplay(w.keys, lruCache.Get, lruCache.Set)

			single := MustNewTinyLFUWithCount[int, int](capacity, 1)
			got := tinyReplay(w.keys, single.Get, single.Set)

			sharded := MustNewTinyLFU[int, int](capacity)
			gotSharded := tinyReplay(w.keys, sharded.Get, sharded.Set)

			t.Logf("LRU %.2f%%  TinyLFU %.2f%% (%+.2f)  TinyLFU/16 shards %.2f%% (%+.2f)",
				lru, got, got-lru, gotSharded, gotSharded-lru)

			r.LessOrEqual(single.Len(), capacity)
			r.GreaterOrEqual(got, w.floor)
			r.GreaterOrEqual(got, lru+w.lruMargin)
			// Sharding fragments the policy; it must still clear most of the gain.
			r.GreaterOrEqual(gotSharded, lru+w.lruMargin-2)

			requireTinyLFUInvariants(t, single)
			requireTinyLFUInvariants(t, sharded)
		})
	}
}

func BenchmarkTinyLFU_Parallel_Get(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewTinyLFU[int, int](size)
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

// BenchmarkComparison_Mixed90 compares the sharded types and Cache on the
// 90/10 read/write mix used throughout the shard benchmarks.
func BenchmarkComparison_Mixed90(b *testing.B) {
	const size = 10000

	run := func(get func(int) (int, bool), set func(int, int)) func(*testing.B) {
		return func(b *testing.B) {
			for i := 0; i < size; i++ {
				set(i, i)
			}
			b.ResetTimer()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					if i%10 == 0 {
						set(i%size, i)
					} else {
						get(i % size)
					}
					i++
				}
			})
		}
	}

	cache := MustNew[int, int](2 * size)
	b.Run("impl=Cache", run(cache.Get, cache.Set))

	sharded := MustNewSharded[int, int](2 * size)
	b.Run("impl=Sharded", run(sharded.Get, sharded.Set))

	clock := MustNewClock[int, int](2 * size)
	b.Run("impl=Clock", run(clock.Get, clock.Set))

	tiny := MustNewTinyLFU[int, int](2 * size)
	b.Run("impl=TinyLFU", run(tiny.Get, tiny.Set))
}

package lru

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// Benchmark sizes to test different cache behaviors
var benchSizes = []int{100, 1_000, 10_000, 100_000}

// =============================================================================
// Cache Benchmarks
// =============================================================================

func BenchmarkCache_Get_Hit(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNew[int, int](size)
			for i := 0; i < size; i++ {
				cache.Set(i, i)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				cache.Get(i % size)
			}
		})
	}
}

func BenchmarkCache_Get_Miss(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNew[int, int](size)
			// leave cache empty

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				cache.Get(i)
			}
		})
	}
}

func BenchmarkCache_Set_New(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNew[int, int](size)

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				cache.Set(i%size, i)
			}
		})
	}
}

func BenchmarkCache_Set_Existing(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNew[int, int](size)
			for i := 0; i < size; i++ {
				cache.Set(i, i)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				cache.Set(i%size, i)
			}
		})
	}
}

func BenchmarkCache_Set_Evict(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNew[int, int](size)
			// fill the cache
			for i := 0; i < size; i++ {
				cache.Set(i, i)
			}

			b.ResetTimer()
			b.ReportAllocs()

			// every set evicts the oldest entry
			for i := 0; i < b.N; i++ {
				cache.Set(size+i, i)
			}
		})
	}
}

// Mixed workload: 80% reads, 20% writes (common cache pattern)
func BenchmarkCache_Mixed_80Read_20Write(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNew[int, int](size)
			for i := 0; i < size; i++ {
				cache.Set(i, i)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				if i%5 == 0 {
					cache.Set(i%size, i)
				} else {
					cache.Get(i % size)
				}
			}
		})
	}
}

// =============================================================================
// Cache Parallel Benchmarks (contention testing)
// =============================================================================

func BenchmarkCache_Parallel_Get(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
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
	}
}

func BenchmarkCache_Parallel_Set(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNew[int, int](size)

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

func BenchmarkCache_Parallel_Mixed(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNew[int, int](size)
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

// High contention: many goroutines hitting the same keys
func BenchmarkCache_Parallel_HighContention(b *testing.B) {
	cache := MustNew[int, int](100)
	for i := 0; i < 100; i++ {
		cache.Set(i, i)
	}

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			// only 10 hot keys
			key := i % 10
			if i%5 == 0 {
				cache.Set(key, i)
			} else {
				cache.Get(key)
			}
			i++
		}
	})
}

// =============================================================================
// Expirable Benchmarks
// =============================================================================

func BenchmarkExpirable_Get_Hit(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewExpirable[int, int](size, time.Hour)
			for i := 0; i < size; i++ {
				cache.Set(i, i)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				cache.Get(i % size)
			}
		})
	}
}

func BenchmarkExpirable_Get_Miss(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewExpirable[int, int](size, time.Hour)

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				cache.Get(i)
			}
		})
	}
}

func BenchmarkExpirable_Set_New(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewExpirable[int, int](size, time.Hour)

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				cache.Set(i%size, i)
			}
		})
	}
}

func BenchmarkExpirable_Set_Evict(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewExpirable[int, int](size, time.Hour)
			for i := 0; i < size; i++ {
				cache.Set(i, i)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				cache.Set(size+i, i)
			}
		})
	}
}

// benchExpiredChunk bounds how many expired entries are staged per refill so
// timer toggles stay rare (StopTimer/StartTimer read memstats) and memory
// stays bounded regardless of b.N.
const benchExpiredChunk = 65536

// BenchmarkExpirable_Set_FullWithExpired measures the amortized cost per
// expired entry purged by a Set into a cache whose storage is full of expired
// entries; b.N counts purged entries, not Set calls.
func BenchmarkExpirable_Set_FullWithExpired(b *testing.B) {
	chunk := benchExpiredChunk
	if b.N < chunk {
		chunk = b.N
	}

	now := time.Now()

	b.ReportAllocs()
	b.ResetTimer()

	for purged := 0; purged < b.N; {
		n := chunk
		if b.N-purged < n {
			n = b.N - purged
		}

		b.StopTimer()
		cache := MustNewExpirable[int, int](n, time.Hour)
		cache.SetTimeNowFunc(func() time.Time { return now })
		for j := 0; j < n; j++ {
			cache.Set(j, j)
		}
		now = now.Add(2 * time.Hour)
		b.StartTimer()

		// storage is full, so this purges all n expired entries
		cache.Set(-1, 0)
		purged += n
	}
}

func BenchmarkExpirable_GetWithTTL(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewExpirable[int, int](size, time.Hour)
			for i := 0; i < size; i++ {
				cache.Set(i, i)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				cache.GetWithTTL(i % size)
			}
		})
	}
}

// BenchmarkExpirable_Get_WithExpiration measures Get hitting an expired entry
// (lazy removal path). Entries are re-staged in untimed chunks so every timed
// Get removes exactly one expired entry rather than degrading into misses on
// an empty cache.
func BenchmarkExpirable_Get_WithExpiration(b *testing.B) {
	chunk := benchExpiredChunk
	if b.N < chunk {
		chunk = b.N
	}

	cache := MustNewExpirable[int, int](chunk, time.Hour)

	// use a mock time function to control expiration
	now := time.Now()
	cache.SetTimeNowFunc(func() time.Time { return now })

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if i%chunk == 0 {
			b.StopTimer()
			for j := 0; j < chunk; j++ {
				cache.Set(j, j)
			}
			now = now.Add(2 * time.Hour)
			b.StartTimer()
		}

		cache.Get(i % chunk)
	}
}

// BenchmarkExpirable_RemoveExpired measures the amortized cost per entry
// removed by RemoveExpired; b.N counts removed entries, not RemoveExpired
// calls.
func BenchmarkExpirable_RemoveExpired(b *testing.B) {
	chunk := benchExpiredChunk
	if b.N < chunk {
		chunk = b.N
	}

	cache := MustNewExpirable[int, int](chunk, time.Hour)

	now := time.Now()
	cache.SetTimeNowFunc(func() time.Time { return now })

	b.ReportAllocs()
	b.ResetTimer()

	for removed := 0; removed < b.N; {
		n := chunk
		if b.N-removed < n {
			n = b.N - removed
		}

		b.StopTimer()
		// refill with entries that expire immediately below
		for j := 0; j < n; j++ {
			cache.Set(j, j)
		}
		now = now.Add(2 * time.Hour)
		b.StartTimer()

		cache.RemoveExpired()
		removed += n
	}
}

// =============================================================================
// Expirable Parallel Benchmarks
// =============================================================================

func BenchmarkExpirable_Parallel_Get(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewExpirable[int, int](size, time.Hour)
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

func BenchmarkExpirable_Parallel_Set(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewExpirable[int, int](size, time.Hour)

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

func BenchmarkExpirable_Parallel_Mixed(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNewExpirable[int, int](size, time.Hour)
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

// =============================================================================
// GetOrSet Benchmarks (important for memoization use case)
// =============================================================================

func BenchmarkCache_GetOrSet_Hit(b *testing.B) {
	cache := MustNew[int, int](1000)
	for i := 0; i < 1000; i++ {
		cache.Set(i, i)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cache.GetOrSet(i%1000, func() (int, error) {
			return i, nil
		})
	}
}

func BenchmarkCache_GetOrSet_Miss(b *testing.B) {
	cache := MustNew[int, int](b.N + 1)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cache.GetOrSet(i, func() (int, error) {
			return i, nil
		})
	}
}

func BenchmarkCache_GetOrSet_Parallel(b *testing.B) {
	cache := MustNew[int, int](1000)
	for i := 0; i < 1000; i++ {
		cache.Set(i, i)
	}

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.GetOrSet(i%1000, func() (int, error) {
				return i, nil
			})
			i++
		}
	})
}

// =============================================================================
// String key benchmarks (common real-world use case)
// =============================================================================

func BenchmarkCache_StringKey_Get(b *testing.B) {
	cache := MustNew[string, int](1000)
	keys := make([]string, 1000)
	for i := 0; i < 1000; i++ {
		keys[i] = fmt.Sprintf("key-%d", i)
		cache.Set(keys[i], i)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cache.Get(keys[i%1000])
	}
}

func BenchmarkCache_StringKey_Set(b *testing.B) {
	cache := MustNew[string, int](1000)
	keys := make([]string, 1000)
	for i := 0; i < 1000; i++ {
		keys[i] = fmt.Sprintf("key-%d", i)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cache.Set(keys[i%1000], i)
	}
}

// =============================================================================
// Zipf distribution (realistic access pattern)
// =============================================================================

func BenchmarkCache_Zipf(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNew[int, int](size)
			for i := 0; i < size; i++ {
				cache.Set(i, i)
			}

			// zipf distribution: some keys accessed much more than others
			rng := rand.New(rand.NewSource(42))
			zipf := rand.NewZipf(rng, 1.2, 1, uint64(size-1))

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				key := int(zipf.Uint64())
				if i%5 == 0 {
					cache.Set(key, i)
				} else {
					cache.Get(key)
				}
			}
		})
	}
}

// =============================================================================
// Memory allocation focused benchmarks
// =============================================================================

func BenchmarkCache_Allocs_Set(b *testing.B) {
	cache := MustNew[int, int](b.N + 1)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cache.Set(i, i)
	}
}

func BenchmarkExpirable_Allocs_Set(b *testing.B) {
	cache := MustNewExpirable[int, int](b.N+1, time.Hour)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cache.Set(i, i)
	}
}

// =============================================================================
// Comparison: Contains vs Get vs Peek (for "exists" checks)
// =============================================================================

func BenchmarkCache_Contains(b *testing.B) {
	cache := MustNew[int, int](1000)
	for i := 0; i < 1000; i++ {
		cache.Set(i, i)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cache.Contains(i % 1000)
	}
}

func BenchmarkCache_Peek(b *testing.B) {
	cache := MustNew[int, int](1000)
	for i := 0; i < 1000; i++ {
		cache.Set(i, i)
	}

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		cache.Peek(i % 1000)
	}
}

func BenchmarkCache_Parallel_Peek(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			cache := MustNew[int, int](size)
			for i := 0; i < size; i++ {
				cache.Set(i, i)
			}

			b.ResetTimer()
			b.ReportAllocs()

			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					cache.Peek(i % size)
					i++
				}
			})
		})
	}
}

// =============================================================================
// Concurrent reader/writer scenarios
// =============================================================================

func BenchmarkCache_ConcurrentReadersOneWriter(b *testing.B) {
	cache := MustNew[int, int](1000)
	for i := 0; i < 1000; i++ {
		cache.Set(i, i)
	}

	var wg sync.WaitGroup

	b.ResetTimer()
	b.ReportAllocs()

	// start writer goroutine
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-done:
				return
			default:
				cache.Set(i%1000, i)
				i++
			}
		}
	}()

	// readers in parallel
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			cache.Get(i % 1000)
			i++
		}
	})

	close(done)
	wg.Wait()
}

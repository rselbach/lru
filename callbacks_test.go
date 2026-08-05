package lru

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRemovalReasonString(t *testing.T) {
	want := []string{"unknown", "capacity", "explicit", "expired", "clear", "resize", "admission"}
	for reason, name := range want {
		if got := RemovalReason(reason).String(); got != name {
			t.Fatalf("reason %d: got %q, want %q", reason, got, name)
		}
	}
	if got := RemovalReason(255).String(); got != "unknown" {
		t.Fatalf("unknown reason = %q", got)
	}
}

func TestCacheOnRemoveReasonsAndOrdering(t *testing.T) {
	cache := MustNew[int, int](2)
	var reasons []RemovalReason
	var order []string
	cache.OnEvict(func(int, int) { order = append(order, "evict") })
	cache.OnRemove(func(_ int, _ int, reason RemovalReason) {
		// Reentry proves the callback runs after unlocking.
		_ = cache.Len()
		order = append(order, "remove")
		reasons = append(reasons, reason)
	})
	cache.Set(1, 1)
	cache.Set(2, 2)
	cache.Set(3, 3)
	cache.Remove(2)
	cache.Set(4, 4)
	cache.Set(5, 5)
	if _, err := cache.Resize(1); err != nil {
		t.Fatal(err)
	}
	cache.Clear()
	want := []RemovalReason{RemovalReasonCapacity, RemovalReasonExplicit, RemovalReasonCapacity, RemovalReasonResize, RemovalReasonClear}
	if !reflect.DeepEqual(reasons, want) {
		t.Fatalf("reasons = %v, want %v", reasons, want)
	}
	for i := 0; i < len(order); i += 2 {
		if order[i] != "evict" || order[i+1] != "remove" {
			t.Fatalf("callback order = %v", order)
		}
	}
	cache.OnRemove(nil)
}

func TestTinyLFUOnRemoveAdmissionAndCapacity(t *testing.T) {
	admission := MustNewTinyLFUWithCount[int, int](2, 1)
	var got []RemovalReason
	admission.OnRemove(func(_, _ int, reason RemovalReason) { got = append(got, reason) })
	admission.Set(1, 1)
	admission.Set(2, 2)
	admission.Set(3, 3) // equal-frequency candidate 2 is rejected
	if !reflect.DeepEqual(got, []RemovalReason{RemovalReasonAdmission}) {
		t.Fatalf("admission = %v", got)
	}

	displacement := MustNewTinyLFUWithCount[int, int](2, 1)
	got = nil
	displacement.OnRemove(func(_, _ int, reason RemovalReason) { got = append(got, reason) })
	displacement.Set(1, 1)
	displacement.Set(2, 2)
	_, hash := displacement.shardFor(2)
	displacement.shards[0].sketch.increment(hash)
	displacement.Set(3, 3)
	if !reflect.DeepEqual(got, []RemovalReason{RemovalReasonCapacity}) {
		t.Fatalf("displacement = %v", got)
	}
}

func TestExpirableOnRemoveExpired(t *testing.T) {
	now := time.Unix(1, 0)
	cache := MustNewExpirable[int, int](1, time.Second)
	cache.SetTimeNowFunc(func() time.Time { return now })
	var got RemovalReason
	cache.OnRemove(func(_, _ int, reason RemovalReason) { got = reason })
	cache.Set(1, 1)
	now = now.Add(2 * time.Second)
	cache.Get(1)
	if got != RemovalReasonExpired {
		t.Fatalf("reason = %v", got)
	}
}

func TestOnRemoveSupportedByEveryCacheType(t *testing.T) {
	type callbackCache struct {
		name     string
		set      func(int, int)
		remove   func(int) bool
		onRemove func(OnRemoveFunc[int, int])
	}
	var tests []callbackCache
	lruCache := MustNew[int, int](1)
	tests = append(tests, callbackCache{"Cache", lruCache.Set, lruCache.Remove, lruCache.OnRemove})
	expirable := MustNewExpirable[int, int](1, time.Hour)
	tests = append(tests, callbackCache{"Expirable", func(key, value int) { expirable.Set(key, value) }, expirable.Remove, expirable.OnRemove})
	sharded := MustNewShardedWithCount[int, int](1, 1)
	tests = append(tests, callbackCache{"Sharded", sharded.Set, sharded.Remove, sharded.OnRemove})
	clock := MustNewClockWithCount[int, int](1, 1)
	tests = append(tests, callbackCache{"Clock", clock.Set, clock.Remove, clock.OnRemove})
	tinyLFU := MustNewTinyLFUWithCount[int, int](1, 1)
	tests = append(tests, callbackCache{"TinyLFU", tinyLFU.Set, tinyLFU.Remove, tinyLFU.OnRemove})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []RemovalReason
			tt.onRemove(func(_, _ int, reason RemovalReason) {
				got = append(got, reason)
			})
			tt.set(1, 1)
			tt.set(2, 2)
			if !tt.remove(2) {
				t.Fatal("Remove(2) = false")
			}
			if want := []RemovalReason{RemovalReasonCapacity, RemovalReasonExplicit}; !reflect.DeepEqual(got, want) {
				t.Fatalf("reasons = %v, want %v", got, want)
			}

			tt.onRemove(nil)
			tt.set(3, 3)
			tt.set(4, 4)
			if !reflect.DeepEqual(got, []RemovalReason{RemovalReasonCapacity, RemovalReasonExplicit}) {
				t.Fatalf("nil did not clear callback: %v", got)
			}
		})
	}
}

func TestCache_OnEvict(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](3)

	evicted := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted[key] = value
	})

	// Add items to the cache
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// No evictions yet
	r.Empty(evicted)

	// This should evict "a" since it's the least recently used
	cache.Set("d", 4)
	r.Equal(map[string]int{"a": 1}, evicted)

	// Test explicit removal
	cache.Remove("b")
	r.Equal(map[string]int{"a": 1, "b": 2}, evicted)

	// Update "c" - should not trigger eviction
	cache.Set("c", 30)
	r.Equal(map[string]int{"a": 1, "b": 2}, evicted)

	// Clear the cache - should evict all remaining items
	cache.Clear()
	r.Equal(map[string]int{"a": 1, "b": 2, "c": 30, "d": 4}, evicted)
}

func TestCache_OnEvictReplacement(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](3)

	evicted1 := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted1[key] = value
	})

	// Add items and cause an eviction
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)
	cache.Set("d", 4) // should evict "a"

	r.Equal(map[string]int{"a": 1}, evicted1)

	// Replace the callback
	evicted2 := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted2[key] = value
	})

	// Cause another eviction
	cache.Set("e", 5) // should evict "b"

	// The new callback should be called, not the old one
	r.Equal(map[string]int{"a": 1}, evicted1)
	r.Equal(map[string]int{"b": 2}, evicted2)

	// Set callback to nil
	cache.OnEvict(nil)

	// Cause another eviction
	cache.Set("f", 6) // should evict "c"

	// No callback should be called
	r.Equal(map[string]int{"a": 1}, evicted1)
	r.Equal(map[string]int{"b": 2}, evicted2)
}

func TestCache_OnEvictConcurrentReplacement(t *testing.T) {
	r := require.New(t)
	cache := MustNew[int, int](1)
	cache.Set(0, 0)

	var callback1Calls int32
	var callback2Calls int32
	callback1 := func(int, int) {
		atomic.AddInt32(&callback1Calls, 1)
		cache.Len()
	}
	callback2 := func(int, int) {
		atomic.AddInt32(&callback2Calls, 1)
		cache.Contains(0)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			switch i % 3 {
			case 0:
				cache.OnEvict(callback1)
			case 1:
				cache.OnEvict(callback2)
			default:
				cache.OnEvict(nil)
			}
		}
	}()

	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				cache.Set(base*1000+i+1, i)
			}
		}(worker)
	}
	wg.Wait()

	r.LessOrEqual(cache.Len(), cache.Capacity())
}

func TestExpirable_OnEvict(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](3, time.Minute)
	r.NoError(err)

	cache.SetTimeNowFunc(mockClock.Now)

	evicted := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted[key] = value
	})

	// Add items to the cache
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// No evictions yet
	r.Empty(evicted)

	// This should evict "a" since it's the least recently used
	cache.Set("d", 4)
	r.Equal(map[string]int{"a": 1}, evicted)

	// Test explicit removal
	cache.Remove("b")
	r.Equal(map[string]int{"a": 1, "b": 2}, evicted)

	// Advance time past expiration
	mockClock.Add(time.Minute + time.Second)

	// The expired items won't be evicted until accessed or RemoveExpired is called
	r.Equal(map[string]int{"a": 1, "b": 2}, evicted)

	// Set below physical capacity does not trigger expiration cleanup.
	cache.Set("e", 5)
	r.Equal(map[string]int{"a": 1, "b": 2}, evicted)

	// Accessing expired items triggers their removal and callback
	_, found := cache.Get("c")
	r.False(found)
	r.Equal(map[string]int{"a": 1, "b": 2, "c": 3}, evicted)

	_, found = cache.Get("d")
	r.False(found)
	r.Equal(map[string]int{"a": 1, "b": 2, "c": 3, "d": 4}, evicted)

	// Add new items to test RemoveExpired with callbacks
	evicted = make(map[string]int) // Reset the eviction map
	cache.Set("f", 6)
	cache.Set("g", 7)

	// Advance time past expiration again
	mockClock.Add(time.Minute + time.Second)

	// Explicit removal should call callbacks
	removed := cache.RemoveExpired()
	r.Equal(3, removed) // should remove e, f, g
	r.Equal(map[string]int{"e": 5, "f": 6, "g": 7}, evicted)
}

func TestExpirable_SetOverExpiredEntryFiresOnEvict(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache := MustNewExpirable[string, int](3, time.Minute)
	cache.SetTimeNowFunc(mockClock.Now)

	evicted := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted[key] = value
	})

	cache.Set("a", 1)
	mockClock.Add(time.Minute + time.Second) // "a" is now expired

	// updating an expired key replaces a dead value; the callback must fire
	cache.Set("a", 2)
	r.Equal(map[string]int{"a": 1}, evicted)

	val, found := cache.Get("a")
	r.True(found)
	r.Equal(2, val)

	// updating a live key is a plain update, no callback
	cache.Set("a", 3)
	r.Equal(map[string]int{"a": 1}, evicted)
}

func TestExpirable_Clear(t *testing.T) {
	r := require.New(t)
	mockClock := newMockTime()

	cache, err := NewExpirable[string, int](3, time.Minute)
	r.NoError(err)

	cache.SetTimeNowFunc(mockClock.Now)

	evicted := make(map[string]int)
	cache.OnEvict(func(key string, value int) {
		evicted[key] = value
	})

	// Add items to the cache
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// No evictions yet
	r.Empty(evicted)

	// Advance time past expiration for "a" and "b" but not "c"
	mockClock.Add(30 * time.Second)
	cache.Set("c", 30)              // update c's TTL
	mockClock.Add(31 * time.Second) // now a and b are expired but c is not

	// Clear reports every physically stored item, including expired entries.
	cache.Clear()
	r.Equal(map[string]int{"a": 1, "b": 2, "c": 30}, evicted)
}

func TestExpirable_OnEvictConcurrentReplacement(t *testing.T) {
	r := require.New(t)
	cache := MustNewExpirable[int, int](1, time.Hour)
	cache.Set(0, 0)

	var callback1Calls int32
	var callback2Calls int32
	callback1 := func(int, int) {
		atomic.AddInt32(&callback1Calls, 1)
		cache.Len()
	}
	callback2 := func(int, int) {
		atomic.AddInt32(&callback2Calls, 1)
		cache.Contains(0)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			switch i % 3 {
			case 0:
				cache.OnEvict(callback1)
			case 1:
				cache.OnEvict(callback2)
			default:
				cache.OnEvict(nil)
			}
		}
	}()

	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				cache.Set(base*1000+i+1, i)
			}
		}(worker)
	}
	wg.Wait()

	r.LessOrEqual(cache.Len(), cache.Capacity())
}

func TestSharded_OnEvictConcurrentReplacement(t *testing.T) {
	r := require.New(t)
	cache := MustNewShardedWithCount[int, int](4, 4)

	for i := 0; i < cache.Capacity(); i++ {
		cache.Set(i, i)
	}

	var callback1Calls int32
	var callback2Calls int32
	callback1 := func(int, int) {
		atomic.AddInt32(&callback1Calls, 1)
		cache.Len()
	}
	callback2 := func(int, int) {
		atomic.AddInt32(&callback2Calls, 1)
		cache.Contains(0)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			switch i % 3 {
			case 0:
				cache.OnEvict(callback1)
			case 1:
				cache.OnEvict(callback2)
			default:
				cache.OnEvict(nil)
			}
		}
	}()

	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				cache.Set(base*1000+i+cache.Capacity(), i)
			}
		}(worker)
	}
	wg.Wait()

	r.LessOrEqual(cache.Len(), cache.Capacity())
}

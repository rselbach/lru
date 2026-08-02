package lru

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type collidingStringKey struct {
	id int
}

func (k collidingStringKey) String() string {
	return "same"
}

func TestAllocationHint(t *testing.T) {
	tests := map[string]struct {
		size int
		want int
	}{
		"empty":          {size: 0, want: 0},
		"small capacity": {size: 32, want: 32},
		"large capacity": {size: 1_000_000, want: initialAllocationLimit},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, allocationHint(tc.size))
		})
	}
}

func TestCache_New(t *testing.T) {
	tests := map[string]struct {
		capacity int
		wantErr  error
	}{
		"valid capacity": {
			capacity: 5,
		},
		"zero capacity": {
			capacity: 0,
			wantErr:  ErrInvalidCapacity,
		},
		"negative capacity": {
			capacity: -1,
			wantErr:  ErrInvalidCapacity,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache, err := New[string, int](tc.capacity)
			if tc.wantErr != nil {
				r.ErrorIs(err, tc.wantErr)
				r.Nil(cache)
			} else {
				r.NoError(err)
				r.NotNil(cache)
				r.Equal(tc.capacity, cache.Capacity())
			}
		})
	}
}

func TestCache_MustNew(t *testing.T) {
	tests := map[string]struct {
		capacity  int
		wantPanic error
	}{
		"valid capacity": {
			capacity: 5,
		},
		"zero capacity": {
			capacity:  0,
			wantPanic: ErrInvalidCapacity,
		},
		"negative capacity": {
			capacity:  -1,
			wantPanic: ErrInvalidCapacity,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			if tc.wantPanic != nil {
				r.PanicsWithError(tc.wantPanic.Error(), func() {
					MustNew[string, int](tc.capacity)
				})
			} else {
				cache := MustNew[string, int](tc.capacity)
				r.NotNil(cache)
				r.Equal(tc.capacity, cache.Capacity())
			}
		})
	}
}

func TestCache_GetSet(t *testing.T) {
	tests := map[string]struct {
		operations []func(c *Cache[string, int])
		want       map[string]int
	}{
		"basic set and get": {
			operations: []func(c *Cache[string, int]){
				func(c *Cache[string, int]) { c.Set("a", 1) },
				func(c *Cache[string, int]) { c.Set("b", 2) },
				func(c *Cache[string, int]) { c.Set("c", 3) },
			},
			want: map[string]int{
				"a": 1,
				"b": 2,
				"c": 3,
			},
		},
		"overwrite value": {
			operations: []func(c *Cache[string, int]){
				func(c *Cache[string, int]) { c.Set("a", 1) },
				func(c *Cache[string, int]) { c.Set("a", 5) },
			},
			want: map[string]int{
				"a": 5,
			},
		},
		"eviction": {
			operations: []func(c *Cache[string, int]){
				func(c *Cache[string, int]) { c.Set("a", 1) },
				func(c *Cache[string, int]) { c.Set("b", 2) },
				func(c *Cache[string, int]) { c.Set("c", 3) },
				func(c *Cache[string, int]) { c.Set("d", 4) },
				func(c *Cache[string, int]) { c.Set("e", 5) },
				func(c *Cache[string, int]) { c.Set("f", 6) }, // should evict "a"
			},
			want: map[string]int{
				"b": 2,
				"c": 3,
				"d": 4,
				"e": 5,
				"f": 6,
			},
		},
		"get affects LRU order": {
			operations: []func(c *Cache[string, int]){
				func(c *Cache[string, int]) { c.Set("a", 1) },
				func(c *Cache[string, int]) { c.Set("b", 2) },
				func(c *Cache[string, int]) { c.Set("c", 3) },
				func(c *Cache[string, int]) { c.Set("d", 4) },
				func(c *Cache[string, int]) { c.Set("e", 5) },
				func(c *Cache[string, int]) { _, _ = c.Get("a") }, // move "a" to front
				func(c *Cache[string, int]) { c.Set("f", 6) },     // should evict "b" now
			},
			want: map[string]int{
				"a": 1,
				"c": 3,
				"d": 4,
				"e": 5,
				"f": 6,
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache := MustNew[string, int](5)
			for _, op := range tc.operations {
				op(cache)
			}

			// verify cache contents
			for k, v := range tc.want {
				got, found := cache.Get(k)
				r.True(found, "key %s should be in cache", k)
				r.Equal(v, got, "value for key %s should be %d", k, v)
			}

			// keys not in tc.want should not be in cache
			r.Equal(len(tc.want), cache.Len())
		})
	}
}

func TestCache_CapacityOne(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](1)

	cache.Set("a", 1)
	cache.Set("b", 2) // evicts "a"

	r.False(cache.Contains("a"))
	val, found := cache.Get("b")
	r.True(found)
	r.Equal(2, val)
	r.Equal(1, cache.Len())
}

func TestCache_SetExistingKeyUpdatesRecency(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](3)

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// updating "a" must also move it to the front
	cache.Set("a", 10)
	r.Equal([]string{"a", "c", "b"}, cache.Keys())

	// so a subsequent eviction removes "b", not "a"
	cache.Set("d", 4)
	r.False(cache.Contains("b"))
	r.Equal([]string{"d", "a", "c"}, cache.Keys())
}

func TestCache_Remove(t *testing.T) {
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

			cache := MustNew[string, int](5)
			for k, v := range tc.setup {
				cache.Set(k, v)
			}

			// test remove
			got := cache.Remove(tc.toRemove)
			r.Equal(tc.want, got)

			// verify key is gone
			_, found := cache.Get(tc.toRemove)
			r.False(found)

			// verify length - only if key was removed
			wantLen := len(tc.setup)
			if tc.want {
				wantLen--
			}
			r.Equal(wantLen, cache.Len(), "cache length should be correct after remove operation")
		})
	}
}

func TestCache_GetOrSet(t *testing.T) {
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
			want:         1, // already in cache, compute not called
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
			wantComputed: true, // compute should be called, but will fail
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			cache := MustNew[string, int](5)
			for k, v := range tc.setup {
				cache.Set(k, v)
			}

			computeCalled := false
			wrappedComputeFunc := func() (int, error) {
				computeCalled = true
				return tc.computeFunc()
			}

			// test GetOrSet
			got, err := cache.GetOrSet(tc.key, wrappedComputeFunc)

			if tc.wantErr {
				r.Error(err)
			} else {
				r.NoError(err)
				r.Equal(tc.want, got)
			}

			r.Equal(tc.wantComputed, computeCalled, "compute function called status")

			// if compute succeeded, verify key is now in cache
			if tc.wantComputed && !tc.wantErr {
				v, found := cache.Get(tc.key)
				r.True(found)
				r.Equal(tc.want, v)
			}
		})
	}
}

func TestCache_GetOrSet_KeyAddedWhileComputing(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](5)

	computeStarted := make(chan struct{})
	release := make(chan struct{})
	result := make(chan int, 1)
	go func() {
		val, _ := cache.GetOrSet("a", func() (int, error) {
			close(computeStarted)
			<-release
			return 10, nil
		})
		result <- val
	}()

	<-computeStarted
	cache.Set("a", 99) // beat the compute to the key
	close(release)

	r.Equal(99, <-result, "GetOrSet must return the value that won the race")
	val, found := cache.Get("a")
	r.True(found)
	r.Equal(99, val)
}

func TestCache_GetOrSetSingleflight_KeyAddedWhileComputing(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](5)

	computeStarted := make(chan struct{})
	release := make(chan struct{})
	result := make(chan int, 1)
	go func() {
		val, _ := cache.GetOrSetSingleflight("a", func() (int, error) {
			close(computeStarted)
			<-release
			return 10, nil
		})
		result <- val
	}()

	<-computeStarted
	cache.Set("a", 99) // beat the compute to the key
	close(release)

	r.Equal(99, <-result, "GetOrSetSingleflight must return the value that won the race")
	val, found := cache.Get("a")
	r.True(found)
	r.Equal(99, val)
}

func TestCache_GetOrSet_CapacityEvictionFiresOnEvict(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](1)

	var evictedKeys []string
	cache.OnEvict(func(key string, _ int) {
		evictedKeys = append(evictedKeys, key)
	})

	cache.Set("a", 1)
	val, err := cache.GetOrSet("b", func() (int, error) { return 2, nil })
	r.NoError(err)
	r.Equal(2, val)
	r.Equal([]string{"a"}, evictedKeys)

	val, err = cache.GetOrSetSingleflight("c", func() (int, error) { return 3, nil })
	r.NoError(err)
	r.Equal(3, val)
	r.Equal([]string{"a", "b"}, evictedKeys)
}

func TestCache_GetOrSet_ComputeMayCallBackIntoCache(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](5)

	// compute runs outside the cache lock, so re-entrant use must not deadlock
	val, err := cache.GetOrSet("a", func() (int, error) {
		cache.Set("b", 2)
		v, ok := cache.Get("b")
		if !ok {
			return 0, fmt.Errorf("b not found")
		}
		return v + 40, nil
	})
	r.NoError(err)
	r.Equal(42, val)
}

func TestCache_Clear(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](5)

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	r.Equal(3, cache.Len())

	cache.Clear()

	r.Equal(0, cache.Len())
	_, found := cache.Get("a")
	r.False(found)
}

func TestCache_Clear_CallbackAfterUnlock(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](2)
	cache.Set("a", 1)
	cache.Set("b", 2)

	callbackLen := make(chan int, 2)
	cache.OnEvict(func(string, int) {
		callbackLen <- cache.Len()
	})

	done := make(chan struct{})
	go func() {
		cache.Clear()
		close(done)
	}()

	select {
	case <-done:
		r.Equal(0, <-callbackLen)
		r.Equal(0, <-callbackLen)
	case <-time.After(time.Second):
		t.Fatal("Clear callback appears to have run while the cache lock was held")
	}
}

func TestCache_Resize(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](3)

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	evicted, err := cache.Resize(5)
	r.NoError(err)
	r.Equal(0, evicted)
	r.Equal(5, cache.Capacity())
	r.Equal([]string{"c", "b", "a"}, cache.Keys())

	cache.Set("d", 4)
	cache.Set("e", 5)
	r.Equal([]string{"e", "d", "c", "b", "a"}, cache.Keys())

	var evictedKeys []string
	cache.OnEvict(func(key string, _ int) {
		evictedKeys = append(evictedKeys, key)
	})

	evicted, err = cache.Resize(2)
	r.NoError(err)
	r.Equal(3, evicted)
	r.Equal(2, cache.Capacity())
	r.Equal([]string{"e", "d"}, cache.Keys())
	r.Equal([]string{"a", "b", "c"}, evictedKeys)

	evicted, err = cache.Resize(0)
	r.ErrorIs(err, ErrInvalidCapacity)
	r.Equal(0, evicted)
	r.Equal(2, cache.Capacity())
}

func TestCache_Resize_CallbackAfterUnlock(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](2)
	cache.Set("a", 1)
	cache.Set("b", 2)

	callbackLen := make(chan int, 1)
	cache.OnEvict(func(string, int) {
		callbackLen <- cache.Len()
	})

	done := make(chan error, 1)
	go func() {
		_, err := cache.Resize(1)
		done <- err
	}()

	select {
	case err := <-done:
		r.NoError(err)
		r.Equal(1, <-callbackLen)
	case <-time.After(time.Second):
		t.Fatal("Resize callback appears to have run while the cache lock was held")
	}
}

func TestCache_Resize_ConcurrentAccess(t *testing.T) {
	r := require.New(t)
	cache := MustNew[int, int](10)

	var wg sync.WaitGroup
	errs := make(chan error, 20*100)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				switch j % 3 {
				case 0:
					cache.Set(base*100+j, j)
				case 1:
					cache.Get(j)
				default:
					_, err := cache.Resize(5 + j%20)
					if err != nil {
						errs <- err
					}
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		r.NoError(err)
	}

	r.LessOrEqual(cache.Len(), cache.Capacity())
}

func TestCache_GetOldestRemoveOldest(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](3)

	key, value, ok := cache.GetOldest()
	r.False(ok)
	r.Empty(key)
	r.Zero(value)

	key, value, ok = cache.RemoveOldest()
	r.False(ok)
	r.Empty(key)
	r.Zero(value)

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	key, value, ok = cache.GetOldest()
	r.True(ok)
	r.Equal("a", key)
	r.Equal(1, value)
	r.Equal([]string{"c", "b", "a"}, cache.Keys())

	_, _ = cache.Get("a")
	key, value, ok = cache.GetOldest()
	r.True(ok)
	r.Equal("b", key)
	r.Equal(2, value)
	r.Equal([]string{"a", "c", "b"}, cache.Keys())

	var evictedKeys []string
	cache.OnEvict(func(key string, _ int) {
		evictedKeys = append(evictedKeys, key)
	})

	key, value, ok = cache.RemoveOldest()
	r.True(ok)
	r.Equal("b", key)
	r.Equal(2, value)
	r.Equal([]string{"a", "c"}, cache.Keys())
	r.Equal([]string{"b"}, evictedKeys)
}

func TestCache_Contains(t *testing.T) {
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
			cache := MustNew[string, int](5)

			for k, v := range tc.setup {
				cache.Set(k, v)
			}

			got := cache.Contains(tc.key)
			r.Equal(tc.want, got)
		})
	}
}

func TestCache_Keys(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](5)

	// empty cache should return empty slice
	r.Empty(cache.Keys())

	// add some items
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// should return keys in order of most recent to least recent
	r.Equal([]string{"c", "b", "a"}, cache.Keys())

	// access 'a' to bring it to front
	_, _ = cache.Get("a")
	r.Equal([]string{"a", "c", "b"}, cache.Keys())
}

func TestCache_Values(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](5)

	r.Empty(cache.Values())

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	r.Equal([]string{"c", "b", "a"}, cache.Keys())
	r.Equal([]int{3, 2, 1}, cache.Values())

	_, _ = cache.Get("a")
	r.Equal([]string{"a", "c", "b"}, cache.Keys())
	r.Equal([]int{1, 3, 2}, cache.Values())
}

func TestCache_Peek(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](5)

	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// peek should return value without affecting LRU order
	val, found := cache.Peek("a")
	r.True(found)
	r.Equal(1, val)

	// order should still be c, b, a (a was not moved to front)
	r.Equal([]string{"c", "b", "a"}, cache.Keys())

	// peek non-existent key
	_, found = cache.Peek("z")
	r.False(found)

	// now use Get to move 'a' to front, then verify Peek didn't affect order before
	_, _ = cache.Get("a")
	r.Equal([]string{"a", "c", "b"}, cache.Keys())
}

func TestCache_GetOrSetSingleflight(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](5)

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

func TestCache_GetOrSetSingleflight_Concurrent(t *testing.T) {
	r := require.New(t)
	cache := MustNew[string, int](5)

	const goroutines = 100
	var computeCount int32
	var wg sync.WaitGroup
	begin := make(chan struct{})
	computeStarted := make(chan struct{})
	release := make(chan struct{})
	results := make([]int, goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-begin
			val, err := cache.GetOrSetSingleflight("shared", func() (int, error) {
				if atomic.AddInt32(&computeCount, 1) == 1 {
					close(computeStarted)
				}
				<-release
				return 42, nil
			})
			results[idx] = val
			errs[idx] = err
		}(i)
	}

	close(begin)
	<-computeStarted
	waitForFlightWaiters(t, &cache.sfGroup, "shared", goroutines-1)
	close(release)
	wg.Wait()

	r.Equal(int32(1), atomic.LoadInt32(&computeCount), "compute should be called exactly once")
	for i, result := range results {
		r.NoError(errs[i], "goroutine %d", i)
		r.Equal(42, result, "goroutine %d got wrong result", i)
	}
}

func TestCache_GetOrSetSingleflight_DistinctStringifiedKeys(t *testing.T) {
	r := require.New(t)
	cache := MustNew[collidingStringKey, string](5)

	computeStarted := make(chan int, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseComputes := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	defer releaseComputes()

	type result struct {
		keyID int
		value string
		err   error
	}
	results := make(chan result, 2)

	for _, tc := range []struct {
		key   collidingStringKey
		value string
	}{
		{key: collidingStringKey{id: 1}, value: "value-1"},
		{key: collidingStringKey{id: 2}, value: "value-2"},
	} {
		tc := tc
		go func() {
			value, err := cache.GetOrSetSingleflight(tc.key, func() (string, error) {
				computeStarted <- tc.key.id
				<-release
				return tc.value, nil
			})
			results <- result{keyID: tc.key.id, value: value, err: err}
		}()
	}

	started := make(map[int]bool)
	for len(started) < 2 {
		select {
		case keyID := <-computeStarted:
			started[keyID] = true
		case <-time.After(time.Second):
			releaseComputes()
			t.Fatalf("want both distinct keys to compute; started: %v", started)
		}
	}
	releaseComputes()

	got := make(map[int]string)
	for i := 0; i < 2; i++ {
		res := <-results
		r.NoError(res.err)
		got[res.keyID] = res.value
	}

	r.Equal(map[int]string{1: "value-1", 2: "value-2"}, got)

	value, found := cache.Peek(collidingStringKey{id: 1})
	r.True(found)
	r.Equal("value-1", value)
	value, found = cache.Peek(collidingStringKey{id: 2})
	r.True(found)
	r.Equal("value-2", value)
}

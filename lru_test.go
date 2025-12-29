package lru

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCache_New(t *testing.T) {
	tests := map[string]struct {
		capacity    int
		expectError bool
	}{
		"valid capacity": {
			capacity:    5,
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

			cache, err := New[string, int](tc.capacity)
			if tc.expectError {
				r.Error(err)
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
		capacity     int
		expectPanic  bool
		panicMessage string
	}{
		"valid capacity": {
			capacity:    5,
			expectPanic: false,
		},
		"zero capacity": {
			capacity:     0,
			expectPanic:  true,
			panicMessage: "capacity must be greater than zero",
		},
		"negative capacity": {
			capacity:     -1,
			expectPanic:  true,
			panicMessage: "capacity must be greater than zero",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)

			if tc.expectPanic {
				r.PanicsWithError(tc.panicMessage, func() {
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
				r.True(found, fmt.Sprintf("key %s should be in cache", k))
				r.Equal(v, got, fmt.Sprintf("value for key %s should be %d", k, v))
			}

			// keys not in tc.want should not be in cache
			r.Equal(len(tc.want), cache.Len())
		})
	}
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
			r.Equal(false, found)

			// verify length - only if key was removed
			expectedLen := len(tc.setup)
			if tc.want {
				expectedLen--
			}
			r.Equal(expectedLen, cache.Len(), "cache length should be correct after remove operation")
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

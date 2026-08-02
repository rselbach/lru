package lru_test

import (
	"fmt"
	"math"

	"github.com/rselbach/lru/v2"
)

// This example demonstrates basic usage of the LRU cache.
func Example_basic() {
	// Create a new LRU cache with a capacity of 3 items
	cache := lru.MustNew[string, int](3)

	// Add items to the cache
	cache.Set("one", 1)
	cache.Set("two", 2)
	cache.Set("three", 3)

	// Get an item from the cache
	value, found := cache.Get("two")
	if found {
		fmt.Printf("Value for 'two': %d\n", value)
	}

	// Adding a fourth item will evict the least recently used item ("one")
	cache.Set("four", 4)

	// "one" is no longer in the cache
	_, found = cache.Get("one")
	fmt.Printf("Is 'one' in the cache? %t\n", found)

	// Print all keys in the cache (most recently used first)
	fmt.Printf("Cache keys: %v\n", cache.Keys())

	// Output:
	// Value for 'two': 2
	// Is 'one' in the cache? false
	// Cache keys: [four two three]
}

// This example demonstrates using GetOrSet for memoizing expensive computations.
func Example_getOrSet() {
	// A simulated expensive computation
	computeCount := 0
	computeExpensive := func(n int) (float64, error) {
		computeCount++
		return math.Pow(float64(n), 2), nil
	}

	cache := lru.MustNew[int, float64](10)

	// First call computes the value
	result, err := cache.GetOrSet(5, func() (float64, error) {
		return computeExpensive(5)
	})
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Printf("Result: %.1f (computed: %t)\n", result, computeCount == 1)

	// Second call gets from cache
	result, err = cache.GetOrSet(5, func() (float64, error) {
		return computeExpensive(5)
	})
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Printf("Result: %.1f (from cache: %t)\n", result, computeCount == 1)

	// Different key computes a new value
	result, err = cache.GetOrSet(10, func() (float64, error) {
		return computeExpensive(10)
	})
	if err != nil {
		fmt.Println("Error:", err)
		return
	}
	fmt.Printf("Result: %.1f (computed: %t)\n", result, computeCount == 2)

	// Output:
	// Result: 25.0 (computed: true)
	// Result: 25.0 (from cache: true)
	// Result: 100.0 (computed: true)
}

func ExampleCache_GetOrSetSingleflight() {
	type requestKey struct {
		userID string
		page   int
	}

	cache := lru.MustNew[requestKey, string](10)
	key := requestKey{userID: "user-42", page: 1}
	computeCount := 0

	getPage := func() (string, error) {
		computeCount++
		return fmt.Sprintf("profile-%s-page-%d", key.userID, key.page), nil
	}

	value, err := cache.GetOrSetSingleflight(key, getPage)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s (computed: %d)\n", value, computeCount)

	value, err = cache.GetOrSetSingleflight(key, getPage)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("%s (computed: %d)\n", value, computeCount)

	// Output:
	// profile-user-42-page-1 (computed: 1)
	// profile-user-42-page-1 (computed: 1)
}

func ExampleCache_Resize() {
	cache := lru.MustNew[string, int](4)
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)
	cache.Set("d", 4)

	evicted, err := cache.Resize(2)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Printf("evicted: %d\n", evicted)
	fmt.Printf("capacity: %d\n", cache.Capacity())
	fmt.Printf("keys: %v\n", cache.Keys())

	// Output:
	// evicted: 2
	// capacity: 2
	// keys: [d c]
}

func ExampleCache_GetOldest() {
	cache := lru.MustNew[string, int](3)
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	key, value, _ := cache.GetOldest()

	fmt.Printf("oldest: %s=%d\n", key, value)
	fmt.Printf("keys: %v\n", cache.Keys())

	// Output:
	// oldest: a=1
	// keys: [c b a]
}

func ExampleCache_RemoveOldest() {
	cache := lru.MustNew[string, int](3)
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	key, value, _ := cache.RemoveOldest()

	fmt.Printf("removed: %s=%d\n", key, value)
	fmt.Printf("keys: %v\n", cache.Keys())

	// Output:
	// removed: a=1
	// keys: [c b]
}

func ExampleCache_Values() {
	cache := lru.MustNew[string, int](3)
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)
	cache.Get("a")

	fmt.Printf("keys: %v\n", cache.Keys())
	fmt.Printf("values: %v\n", cache.Values())

	// Output:
	// keys: [a c b]
	// values: [1 3 2]
}

func ExampleSharded() {
	cache, err := lru.NewShardedWithCount[string, int](8, 2)
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	cache.Set("troy", 42)
	value, found := cache.Get("troy")
	fmt.Printf("shards: %d, capacity: %d\n", cache.ShardCount(), cache.Capacity())
	fmt.Printf("value: %d, found: %t\n", value, found)

	// Output:
	// shards: 2, capacity: 8
	// value: 42, found: true
}

// This example demonstrates eviction of items when the cache is at capacity.
func Example_eviction() {
	// Create a small cache with capacity of 2
	cache := lru.MustNew[string, string](2)

	// Add two items to fill the cache
	cache.Set("A", "Item A")
	cache.Set("B", "Item B")

	// Print current keys
	fmt.Printf("After adding A, B: %v\n", cache.Keys())

	// Access A to make B the least recently used
	cache.Get("A")
	fmt.Printf("After accessing A: %v\n", cache.Keys())

	// Add C, which should evict B
	cache.Set("C", "Item C")
	fmt.Printf("After adding C: %v\n", cache.Keys())

	// Verify B is gone
	_, hasB := cache.Get("B")
	fmt.Printf("Contains B? %t\n", hasB)

	// Output:
	// After adding A, B: [B A]
	// After accessing A: [A B]
	// After adding C: [C A]
	// Contains B? false
}

// This example demonstrates using the eviction callback to track which items are evicted from the cache.
func Example_evictionCallback() {
	// Create a cache with a small capacity
	cache := lru.MustNew[string, int](3)

	// Keep track of evicted items
	evictedKeys := make([]string, 0)
	evictedValues := make([]int, 0)

	// Set the eviction callback
	cache.OnEvict(func(key string, value int) {
		evictedKeys = append(evictedKeys, key)
		evictedValues = append(evictedValues, value)
		fmt.Printf("Evicted: %s=%d\n", key, value)
	})

	// Fill the cache to capacity
	cache.Set("a", 1)
	cache.Set("b", 2)
	cache.Set("c", 3)

	// Adding a fourth item will evict the least recently used one (a)
	cache.Set("d", 4)

	// Explicitly remove an item
	cache.Remove("b")

	// Clear the cache - this will evict all remaining items
	cache.Clear()

	// Print all evicted items in the order they were evicted
	fmt.Printf("All evicted keys: %v\n", evictedKeys)
	fmt.Printf("All evicted values: %v\n", evictedValues)

	// Output:
	// Evicted: a=1
	// Evicted: b=2
	// Evicted: c=3
	// Evicted: d=4
	// All evicted keys: [a b c d]
	// All evicted values: [1 2 3 4]
}

func ExampleClock() {
	// Clock approximates LRU so reads take only a read lock, which lets read
	// throughput rise with core count instead of serializing.
	cache := lru.MustNewClock[string, int](100)

	cache.Set("troy", 1)
	cache.Set("abed", 2)

	value, found := cache.Get("abed")
	fmt.Println("abed:", value, found)

	// Peek reads without marking the entry as recently referenced.
	value, found = cache.Peek("troy")
	fmt.Println("troy:", value, found)
	fmt.Println("len:", cache.Len())

	// Output:
	// abed: 2 true
	// troy: 1 true
	// len: 2
}

func ExampleClock_secondChance() {
	// A single shard makes the eviction hand's path deterministic. Capacity is
	// split across shards, so real deployments leave the default shard count.
	cache := lru.MustNewClockWithCount[string, int](3, 1)

	cache.Set("troy", 1)
	cache.Set("abed", 2)
	cache.Set("britta", 3)

	// Every entry is referenced, so this insert sweeps the ring clearing bits
	// and evicts the first slot.
	cache.Set("shirley", 4)
	fmt.Println("troy cached:", cache.Contains("troy"))

	// Referencing abed buys it a second chance, so the hand passes over it and
	// takes the still-unreferenced britta instead.
	cache.Get("abed")
	cache.Set("pierce", 5)

	fmt.Println("abed cached:", cache.Contains("abed"))
	fmt.Println("britta cached:", cache.Contains("britta"))

	// Output:
	// troy cached: false
	// abed cached: true
	// britta cached: false
}

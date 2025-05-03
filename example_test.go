package lru_test

import (
	"fmt"
	"math"

	"github.com/rselbach/lru"
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
	// Evicted: d=4
	// Evicted: c=3
	// All evicted keys: [a b d c]
	// All evicted values: [1 2 4 3]
}

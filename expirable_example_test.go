package lru_test

import (
	"fmt"
	"sync"
	"time"

	"github.com/rselbach/lru"
)

// This example demonstrates basic usage of the Expirable cache with time-to-live functionality.
func Example_expirableBasic() {
	// Create a new Expirable cache with a capacity of 3 items and a TTL of 1 hour
	cache := lru.MustNewExpirable[string, int](3, time.Hour)

	// Add items to the cache
	cache.Set("one", 1)
	cache.Set("two", 2)
	cache.Set("three", 3)

	// Get an item from the cache
	value, found := cache.Get("two")
	if found {
		fmt.Printf("Value for 'two': %d\n", value)
	}

	// Check if a key exists in the cache
	if cache.Contains("three") {
		fmt.Println("'three' is in the cache")
	}

	// Print all keys in the cache (most recently used first)
	fmt.Printf("Cache keys: %v\n", cache.Keys())

	// Output:
	// Value for 'two': 2
	// 'three' is in the cache
	// Cache keys: [two three one]
}

// This example demonstrates using GetWithTTL to retrieve a value along with its remaining TTL.
func Example_getWithTTL() {
	// For this example, we'll create a small wrapper to simulate the passage of time
	type timedCache struct {
		*lru.Expirable[string, string]
		currentTime time.Time
	}

	simulateTime := func() *timedCache {
		startTime := time.Date(2023, 1, 1, 12, 0, 0, 0, time.UTC)
		cache := lru.MustNewExpirable[string, string](5, 1*time.Hour)

		// Replace the timeNow function with our simulated time
		tc := &timedCache{
			Expirable:   cache,
			currentTime: startTime,
		}
		tc.Expirable.SetTimeNowFunc(func() time.Time {
			return tc.currentTime
		})

		return tc
	}

	advanceTime := func(tc *timedCache, duration time.Duration) {
		tc.currentTime = tc.currentTime.Add(duration)
		fmt.Printf("Time is now: %s\n", tc.currentTime.Format(time.Kitchen))
	}

	// Create our simulated cache
	tc := simulateTime()
	fmt.Printf("Time is now: %s\n", tc.currentTime.Format(time.Kitchen))

	// Add items to the cache
	tc.Set("key1", "value1")
	tc.Set("key2", "value2")

	// Check TTLs
	_, ttl1, _ := tc.GetWithTTL("key1")
	_, ttl2, _ := tc.GetWithTTL("key2")
	fmt.Printf("key1 TTL: %s\n", ttl1.Round(time.Second))
	fmt.Printf("key2 TTL: %s\n", ttl2.Round(time.Second))

	// Advance time by 20 minutes
	advanceTime(tc, 20*time.Minute)

	// Check TTLs again - both should still be valid
	_, ttl1, found1 := tc.GetWithTTL("key1")
	_, ttl2, found2 := tc.GetWithTTL("key2")
	fmt.Printf("key1 TTL: %s (exists: %t)\n", ttl1.Round(time.Second), found1)
	fmt.Printf("key2 TTL: %s (exists: %t)\n", ttl2.Round(time.Second), found2)

	// Advance time past the TTL
	advanceTime(tc, 41*time.Minute) // Now at 1:01 PM (past the 1 hour TTL)

	// Both should be expired now
	_, _, found1 = tc.GetWithTTL("key1")
	_, _, found2 = tc.GetWithTTL("key2")
	fmt.Printf("key1 exists: %t\n", found1)
	fmt.Printf("key2 exists: %t\n", found2)

	// Remove expired entries
	removed := tc.RemoveExpired()
	fmt.Printf("Removed %d expired entries\n", removed)

	// Output:
	// Time is now: 12:00PM
	// key1 TTL: 1h0m0s
	// key2 TTL: 1h0m0s
	// Time is now: 12:20PM
	// key1 TTL: 40m0s (exists: true)
	// key2 TTL: 40m0s (exists: true)
	// Time is now: 1:01PM
	// key1 exists: false
	// key2 exists: false
	// Removed 2 expired entries
}

// This example demonstrates cache eviction based on both LRU and expiration.
func Example_lruAndExpiration() {
	// Create a new cache with a small capacity
	cache := lru.MustNewExpirable[string, string](2, 1*time.Minute)

	// Override time function for testing
	var mutex sync.Mutex
	simulatedTime := time.Now()
	cache.SetTimeNowFunc(func() time.Time {
		mutex.Lock()
		defer mutex.Unlock()
		return simulatedTime
	})

	// Function to advance time
	advanceTime := func(duration time.Duration) {
		mutex.Lock()
		defer mutex.Unlock()
		simulatedTime = simulatedTime.Add(duration)
	}

	// Add two items to fill the cache
	cache.Set("A", "Item A")
	cache.Set("B", "Item B")

	fmt.Printf("After adding A, B: %v\n", cache.Keys())

	// Access A to make B the least recently used
	_, _ = cache.Get("A")
	fmt.Printf("After accessing A: %v\n", cache.Keys())

	// Add C, which should evict B due to LRU
	cache.Set("C", "Item C")
	fmt.Printf("After adding C: %v\n", cache.Keys())

	// Advance time past expiration for all entries
	advanceTime(61 * time.Second) // Now past the 1 minute TTL

	// This should only return D since all other items have expired and
	// our Set operation automatically removes expired items
	cache.Set("D", "Item D")
	fmt.Printf("After adding D: %v\n", cache.Keys())

	// Output:
	// After adding A, B: [B A]
	// After accessing A: [A B]
	// After adding C: [C A]
	// After adding D: [D]
}

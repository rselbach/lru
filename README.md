# Generic LRU Cache for Go

A thread-safe, generic Least Recently Used (LRU) cache implementation in Go, with optional time-to-live functionality.

## Features

- Generic implementation using Go generics (requires Go 1.18+)
- Efficient key-value lookups with O(1) complexity
- Thread-safe operations (supports concurrent access)
- Configurable capacity
- Automatic eviction of least recently used items when capacity is reached
- Comprehensive API for common cache operations
- Optional time-based expiry mechanism with configurable TTL

## Installation

```shell
go get github.com/rselbach/lru
```

## Usage

### Basic LRU Cache Usage

```go
package main

import (
	"fmt"

	"github.com/rselbach/lru"
)

func main() {
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
}
```

### Using GetOrSet for computation memoization

```go
package main

import (
	"fmt"
	"math"

	"github.com/rselbach/lru"
)

// A function that's expensive to compute
func computeExpensiveValue(n float64) (float64, error) {
	fmt.Printf("Computing expensive value for %f...\n", n)
	return math.Pow(n, 3), nil
}

func main() {
	cache := lru.MustNew[float64, float64](100)

	// Compute value for the first time
	key := 42.0
	value, err := cache.GetOrSet(key, func() (float64, error) {
		return computeExpensiveValue(key)
	})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	fmt.Printf("Value: %f\n", value)

	// Second time will be retrieved from cache
	value, err = cache.GetOrSet(key, func() (float64, error) {
		return computeExpensiveValue(key)
	})
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	fmt.Printf("Value (from cache): %f\n", value)
}
```

### Using Expirable Cache with TTL

```go
package main

import (
	"fmt"
	"time"

	"github.com/rselbach/lru"
)

func main() {
	// Create a new expirable cache with a capacity of 3 items and 1 minute TTL
	cache := lru.MustNewExpirable[string, int](3, time.Minute)

	// Add items to the cache
	cache.Set("one", 1)
	cache.Set("two", 2)
	cache.Set("three", 3)
	
	// Get an item and check its remaining TTL
	value, ttl, found := cache.GetWithTTL("two")
	if found {
		fmt.Printf("Value: %d, TTL: %v\n", value, ttl)
	}
	
	// Wait for all items to expire
	fmt.Println("Waiting for items to expire...")
	time.Sleep(61 * time.Second)
	
	// All items should be expired now
	if !cache.Contains("one") && !cache.Contains("two") && !cache.Contains("three") {
		fmt.Println("All items have expired")
	}
	
	// Add a new item
	cache.Set("four", 4)
	
	// Now only the new item is in the cache
	fmt.Printf("Items in cache: %v\n", cache.Keys())
}
```

## API

### Creating a cache

#### Standard LRU cache

```go
// Create a new cache with error handling
cache, err := lru.New[KeyType, ValueType](capacity)
if err != nil {
    // handle error
}

// Create a new cache, panic if capacity is invalid
cache := lru.MustNew[KeyType, ValueType](capacity)
```

#### Expirable LRU cache with TTL

```go
// Create a new expirable cache with error handling
cache, err := lru.NewExpirable[KeyType, ValueType](capacity, ttl)
if err != nil {
    // handle error
}

// Create a new expirable cache, panic if capacity or TTL is invalid
cache := lru.MustNewExpirable[KeyType, ValueType](capacity, ttl)
```

### Standard Cache operations

- `Get(key K) (V, bool)` - Get a value from the cache
- `Set(key K, value V)` - Add or update a value in the cache
- `GetOrSet(key K, compute func() (V, error)) (V, error)` - Get a value or compute and store it if not present
- `Remove(key K) bool` - Remove an item from the cache
- `Contains(key K) bool` - Check if a key exists in the cache
- `Len() int` - Get the number of items in the cache
- `Capacity() int` - Get the maximum capacity of the cache
- `Clear()` - Remove all items from the cache
- `Keys() []K` - Get all keys in the cache (ordered by most to least recently used)

### Expirable Cache additional operations

- `GetWithTTL(key K) (V, time.Duration, bool)` - Get a value and its remaining TTL
- `TTL() time.Duration` - Get the time-to-live duration for cache entries
- `SetTTL(ttl time.Duration) error` - Update the TTL for future cache entries
- `RemoveExpired() int` - Explicitly remove all expired entries

## Thread Safety

All operations on the cache are thread-safe. The cache uses read-write locks to allow concurrent reads while ensuring exclusive access during writes.

## License

[MIT License](LICENSE)
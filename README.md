# Generic LRU Cache for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/rselbach/lru.svg)](https://pkg.go.dev/github.com/rselbach/lru)

A thread-safe, generic LRU cache implementation in Go with optional TTL
expiration, sharding, and eviction callbacks.

## Features

- Generic implementation (Go 1.18+)
- O(1) lookups, insertions, and deletions in the standard cache
- Expirable writes avoid cleanup scans until the earliest expiry is due
- Thread-safe for concurrent access
- Optional time-based expiration (`Expirable`)
- Sharded cache for reduced lock contention (`Sharded`)
- `GetOrSet` / `GetOrSetSingleflight` memoization
- Eviction callbacks and `Resize` on every cache type
- Oldest-entry helpers on non-sharded caches

## Cache Types

| Capability | `Cache` | `Expirable` | `Sharded` |
| --- | --- | --- | --- |
| LRU scope | Global | Global | Per shard |
| TTL expiration | No | Yes | No |
| `Resize` / callbacks | Yes | Yes | Yes, per shard |
| Oldest-entry helpers | Yes | Yes | No |
| `Len` complexity | O(1) | O(n), live entries only | O(shards) |

`Get` updates recency and therefore takes an exclusive cache lock. Use `Peek`
when a read should neither change recency nor serialize with other readers.

## Installation

```shell
go get github.com/rselbach/lru
```

## Quick Start

```go
cache := lru.MustNew[string, int](100)
cache.Set("key", 42)
value, found := cache.Get("key")
```

With TTL expiration:

```go
cache := lru.MustNewExpirable[string, int](100, 5*time.Minute)
cache.Set("key", 42)
value, ttl, found := cache.GetWithTTL("key")

// optional background purge of expired entries
if err := cache.StartJanitor(time.Minute); err != nil {
    log.Fatal(err)
}
defer cache.StopJanitor()
```

Memoize an expensive compute, deduplicating concurrent misses:

```go
value, err := cache.GetOrSetSingleflight("key", func() (int, error) {
    return fetchFromDB("key")
})
if err != nil {
    log.Fatal(err)
}
```

Sharded cache for high-concurrency workloads (per-shard LRU, not global):

```go
cache := lru.MustNewSharded[string, int](10_000)
cache.Set("key", 42)
```

See the [package documentation](https://pkg.go.dev/github.com/rselbach/lru)
for complete API reference, eviction-callback rules, and expiry semantics.

## License

[MIT License](LICENSE)

# Generic LRU Cache for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/rselbach/lru/v2.svg)](https://pkg.go.dev/github.com/rselbach/lru/v2)

A thread-safe, generic LRU cache implementation in Go with optional TTL
expiration, sharding, and eviction callbacks.

## Version 2

Version 2 uses the `github.com/rselbach/lru/v2` import path. Compared with v1:

- non-positive `WithTTL` overrides are rejected instead of using the default;
- explicit shard counts greater than capacity return an error instead of being
  clamped; and
- `Expirable.Clear` reports unpurged expired entries to eviction callbacks.

The v1 API remains available at `github.com/rselbach/lru`.

## Features

- Generic implementation (Go 1.18+)
- O(1) keyed lookups and keyed deletions in every cache type
- Amortized O(1) insertions; `Expirable` and `TinyLFU` occasionally perform
  linear maintenance, and a `Clock` insertion can scan one shard
- Expirable writes avoid cleanup scans until the earliest expiry is due
- Thread-safe for concurrent access
- Optional time-based expiration (`Expirable`)
- Sharded cache for reduced lock contention (`Sharded`)
- Approximate-LRU cache whose reads scale with cores (`Clock`)
- W-TinyLFU admission cache that resists scans and loops (`TinyLFU`)
- `GetOrSet` / `GetOrSetSingleflight` memoization
- Eviction callbacks and `Resize` on every cache type
- Consistent key/value pair capture with `Items`
- Oldest-entry helpers on non-sharded caches

## Cache Types

| Capability | `Cache` | `Expirable` | `Sharded` | `Clock` | `TinyLFU` |
| --- | --- | --- | --- | --- | --- |
| Eviction policy | Exact LRU | Exact LRU + TTL | Exact LRU per shard | Approximate (CLOCK) | W-TinyLFU admission |
| LRU scope | Global | Global | Per shard | Per shard | Per shard |
| TTL expiration | No | Yes | No | No | No |
| `Resize` / callbacks | Yes | Yes | Yes, per shard | Yes, per shard | Yes, per shard |
| Oldest-entry helpers | Yes | Yes | No | No | No |
| `Keys` / `Values` order | MRU to LRU | MRU to LRU | Per shard | Unspecified | Unspecified |
| Reads scale with cores | No | No | No | Yes | Yes |
| Scan and loop resistant | No | No | No | No | Yes |
| `Len` complexity | O(1) | O(n), live entries only | O(shards) | O(shards) | O(shards) |

On `Cache`, `Expirable`, and `Sharded`, `Get` updates recency and therefore
takes an exclusive cache lock, so concurrent `Get` calls serialize. Use `Peek`
when a read should neither change recency nor serialize with other readers.

`Clock` and `TinyLFU` maintain no exact recency order, so their `Get` takes
only a read lock and their read throughput rises rather than falls as cores
are added, at the cost of approximate eviction and unordered `Keys`/`Values`.

Between the two: `Clock` optimizes what a hit costs, `TinyLFU` optimizes how
often you hit. A point of hit rate saves a hundredth of the miss cost per
request, so when a miss costs more than about a microsecond (a database query,
RPC, or disk read), `TinyLFU`'s admission policy is the better default; it also
survives scans and loops that flush LRU-family caches. Prefer `Clock` when
misses are nearly free, when the working set fits in capacity, or when traffic
concentrates on a single hot key. Note that `TinyLFU` admission may evict a
just-written cold key before it is ever read; use `Clock` or `Cache` when a
stored entry must survive until evicted by pressure.

`Sharded` helps only when concurrent keys spread across shards. One dominant
key routes every operation to the same shard, where hashing is pure overhead,
and a single-goroutine workload pays that overhead with no contention to offset
it. `DefaultShardCount` (16) suits moderate concurrency; on many-core machines
throughput keeps improving well past it, so use `NewShardedWithCount` when many
goroutines share one cache and the capacity leaves each shard a useful number
of entries.

## Installation

```shell
go get github.com/rselbach/lru/v2
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

Read-heavy workloads that can accept approximate eviction (`Clock` reads take a
read lock, so they scale with cores instead of serializing):

```go
cache := lru.MustNewClock[string, int](10_000)
cache.Set("key", 42)
value, found := cache.Get("key")
```

Caches in front of expensive misses, or traffic with scans and loops
(`TinyLFU`'s admission policy keeps one-shot keys from flushing the working
set):

```go
cache := lru.MustNewTinyLFU[string, int](10_000)
cache.Set("key", 42)
value, found := cache.Get("key")
```

See the [package documentation](https://pkg.go.dev/github.com/rselbach/lru/v2)
for complete API reference, eviction-callback rules, and expiry semantics.

## License

[MIT License](LICENSE)

# TODO

Implementation tasks to make this package more production-ready while preserving
the current small API surface and simple O(1) LRU core.

## P0 - Correctness and API hardening

- [x] Replace string-based singleflight keys.
  - Previous `GetOrSetSingleflight` used `fmt.Sprintf("%v", key)`, which could
    coalesce distinct comparable keys with the same string representation.
  - Implement typed in-flight suppression keyed by `K`, or another collision-free
    strategy that does not require converting keys to strings.
  - Apply the fix to both `Cache` and `Expirable`.
  - Add tests with distinct key types/values that stringify identically.

- [x] Add `Resize(capacity int) (evicted int, err error)` to `Cache`.
  - Reject capacities less than or equal to zero.
  - Increasing capacity should not evict.
  - Decreasing capacity should evict from the tail until `Len() <= capacity`.
  - Invoke eviction callbacks after releasing the cache lock.
  - Add tests for grow, shrink, invalid size, callback ordering, and concurrency.

- [x] Add oldest-entry operations to `Cache`.
  - `GetOldest() (key K, value V, ok bool)` should inspect `tail` without
    affecting recency.
  - `RemoveOldest() (key K, value V, ok bool)` should remove `tail` and invoke
    callbacks after releasing the lock.
  - Add tests for empty cache, normal ordering, recency changes, and callbacks.

- [x] Add `Values() []V` to `Cache`.
  - Match `Keys()` ordering: most recently used to least recently used.
  - Document ordering explicitly.
  - Add tests that verify values stay aligned with key recency order.

## P1 - Expirable cache behavior

- [ ] Improve `Expirable` capacity eviction around expired entries.
  - Before evicting a live LRU entry for capacity, remove expired entries if any
    are present.
  - Prefer avoiding a full scan on every write if possible; if scanning is used,
    benchmark the impact and document the tradeoff.
  - Ensure callbacks for expired removals happen after releasing the lock.
  - Add tests where expired non-tail entries should be purged instead of evicting
    a live tail entry.

- [ ] Add `Resize(capacity int) (evicted int, err error)` to `Expirable`.
  - Keep semantics consistent with `Cache.Resize`.
  - Decide and document whether expired removals count as evictions in the return
    value.
  - Add tests for expired entries, live evictions, invalid size, and callbacks.

- [ ] Add `GetOldest`, `RemoveOldest`, and `Values` to `Expirable`.
  - `GetOldest` should return the oldest non-expired entry or report not found.
  - `RemoveOldest` should remove the oldest non-expired entry; decide whether it
    also purges expired entries encountered while searching.
  - `Values` should return non-expired values in the same order as `Keys`.

- [ ] Add optional automatic expiry cleanup.
  - Keep lazy expiry as the default to avoid surprise goroutines.
  - Consider `StartJanitor(interval time.Duration)`, `StopJanitor()`, or a
    constructor option such as `WithCleanupInterval`.
  - Define lifecycle behavior clearly: idempotent start/stop, no goroutine leaks,
    and no callbacks while holding the cache lock.
  - Add race-enabled tests for start/stop and concurrent cache operations.

## P2 - Constructors and configuration

- [ ] Add constructor options.
  - Keep existing constructors for compatibility.
  - Consider `NewWithOptions(capacity int, opts ...Option)`.
  - Candidate options: `WithOnEvict`, `WithCleanupInterval`,
    `WithShardCount`, and test-only clock injection.
  - Avoid option sprawl; only add options with clear production value.

- [ ] Revisit `OnEvict` configuration.
  - Keep `OnEvict` setter for compatibility.
  - Document whether replacing a callback is safe during concurrent operations.
  - Add tests for changing or clearing the callback while operations are running.

- [ ] Lower the Go version if possible.
  - The code appears compatible with Go generics and likely does not require
    `go 1.24.0`.
  - Verify with the lowest intended Go version before changing `go.mod`.
  - Update CI/test matrix if present.

## P3 - Sharded cache improvements

- [ ] Expand sharded cache documentation.
  - Clearly state that sharded mode is not a global LRU.
  - Explain that capacity and recency are enforced per shard.
  - Document callback concurrency requirements.

- [ ] Add `Resize` support for `Sharded`.
  - Decide how to redistribute capacity across shards.
  - Preserve total capacity and ensure each shard has at least one slot.
  - Add tests for capacity redistribution and callback behavior.

- [ ] Add `Stats` or `ShardStats`.
  - Track per-shard length/capacity, total length/capacity, hits, misses,
    evictions, and expired removals if stats are enabled.
  - Keep stats optional or cheap enough to always maintain.
  - Use atomics or per-shard accounting to avoid adding central contention.

## P4 - Observability and benchmarks

- [ ] Add basic cache stats.
  - Candidate counters: hits, misses, sets, updates, removes, evictions,
    expirations, singleflight shared calls.
  - Define whether `Peek` and `Contains` affect hit/miss counters.
  - Add tests for counter behavior.

- [ ] Add comparative benchmarks for new behavior.
  - Benchmark typed singleflight replacement versus current string keying.
  - Benchmark resize, oldest operations, expirable cleanup, and sharded stats.
  - Include high-contention and Zipf-like workloads.

- [ ] Add documentation examples for new APIs.
  - Examples should cover `Resize`, `GetOldest`/`RemoveOldest`,
    `GetOrSetSingleflight`, per-entry TTL, and optional cleanup.
  - Keep README short and link to package docs/examples for details.

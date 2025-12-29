# Generic LRU Cache for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/rselbach/lru.svg)](https://pkg.go.dev/github.com/rselbach/lru)

A thread-safe, generic LRU cache implementation in Go with optional TTL expiration and eviction callbacks.

## Features

- Generic implementation (Go 1.18+)
- O(1) lookups, insertions, and deletions
- Thread-safe for concurrent access
- Optional time-based expiration ([Expirable])
- Eviction callbacks

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
```

See the [package documentation](https://pkg.go.dev/github.com/rselbach/lru) for complete API reference and examples.

## License

[MIT License](LICENSE)

package lru

import (
	"context"
	"errors"
	"sync"
	"time"
)

// expiryMeta is the per-entry metadata stored in an Expirable cache node.
type expiryMeta struct {
	expiry time.Time
}

func expiryElapsed(now, expiry time.Time) bool {
	return now.After(expiry)
}

// ErrInvalidTTL is returned when a TTL override is zero or negative.
var ErrInvalidTTL = errors.New("lru: TTL must be greater than zero")

// ErrJanitorStopping is returned when StartJanitor is called while a previously
// signaled janitor is still exiting.
var ErrJanitorStopping = errors.New("lru: janitor is stopping")

// Expirable represents a thread-safe, fixed-size LRU cache with expiry functionality.
// Each entry has an absolute expiration time set when written via [Expirable.Set],
// [Expirable.GetOrSet], or [Expirable.GetOrSetSingleflight]. The TTL is not
// refreshed on reads (no sliding expiration). An entry is still returned at
// exactly its expiration instant and is treated as expired strictly after it.
// An Expirable must be created with [NewExpirable] or [MustNewExpirable]; the
// zero value is not ready for use. An Expirable must not be copied after first use.
type Expirable[K comparable, V any] struct {
	base[K, V, expiryMeta]
	ttl           time.Duration
	timeNow       func() time.Time // for testing
	nextExpiry    time.Time        // conservative earliest stored expiry
	hasNextExpiry bool

	janitorMu   sync.Mutex
	janitorStop chan struct{}
	janitorDone chan struct{}
}

// setOptions holds optional parameters for Set operations.
type setOptions struct {
	ttl    time.Duration
	ttlSet bool
}

// SetOption is a functional option for [Expirable.Set], [Expirable.GetOrSet],
// and [Expirable.GetOrSetSingleflight].
type SetOption func(*setOptions)

// WithTTL sets a custom TTL for the entry being set, overriding the cache's
// default TTL. The TTL must be greater than zero. [Expirable.Set] panics with
// [ErrInvalidTTL] for an invalid override; SetErr and GetOrSet methods return
// the error.
func WithTTL(ttl time.Duration) SetOption {
	return func(o *setOptions) {
		o.ttl = ttl
		o.ttlSet = true
	}
}

func resolveSetOptions(opts []SetOption) (setOptions, error) {
	opt := setOptions{}
	for _, apply := range opts {
		apply(&opt)
	}
	if opt.ttlSet && opt.ttl <= 0 {
		return setOptions{}, ErrInvalidTTL
	}
	return opt, nil
}

// resolveTTL returns the effective TTL for a set. Caller must hold c.mu.
func (c *Expirable[K, V]) resolveTTL(opt setOptions) time.Duration {
	if opt.ttlSet {
		return opt.ttl
	}
	return c.ttl
}

// NewExpirable creates a new LRU cache with the given capacity and TTL.
// Each entry expires a fixed duration after it is written via Set or GetOrSet.
// Reads (Get, Peek, GetWithTTL) do not extend an entry's TTL.
// The capacity must be greater than zero, and the TTL must be greater than zero.
func NewExpirable[K comparable, V any](capacity int, ttl time.Duration) (*Expirable[K, V], error) {
	if capacity <= 0 {
		return nil, ErrInvalidCapacity
	}
	if ttl <= 0 {
		return nil, ErrInvalidTTL
	}

	return &Expirable[K, V]{
		base:    newBase[K, V, expiryMeta](capacity),
		ttl:     ttl,
		timeNow: time.Now,
	}, nil
}

// MustNewExpirable creates a new LRU cache with the given capacity and TTL.
// It panics if the capacity or TTL is less than or equal to zero.
func MustNewExpirable[K comparable, V any](capacity int, ttl time.Duration) *Expirable[K, V] {
	cache, err := NewExpirable[K, V](capacity, ttl)
	if err != nil {
		panic(err)
	}
	return cache
}

// Get retrieves a value from the cache by key.
// It returns the value and a boolean indicating whether the key was found and not expired.
// This method also updates the item's position in the LRU list.
// Expired items are removed when accessed.
func (c *Expirable[K, V]) Get(key K) (V, bool) {
	var zero V
	if c.checkKey(key) != nil {
		return zero, false
	}

	c.mu.Lock()

	e, found := c.items[key]
	if !found {
		c.mu.Unlock()
		return zero, false
	}

	// check if the entry has expired
	if expiryElapsed(c.timeNow(), e.meta.expiry) {
		evictedKey := e.key
		evictedVal := e.val
		onEvict := c.onEvict
		onRemove := c.onRemove
		c.deleteEntry(e)
		c.noteRemovedExpiryLocked()
		c.mu.Unlock()

		if onEvict != nil || onRemove != nil {
			invokeCallbacks(onEvict, onRemove, evictedKey, evictedVal, RemovalReasonExpired)
		}
		return zero, false
	}

	c.moveToFront(e)
	val := e.val
	c.mu.Unlock()

	return val, true
}

// Peek retrieves a value from the cache by key without updating its position
// in the LRU list. This is useful for checking a value without affecting
// eviction order. Returns the value and a boolean indicating whether the key
// was found and not expired.
//
// Note: Unlike [Expirable.Get], expired items are not removed from the cache.
// Use [Expirable.RemoveExpired] to explicitly purge expired entries.
func (c *Expirable[K, V]) Peek(key K) (V, bool) {
	var zero V
	if c.checkKey(key) != nil {
		return zero, false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	e, found := c.items[key]
	if !found {
		return zero, false
	}

	if expiryElapsed(c.timeNow(), e.meta.expiry) {
		return zero, false
	}

	return e.val, true
}

// GetWithTTL retrieves a value and its remaining TTL from the cache by key.
// It returns the value, remaining TTL, and a boolean indicating whether the key was found and not expired.
// Expired items are removed when accessed.
func (c *Expirable[K, V]) GetWithTTL(key K) (V, time.Duration, bool) {
	var zero V
	if c.checkKey(key) != nil {
		return zero, 0, false
	}

	c.mu.Lock()

	e, found := c.items[key]
	if !found {
		c.mu.Unlock()
		return zero, 0, false
	}

	now := c.timeNow()
	// check if the entry has expired
	if expiryElapsed(now, e.meta.expiry) {
		evictedKey := e.key
		evictedVal := e.val
		onEvict := c.onEvict
		onRemove := c.onRemove
		c.deleteEntry(e)
		c.noteRemovedExpiryLocked()
		c.mu.Unlock()

		if onEvict != nil || onRemove != nil {
			invokeCallbacks(onEvict, onRemove, evictedKey, evictedVal, RemovalReasonExpired)
		}
		return zero, 0, false
	}

	c.moveToFront(e)

	// calculate remaining TTL
	ttl := e.meta.expiry.Sub(now)
	val := e.val
	c.mu.Unlock()

	return val, ttl, true
}

// GetOrSet retrieves a value from the cache by key, or computes and sets it if not present or expired.
// The compute function is only called if the key is not present in the cache or is expired.
// Note: if multiple goroutines call GetOrSet concurrently for the same missing/expired key,
// compute may be called multiple times but only one result will be cached.
// If another goroutine inserts the key while compute runs, that stored value is
// returned and the result of compute is discarded. compute must be safe to abandon
// (no unreclaimed side effects), or use [Expirable.GetOrSetSingleflight].
//
// Options can be passed to customize the entry, such as [WithTTL] to override
// the cache's default TTL for this specific entry.
func (c *Expirable[K, V]) GetOrSet(key K, compute func() (V, error), opts ...SetOption) (V, error) {
	if err := c.checkKey(key); err != nil {
		var zero V
		return zero, err
	}
	opt, err := resolveSetOptions(opts)
	if err != nil {
		var zero V
		return zero, err
	}

	// fast path: check if item exists and is not expired
	if val, found := c.Get(key); found {
		return val, nil
	}

	// compute the value outside the lock to avoid deadlock if compute
	// calls back into the cache
	val, err := compute()
	if err != nil {
		var zero V
		return zero, err
	}

	c.mu.Lock()
	// check again in case it was added while we were computing
	e, found := c.items[key]
	var expiredEntry *entry[K, V, expiryMeta]
	if found {
		if !expiryElapsed(c.timeNow(), e.meta.expiry) {
			c.moveToFront(e)
			val := e.val
			c.mu.Unlock()
			return val, nil
		}
		// expired entry, remove it and save for callback
		expiredEntry = e
		c.deleteEntry(e)
		c.noteRemovedExpiryLocked()
	}

	onEvict := c.onEvict
	onRemove := c.onRemove
	evicted := c.setLocked(key, val, c.resolveTTL(opt), onEvict != nil || onRemove != nil)
	c.mu.Unlock()

	if onEvict != nil || onRemove != nil {
		if expiredEntry != nil {
			invokeCallbacks(onEvict, onRemove, expiredEntry.key, expiredEntry.val, RemovalReasonExpired)
		}
		for _, e := range evicted {
			invokeCallbacks(onEvict, onRemove, e.key, e.val, e.reason)
		}
	}
	return val, nil
}

// GetOrSetSingleflight retrieves a value from the cache by key, or computes and sets it if not present or expired.
// Unlike [Expirable.GetOrSet], if multiple goroutines call GetOrSetSingleflight concurrently for the same
// missing/expired key, the compute function is called exactly once and all callers receive the same result.
// This is useful when the compute function is expensive (e.g., database queries, API calls).
//
// The singleflight deduplication only applies to concurrent in-flight calls; once a value is cached,
// subsequent calls return the cached value without invoking singleflight. compute must not call
// GetOrSetSingleflight recursively for the same key because it would wait on its own call.
//
// Options can be passed to customize the entry, such as [WithTTL] to override
// the cache's default TTL for this specific entry. Concurrent callers that share
// an in-flight key share the leader's result and the leader's effective TTL; a
// waiter's [WithTTL] option is not applied.
// A compute error is returned to all current callers and is not cached. A panic
// or runtime.Goexit from compute is propagated to all current callers.
func (c *Expirable[K, V]) GetOrSetSingleflight(key K, compute func() (V, error), opts ...SetOption) (V, error) {
	return c.getOrSetSingleflight(context.Background(), key, compute, opts)
}

// GetOrSetSingleflightContext behaves like [Expirable.GetOrSetSingleflight],
// with context cancellation for the computation and its waiters. The caller
// that starts the computation supplies the context passed to compute. A follower
// can stop waiting when its own context is canceled without canceling the shared
// computation, which may still cache its result. The method panics if ctx is nil.
func (c *Expirable[K, V]) GetOrSetSingleflightContext(
	ctx context.Context,
	key K,
	compute func(context.Context) (V, error),
	opts ...SetOption,
) (V, error) {
	if ctx == nil {
		panic("lru: nil Context")
	}
	return c.getOrSetSingleflight(ctx, key, func() (V, error) {
		return compute(ctx)
	}, opts)
}

func (c *Expirable[K, V]) getOrSetSingleflight(
	ctx context.Context,
	key K,
	compute func() (V, error),
	opts []SetOption,
) (V, error) {
	if err := c.checkKey(key); err != nil {
		var zero V
		return zero, err
	}
	opt, err := resolveSetOptions(opts)
	if err != nil {
		var zero V
		return zero, err
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			var zero V
			return zero, err
		}
	}

	// fast path: check if item exists and is not expired
	if val, found := c.Get(key); found {
		return val, nil
	}

	// use singleflight to deduplicate concurrent computes for the same typed key
	result, err := c.sfGroup.do(ctx, key, func() (V, error) {
		// check again inside singleflight in case another goroutine just cached it
		if val, found := c.Get(key); found {
			return val, nil
		}

		val, err := compute()
		if err != nil {
			var zero V
			return zero, err
		}

		c.mu.Lock()
		// check again in case it was added while we were computing
		e, found := c.items[key]
		var expiredEntry *entry[K, V, expiryMeta]
		if found {
			if !expiryElapsed(c.timeNow(), e.meta.expiry) {
				c.moveToFront(e)
				existingVal := e.val
				c.mu.Unlock()
				return existingVal, nil
			}
			// expired entry, remove it and save for callback
			expiredEntry = e
			c.deleteEntry(e)
			c.noteRemovedExpiryLocked()
		}

		onEvict := c.onEvict
		onRemove := c.onRemove
		evicted := c.setLocked(key, val, c.resolveTTL(opt), onEvict != nil || onRemove != nil)
		c.mu.Unlock()

		if onEvict != nil || onRemove != nil {
			if expiredEntry != nil {
				invokeCallbacks(onEvict, onRemove, expiredEntry.key, expiredEntry.val, RemovalReasonExpired)
			}
			for _, e := range evicted {
				invokeCallbacks(onEvict, onRemove, e.key, e.val, e.reason)
			}
		}
		return val, nil
	})

	if err != nil {
		var zero V
		return zero, err
	}
	return result, nil
}

// Set adds or updates an item in the cache.
// If the key already exists and is still live, its value is updated without
// invoking the eviction callback for the previous value. If that existing entry
// had already expired, the eviction callback is invoked for the replaced value.
// If the cache is at capacity, the least recently used item is evicted.
// If a new key would exceed capacity, expired entries are removed before
// evicting a non-expired least recently used entry. Otherwise expired items are
// removed lazily on access or via RemoveExpired. The capacity cleanup scans the
// cache only when physical storage is full and the earliest possible expiry
// has passed.
//
// Options can be passed to customize the entry, such as [WithTTL] to override
// the cache's default TTL for this specific entry. Set panics with
// [ErrInvalidKey] for an invalid key and [ErrInvalidTTL] for an invalid TTL. Use
// [Expirable.SetErr] to receive those errors instead.
func (c *Expirable[K, V]) Set(key K, value V, opts ...SetOption) {
	if err := c.SetErr(key, value, opts...); err != nil {
		panic(err)
	}
}

// SetErr adds or updates an item like [Expirable.Set], returning
// [ErrInvalidKey] or [ErrInvalidTTL] instead of panicking.
func (c *Expirable[K, V]) SetErr(key K, value V, opts ...SetOption) error {
	if err := c.checkKey(key); err != nil {
		return err
	}
	opt, err := resolveSetOptions(opts)
	if err != nil {
		return err
	}

	c.mu.Lock()
	onEvict := c.onEvict
	onRemove := c.onRemove
	evicted := c.setLocked(key, value, c.resolveTTL(opt), onEvict != nil || onRemove != nil)
	c.mu.Unlock()

	for _, e := range evicted {
		invokeCallbacks(onEvict, onRemove, e.key, e.val, e.reason)
	}
	return nil
}

// Resize changes the maximum capacity of the cache.
// The new capacity must be greater than zero. Expired entries are purged first
// and do not count toward the returned eviction count. If the cache is still
// over capacity after expiry cleanup, least recently used non-expired entries
// are evicted until the cache length is less than or equal to capacity.
func (c *Expirable[K, V]) Resize(capacity int) (int, error) {
	if capacity <= 0 {
		return 0, ErrInvalidCapacity
	}

	c.mu.Lock()
	onEvict := c.onEvict
	onRemove := c.onRemove
	evicted, liveEvicted := c.resizeExpirableLocked(
		capacity,
		c.timeNow(),
		onEvict != nil || onRemove != nil,
	)
	c.mu.Unlock()

	for _, e := range evicted {
		invokeCallbacks(onEvict, onRemove, e.key, e.val, e.reason)
	}

	return liveEvicted, nil
}

func (c *Expirable[K, V]) resizeExpirableLocked(
	capacity int,
	now time.Time,
	collect bool,
) ([]evictedItem[K, V], int) {
	liveCount := 0
	for e := c.tail; e != nil; e = e.prev {
		if !expiryElapsed(now, e.meta.expiry) {
			liveCount++
		}
	}

	liveToEvict := liveCount - capacity
	if liveToEvict < 0 {
		liveToEvict = 0
	}

	var evicted []evictedItem[K, V]
	var nextExpiry time.Time
	hasNextExpiry := false
	liveEvicted := 0
	for e := c.tail; e != nil; {
		prev := e.prev
		expired := expiryElapsed(now, e.meta.expiry)
		if expired || liveToEvict > 0 {
			reason := RemovalReasonExpired
			if !expired {
				reason = RemovalReasonResize
			}
			if collect {
				evicted = append(evicted, evictedItem[K, V]{key: e.key, val: e.val, reason: reason})
			}
			c.deleteEntry(e)
			if !expired {
				liveToEvict--
				liveEvicted++
			}
		} else if !hasNextExpiry || e.meta.expiry.Before(nextExpiry) {
			nextExpiry = e.meta.expiry
			hasNextExpiry = true
		}
		e = prev
	}

	c.capacity = capacity
	c.nextExpiry = nextExpiry
	c.hasNextExpiry = hasNextExpiry
	return evicted, liveEvicted
}

// setLocked is an internal method that adds or updates an item in the cache.
// it assumes the mutex is already locked.
// Returns entries removed due to expiry cleanup or capacity eviction.
func (c *Expirable[K, V]) setLocked(key K, value V, ttl time.Duration, collectEvicted bool) []evictedItem[K, V] {
	now := c.timeNow()
	expiry := now.Add(ttl)

	// if key exists, update value and expiry and move to front
	if e, found := c.items[key]; found {
		var evicted []evictedItem[K, V]
		// replacing an expired entry retires its dead value, so report it to
		// the eviction callback like any other expiry removal
		if collectEvicted && expiryElapsed(now, e.meta.expiry) {
			evicted = append(evicted, evictedItem[K, V]{key: e.key, val: e.val, reason: RemovalReasonExpired})
		}
		c.moveToFront(e)
		e.val = value
		e.meta.expiry = expiry
		// Extending an entry can leave the watermark earlier than the true
		// minimum, which is allowed: the next capacity write scans once and
		// recomputes it exactly.
		c.noteExpiryLocked(expiry)
		return evicted
	}

	var evicted []evictedItem[K, V]

	// If physical storage is full and an entry may have expired, purge expired
	// entries before evicting a live least recently used entry.
	if len(c.items) >= c.capacity && c.expiryDueLocked(now) {
		evicted = c.removeExpiredLocked(now, collectEvicted)
	}

	// If we're still at capacity, remove the least recently used item.
	if len(c.items) >= c.capacity {
		oldest := c.tail
		if oldest != nil {
			if collectEvicted {
				evicted = append(evicted, evictedItem[K, V]{key: oldest.key, val: oldest.val, reason: RemovalReasonCapacity})
			}
			c.deleteEntry(oldest)
			c.noteRemovedExpiryLocked()
		}
	}

	// add new item
	e := &entry[K, V, expiryMeta]{
		key:  key,
		val:  value,
		meta: expiryMeta{expiry: expiry},
	}
	c.pushFront(e)
	c.items[key] = e
	c.noteExpiryLocked(expiry)
	return evicted
}

func (c *Expirable[K, V]) noteExpiryLocked(expiry time.Time) {
	if !c.hasNextExpiry || expiry.Before(c.nextExpiry) {
		c.nextExpiry = expiry
		c.hasNextExpiry = true
	}
}

// noteRemovedExpiryLocked updates the earliest-expiry watermark after an entry
// is removed. Caller must hold c.mu.
//
// Removing an entry can only raise the true earliest expiry, so the watermark
// stays conservative without any work. Leaving it early costs at most one
// expiry scan on a later capacity write, and that scan restores an exact
// watermark; rescanning here would make every removal O(n).
func (c *Expirable[K, V]) noteRemovedExpiryLocked() {
	if len(c.items) == 0 {
		c.nextExpiry = time.Time{}
		c.hasNextExpiry = false
	}
}

func (c *Expirable[K, V]) expiryDueLocked(now time.Time) bool {
	return c.hasNextExpiry && expiryElapsed(now, c.nextExpiry)
}

func (c *Expirable[K, V]) removeExpiredLocked(now time.Time, collect bool) []evictedItem[K, V] {
	var expired []evictedItem[K, V]
	var nextExpiry time.Time
	hasNextExpiry := false
	for e := c.head; e != nil; {
		next := e.next
		if expiryElapsed(now, e.meta.expiry) {
			if collect {
				expired = append(expired, evictedItem[K, V]{key: e.key, val: e.val, reason: RemovalReasonExpired})
			}
			c.deleteEntry(e)
		} else if !hasNextExpiry || e.meta.expiry.Before(nextExpiry) {
			nextExpiry = e.meta.expiry
			hasNextExpiry = true
		}
		e = next
	}
	c.nextExpiry = nextExpiry
	c.hasNextExpiry = hasNextExpiry
	return expired
}

// Remove deletes an item from the cache by key.
// It returns whether the key was found and removed.
func (c *Expirable[K, V]) Remove(key K) bool {
	if c.checkKey(key) != nil {
		return false
	}

	c.mu.Lock()
	e, found := c.items[key]
	if !found {
		c.mu.Unlock()
		return false
	}

	evictedKey := e.key
	evictedVal := e.val
	onEvict := c.onEvict
	onRemove := c.onRemove

	c.deleteEntry(e)
	c.noteRemovedExpiryLocked()
	c.mu.Unlock()

	if onEvict != nil || onRemove != nil {
		invokeCallbacks(onEvict, onRemove, evictedKey, evictedVal, RemovalReasonExplicit)
	}
	return true
}

// GetOldest returns the least recently used non-expired entry without updating recency.
func (c *Expirable[K, V]) GetOldest() (K, V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var zeroKey K
	var zeroVal V
	now := c.timeNow()

	for e := c.tail; e != nil; e = e.prev {
		if !expiryElapsed(now, e.meta.expiry) {
			return e.key, e.val, true
		}
	}

	return zeroKey, zeroVal, false
}

// RemoveOldest removes and returns the least recently used non-expired entry.
// Expired entries encountered while searching from the tail are also removed.
func (c *Expirable[K, V]) RemoveOldest() (K, V, bool) {
	c.mu.Lock()

	var zeroKey K
	var zeroVal V
	var key K
	var val V
	found := false
	now := c.timeNow()
	onEvict := c.onEvict
	onRemove := c.onRemove
	var evicted []evictedItem[K, V]

	for e := c.tail; e != nil; {
		prev := e.prev
		if expiryElapsed(now, e.meta.expiry) {
			if onEvict != nil || onRemove != nil {
				evicted = append(evicted, evictedItem[K, V]{key: e.key, val: e.val, reason: RemovalReasonExpired})
			}
			c.deleteEntry(e)
			c.noteRemovedExpiryLocked()
			e = prev
			continue
		}

		key = e.key
		val = e.val
		found = true
		if onEvict != nil || onRemove != nil {
			evicted = append(evicted, evictedItem[K, V]{key: e.key, val: e.val, reason: RemovalReasonExplicit})
		}
		c.deleteEntry(e)
		c.noteRemovedExpiryLocked()
		break
	}

	c.mu.Unlock()

	for _, e := range evicted {
		invokeCallbacks(onEvict, onRemove, e.key, e.val, e.reason)
	}

	if !found {
		return zeroKey, zeroVal, false
	}
	return key, val, true
}

// Len returns the current number of non-expired items in the cache.
// It is O(n) in the number of stored entries because expiry is evaluated against
// the current time without purging. Expired entries still occupy capacity until
// removed; use [Expirable.PhysicalLen] for the stored entry count and
// [Expirable.RemoveExpired] to purge them.
func (c *Expirable[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.liveLenLocked(c.timeNow())
}

func (c *Expirable[K, V]) liveLenLocked(now time.Time) int {
	count := 0
	for _, e := range c.items {
		if !expiryElapsed(now, e.meta.expiry) {
			count++
		}
	}
	return count
}

// PhysicalLen returns the number of entries stored in the cache, including
// expired entries that have not yet been purged. It is O(1).
func (c *Expirable[K, V]) PhysicalLen() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// Clear removes all items from the cache.
//
// If an eviction callback is set, it is called for every stored entry in order
// from least recently used to most recently used, including entries that have
// already expired but have not yet been purged. The entries are buffered while
// the lock is held so the callbacks can run without it, so clearing a large
// cache with a callback set allocates one key/value pair per entry.
func (c *Expirable[K, V]) Clear() {
	c.mu.Lock()
	onEvict := c.onEvict
	onRemove := c.onRemove

	var evicted []evictedItem[K, V]
	if onEvict != nil || onRemove != nil {
		evicted = make([]evictedItem[K, V], 0, len(c.items))
		for e := c.tail; e != nil; e = e.prev {
			evicted = append(evicted, evictedItem[K, V]{key: e.key, val: e.val, reason: RemovalReasonClear})
		}
	}

	c.resetLocked()
	c.nextExpiry = time.Time{}
	c.hasNextExpiry = false
	c.mu.Unlock()

	for _, e := range evicted {
		invokeCallbacks(onEvict, onRemove, e.key, e.val, e.reason)
	}
}

// Contains checks if a key exists in the cache and is not expired.
//
// Note: This method does not remove expired entries from the cache.
// Use [Expirable.RemoveExpired] to explicitly purge expired entries.
func (c *Expirable[K, V]) Contains(key K) bool {
	if c.checkKey(key) != nil {
		return false
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	e, found := c.items[key]
	if !found {
		return false
	}

	return !expiryElapsed(c.timeNow(), e.meta.expiry)
}

// Keys returns a slice of all keys in the cache that haven't expired.
// The order is from most recently used to least recently used.
func (c *Expirable[K, V]) Keys() []K {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := c.timeNow()
	keys := make([]K, 0, c.liveLenLocked(now))

	for e := c.head; e != nil; e = e.next {
		if !expiryElapsed(now, e.meta.expiry) {
			keys = append(keys, e.key)
		}
	}

	return keys
}

// Values returns a slice of all values in the cache that haven't expired.
// The order matches [Expirable.Keys]: most recently used to least recently used.
//
// Keys and Values are separate snapshots taken under separate lock holds, so
// their elements line up only when no other goroutine writes to the cache
// between the two calls, and only when no entry expires between them.
func (c *Expirable[K, V]) Values() []V {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := c.timeNow()
	values := make([]V, 0, c.liveLenLocked(now))

	for e := c.head; e != nil; e = e.next {
		if !expiryElapsed(now, e.meta.expiry) {
			values = append(values, e.val)
		}
	}

	return values
}

// Items returns key/value pairs for all non-expired entries, in order from most
// recently used to least recently used. Unlike separate calls to
// [Expirable.Keys] and [Expirable.Values], each key and value in the result is
// captured under the same lock hold and evaluated against the same instant.
// Expired entries are filtered without being removed.
func (c *Expirable[K, V]) Items() []Item[K, V] {
	c.mu.RLock()
	defer c.mu.RUnlock()

	now := c.timeNow()
	items := make([]Item[K, V], 0, c.liveLenLocked(now))
	for e := c.head; e != nil; e = e.next {
		if !expiryElapsed(now, e.meta.expiry) {
			items = append(items, Item[K, V]{Key: e.key, Value: e.val})
		}
	}
	return items
}

// TTL returns the time-to-live duration for cache entries.
func (c *Expirable[K, V]) TTL() time.Duration {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ttl
}

// SetTTL updates the TTL for future cache entries.
// It does not affect existing entries.
func (c *Expirable[K, V]) SetTTL(ttl time.Duration) error {
	if ttl <= 0 {
		return ErrInvalidTTL
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.ttl = ttl
	return nil
}

// StartJanitor starts a background goroutine that periodically removes expired entries.
// Lazy expiration remains the default; the janitor only runs after this method is called.
//
// The interval must be greater than zero. Calling StartJanitor while the janitor
// is already running is a no-op. If a previous janitor was signaled to stop but
// has not exited yet, StartJanitor returns [ErrJanitorStopping].
//
// The janitor goroutine runs until [Expirable.StopJanitor] or
// [Expirable.SignalStopJanitor] is called; abandoning the cache without
// stopping the janitor leaks the goroutine.
func (c *Expirable[K, V]) StartJanitor(interval time.Duration) error {
	if interval <= 0 {
		return ErrInvalidJanitorInterval
	}

	c.janitorMu.Lock()
	defer c.janitorMu.Unlock()

	if c.janitorStop != nil {
		return nil
	}

	if c.janitorDone != nil {
		select {
		case <-c.janitorDone:
			c.janitorDone = nil
		default:
			return ErrJanitorStopping
		}
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	c.janitorStop = stop
	c.janitorDone = done

	go c.runJanitor(interval, stop, done)
	return nil
}

func (c *Expirable[K, V]) runJanitor(interval time.Duration, stop <-chan struct{}, done chan<- struct{}) {
	ticker := time.NewTicker(interval)
	defer func() {
		ticker.Stop()
		close(done)
	}()

	for {
		select {
		case <-ticker.C:
			c.RemoveExpired()
		case <-stop:
			return
		}
	}
}

// StopJanitor stops the background expiry cleanup goroutine if it is running.
// Calling StopJanitor when the janitor is not running is a no-op. StopJanitor
// waits for the goroutine to exit before returning.
//
// Do not call StopJanitor from an eviction callback fired by the janitor
// itself: StopJanitor waits for the janitor goroutine, which is blocked
// invoking the callback, so the call would deadlock. Use
// [Expirable.SignalStopJanitor] from such callbacks instead.
func (c *Expirable[K, V]) StopJanitor() {
	c.janitorMu.Lock()
	c.signalStopJanitorLocked()
	done := c.janitorDone
	c.janitorMu.Unlock()

	if done == nil {
		return
	}
	<-done

	c.janitorMu.Lock()
	if c.janitorDone == done {
		c.janitorDone = nil
	}
	c.janitorMu.Unlock()
}

// SignalStopJanitor requests that the background expiry cleanup goroutine stop
// if it is running. Unlike [Expirable.StopJanitor], it does not wait for the
// goroutine to exit, so it is safe to call from an eviction callback invoked by
// the janitor. Calling SignalStopJanitor when the janitor is not running is a
// no-op.
//
// [Expirable.StopJanitor] may still be used afterward to wait for exit.
// [Expirable.StartJanitor] returns [ErrJanitorStopping] until a previously
// signaled janitor has exited.
func (c *Expirable[K, V]) SignalStopJanitor() {
	c.janitorMu.Lock()
	defer c.janitorMu.Unlock()
	c.signalStopJanitorLocked()
}

// signalStopJanitorLocked closes the stop channel if the janitor is running.
// Caller must hold janitorMu. janitorDone remains set until a waiter observes exit.
func (c *Expirable[K, V]) signalStopJanitorLocked() {
	if c.janitorStop == nil {
		return
	}
	close(c.janitorStop)
	c.janitorStop = nil
}

// OnEvict sets a callback function that will be called when an entry is evicted from the cache.
// The callback will receive the key and value of the evicted entry.
// This includes both manual removals and automatic evictions due to capacity or expiry.
//
// Calling OnEvict again replaces the callback used for future removals. Passing
// nil clears the callback. If an eviction is already in progress, it may still
// invoke the callback that was current when that eviction released the cache lock.
//
// The callback is invoked synchronously after the cache's internal lock is
// released and before the removing method returns. It may be called concurrently
// from multiple goroutines and must be safe for concurrent use.
func (c *Expirable[K, V]) OnEvict(f OnEvictFunc[K, V]) {
	c.base.OnEvict(f)
}

// OnRemove sets the reason-bearing removal callback. Passing nil clears it.
func (c *Expirable[K, V]) OnRemove(f OnRemoveFunc[K, V]) { c.base.OnRemove(f) }

// SetTimeNowFunc replaces the function used to get the current time.
// This is primarily useful for testing. The function may be called concurrently
// while a cache lock is held, so it must be concurrency-safe and must not call
// methods on this cache. Passing nil resets to time.Now.
func (c *Expirable[K, V]) SetTimeNowFunc(f func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if f == nil {
		f = time.Now
	}
	c.timeNow = f
}

// RemoveExpired explicitly removes all expired items from the cache.
// Returns the number of items removed.
// This method will call the eviction callback for each expired item if one is set.
func (c *Expirable[K, V]) RemoveExpired() int {
	c.mu.Lock()

	onEvict := c.onEvict
	onRemove := c.onRemove
	before := len(c.items)
	expired := c.removeExpiredLocked(c.timeNow(), onEvict != nil || onRemove != nil)
	removed := before - len(c.items)

	c.mu.Unlock()

	for _, e := range expired {
		invokeCallbacks(onEvict, onRemove, e.key, e.val, e.reason)
	}

	return removed
}

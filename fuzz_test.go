package lru

import (
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type cacheModel struct {
	capacity int
	values   map[int]int
	order    []int
}

func newCacheModel(capacity int) *cacheModel {
	return &cacheModel{
		capacity: capacity,
		values:   make(map[int]int),
		order:    make([]int, 0, capacity),
	}
}

func (m *cacheModel) touch(key int) {
	for i, candidate := range m.order {
		if candidate == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.order = append([]int{key}, m.order...)
}

func (m *cacheModel) set(key, value int) {
	if _, found := m.values[key]; !found && len(m.values) >= m.capacity {
		oldest := m.order[len(m.order)-1]
		delete(m.values, oldest)
		m.order = m.order[:len(m.order)-1]
	}
	m.values[key] = value
	m.touch(key)
}

func (m *cacheModel) get(key int) (int, bool) {
	value, found := m.values[key]
	if found {
		m.touch(key)
	}
	return value, found
}

func (m *cacheModel) remove(key int) bool {
	if _, found := m.values[key]; !found {
		return false
	}
	delete(m.values, key)
	for i, candidate := range m.order {
		if candidate == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	return true
}

func (m *cacheModel) resize(capacity int) {
	for len(m.values) > capacity {
		oldest := m.order[len(m.order)-1]
		delete(m.values, oldest)
		m.order = m.order[:len(m.order)-1]
	}
	m.capacity = capacity
}

func (m *cacheModel) clear() {
	m.values = make(map[int]int)
	m.order = m.order[:0]
}

func assertCacheMatchesModel(t *testing.T, cache *Cache[int, int], model *cacheModel) {
	t.Helper()
	if cache.Len() != len(model.values) {
		t.Fatalf("length: got %d, want %d", cache.Len(), len(model.values))
	}
	if keys := cache.Keys(); !reflect.DeepEqual(keys, model.order) {
		t.Fatalf("keys: got %v, want %v", keys, model.order)
	}

	values := cache.Values()
	if len(values) != len(model.order) {
		t.Fatalf("values length: got %d, want %d", len(values), len(model.order))
	}
	for i, key := range model.order {
		if values[i] != model.values[key] {
			t.Fatalf("value at %d: got %d, want %d", i, values[i], model.values[key])
		}
	}

	if len(cache.items) > cache.capacity {
		t.Fatalf("physical length %d exceeds capacity %d", len(cache.items), cache.capacity)
	}
	count := 0
	var previous *entry[int, int, struct{}]
	for current := cache.head; current != nil; current = current.next {
		count++
		if count > cache.capacity {
			t.Fatal("list contains a cycle")
		}
		if current.prev != previous {
			t.Fatal("broken previous link")
		}
		if cache.items[current.key] != current {
			t.Fatal("map and list entry disagree")
		}
		previous = current
	}
	if previous != cache.tail {
		t.Fatal("tail does not match final list entry")
	}
	if count != len(cache.items) {
		t.Fatalf("list length %d differs from map length %d", count, len(cache.items))
	}
}

func FuzzCacheOperations(f *testing.F) {
	f.Add([]byte{3, 1, 2, 3, 4, 5})
	f.Add([]byte{1, 255, 0, 127, 64, 32, 16})

	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) == 0 {
			return
		}
		if len(operations) > 1024 {
			operations = operations[:1024]
		}

		capacity := int(operations[0]%16) + 1
		cache := MustNew[int, int](capacity)
		model := newCacheModel(capacity)

		for i, operation := range operations[1:] {
			key := int(operation & 31)
			switch (operation >> 5) & 7 {
			case 0:
				cache.Set(key, i)
				model.set(key, i)
			case 1:
				gotValue, gotFound := cache.Get(key)
				wantValue, wantFound := model.get(key)
				if gotValue != wantValue || gotFound != wantFound {
					t.Fatalf("Get(%d): got (%d, %t), want (%d, %t)", key, gotValue, gotFound, wantValue, wantFound)
				}
			case 2:
				gotValue, gotFound := cache.Peek(key)
				wantValue, wantFound := model.values[key]
				if gotValue != wantValue || gotFound != wantFound {
					t.Fatalf("Peek(%d): got (%d, %t), want (%d, %t)", key, gotValue, gotFound, wantValue, wantFound)
				}
			case 3:
				if got, want := cache.Remove(key), model.remove(key); got != want {
					t.Fatalf("Remove(%d): got %t, want %t", key, got, want)
				}
			case 4:
				newCapacity := int(operation&15) + 1
				if _, err := cache.Resize(newCapacity); err != nil {
					t.Fatal(err)
				}
				model.resize(newCapacity)
			case 5:
				cache.Clear()
				model.clear()
			case 6:
				_, want := model.values[key]
				if got := cache.Contains(key); got != want {
					t.Fatalf("Contains(%d): got %t, want %t", key, got, want)
				}
			case 7:
				cache.Set(key, -i)
				model.set(key, -i)
			}
			assertCacheMatchesModel(t, cache, model)
		}
	})
}

type policyFuzzCache interface {
	Set(int, int)
	Get(int) (int, bool)
	GetOrSet(int, func() (int, error)) (int, error)
	Peek(int) (int, bool)
	Contains(int) bool
	Remove(int) bool
	Resize(int) (int, error)
	Clear()
	Len() int
	Capacity() int
	Items() []Item[int, int]
	OnRemove(OnRemoveFunc[int, int])
}

type fuzzRemoval struct {
	key    int
	value  int
	reason RemovalReason
}

func applyFuzzRemovals(t *testing.T, model map[int]int, removals []fuzzRemoval, allowed ...RemovalReason) {
	t.Helper()
	for _, removal := range removals {
		reasonAllowed := false
		for _, reason := range allowed {
			if removal.reason == reason {
				reasonAllowed = true
				break
			}
		}
		if !reasonAllowed {
			t.Fatalf("callback for key %d has unexpected reason %v", removal.key, removal.reason)
		}
		value, found := model[removal.key]
		if !found {
			t.Fatalf("callback reported unknown or duplicate key %d", removal.key)
		}
		if value != removal.value {
			t.Fatalf("callback value for key %d: got %d, want %d", removal.key, removal.value, value)
		}
		delete(model, removal.key)
	}
}

func assertPolicyCacheMatchesModel(t *testing.T, cache policyFuzzCache, model map[int]int) {
	t.Helper()
	if got := cache.Len(); got != len(model) {
		t.Fatalf("Len: got %d, want %d", got, len(model))
	}
	if cache.Len() > cache.Capacity() {
		t.Fatalf("length %d exceeds capacity %d", cache.Len(), cache.Capacity())
	}

	items := cache.Items()
	if len(items) != len(model) {
		t.Fatalf("Items length: got %d, want %d", len(items), len(model))
	}
	seen := make(map[int]bool, len(items))
	for _, item := range items {
		if seen[item.Key] {
			t.Fatalf("Items contains duplicate key %d", item.Key)
		}
		seen[item.Key] = true
		value, found := model[item.Key]
		if !found {
			t.Fatalf("Items contains unexpected key %d", item.Key)
		}
		if value != item.Value {
			t.Fatalf("Items value for key %d: got %d, want %d", item.Key, item.Value, value)
		}
	}
}

func fuzzPolicyOperations(
	t *testing.T,
	operations []byte,
	cache policyFuzzCache,
	shardCount int,
	insertReasons []RemovalReason,
	assertInvariants func(*testing.T),
) {
	t.Helper()
	model := make(map[int]int)
	var removals []fuzzRemoval
	cache.OnRemove(func(key, value int, reason RemovalReason) {
		// Reentry also verifies that callbacks never run under a shard lock.
		_ = cache.Len()
		removals = append(removals, fuzzRemoval{key: key, value: value, reason: reason})
	})

	for i, operation := range operations[1:] {
		removals = removals[:0]
		key := int(operation & 31)
		switch (operation >> 5) & 7 {
		case 0:
			value := i + 1
			cache.Set(key, value)
			model[key] = value
			if len(removals) > 1 {
				t.Fatalf("Set removed %d entries", len(removals))
			}
			applyFuzzRemovals(t, model, removals, insertReasons...)
		case 1:
			gotValue, gotFound := cache.Get(key)
			wantValue, wantFound := model[key]
			if gotValue != wantValue || gotFound != wantFound {
				t.Fatalf("Get(%d): got (%d, %t), want (%d, %t)", key, gotValue, gotFound, wantValue, wantFound)
			}
			applyFuzzRemovals(t, model, removals)
		case 2:
			gotValue, gotFound := cache.Peek(key)
			wantValue, wantFound := model[key]
			if gotValue != wantValue || gotFound != wantFound {
				t.Fatalf("Peek(%d): got (%d, %t), want (%d, %t)", key, gotValue, gotFound, wantValue, wantFound)
			}
			applyFuzzRemovals(t, model, removals)
		case 3:
			_, wantRemoved := model[key]
			if gotRemoved := cache.Remove(key); gotRemoved != wantRemoved {
				t.Fatalf("Remove(%d): got %t, want %t", key, gotRemoved, wantRemoved)
			}
			if len(removals) != boolInt(wantRemoved) {
				t.Fatalf("Remove(%d) produced %d callbacks", key, len(removals))
			}
			applyFuzzRemovals(t, model, removals, RemovalReasonExplicit)
		case 4:
			newCapacity := shardCount + int(operation&15)
			evicted, err := cache.Resize(newCapacity)
			if err != nil {
				t.Fatal(err)
			}
			if evicted != len(removals) {
				t.Fatalf("Resize returned %d evictions for %d callbacks", evicted, len(removals))
			}
			applyFuzzRemovals(t, model, removals, RemovalReasonResize)
			if got := cache.Capacity(); got != newCapacity {
				t.Fatalf("Capacity after Resize: got %d, want %d", got, newCapacity)
			}
		case 5:
			before := len(model)
			cache.Clear()
			if len(removals) != before {
				t.Fatalf("Clear produced %d callbacks for %d entries", len(removals), before)
			}
			applyFuzzRemovals(t, model, removals, RemovalReasonClear)
		case 6:
			_, want := model[key]
			if got := cache.Contains(key); got != want {
				t.Fatalf("Contains(%d): got %t, want %t", key, got, want)
			}
			applyFuzzRemovals(t, model, removals)
		case 7:
			wantValue, found := model[key]
			computeCalls := 0
			computedValue := -(i + 1)
			gotValue, err := cache.GetOrSet(key, func() (int, error) {
				computeCalls++
				return computedValue, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if found {
				if computeCalls != 0 || gotValue != wantValue {
					t.Fatalf("GetOrSet(%d) hit: got value %d and %d computes, want %d and 0", key, gotValue, computeCalls, wantValue)
				}
			} else {
				if computeCalls != 1 || gotValue != computedValue {
					t.Fatalf("GetOrSet(%d) miss: got value %d and %d computes, want %d and 1", key, gotValue, computeCalls, computedValue)
				}
				model[key] = computedValue
			}
			if len(removals) > 1 {
				t.Fatalf("GetOrSet removed %d entries", len(removals))
			}
			applyFuzzRemovals(t, model, removals, insertReasons...)
		}

		assertPolicyCacheMatchesModel(t, cache, model)
		assertInvariants(t)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func assertClockInvariants(t *testing.T, cache *Clock[int, int]) {
	t.Helper()
	totalCapacity := 0
	for shardIndex, shard := range cache.shards {
		totalCapacity += shard.capacity
		if len(shard.items) > shard.capacity {
			t.Fatalf("shard %d: %d items exceed capacity %d", shardIndex, len(shard.items), shard.capacity)
		}
		if len(shard.ring) > shard.capacity {
			t.Fatalf("shard %d: ring length %d exceeds capacity %d", shardIndex, len(shard.ring), shard.capacity)
		}
		if len(shard.ring) == 0 {
			if shard.hand != 0 {
				t.Fatalf("shard %d: empty ring has hand %d", shardIndex, shard.hand)
			}
		} else if shard.hand < 0 || shard.hand >= len(shard.ring) {
			t.Fatalf("shard %d: hand %d is outside ring length %d", shardIndex, shard.hand, len(shard.ring))
		}

		free := make(map[int]bool, len(shard.free))
		for _, index := range shard.free {
			if index < 0 || index >= len(shard.ring) {
				t.Fatalf("shard %d: free index %d is outside ring", shardIndex, index)
			}
			if free[index] {
				t.Fatalf("shard %d: duplicate free index %d", shardIndex, index)
			}
			free[index] = true
			if shard.ring[index] != nil {
				t.Fatalf("shard %d: free index %d contains an entry", shardIndex, index)
			}
		}

		ringEntries := 0
		for index, entry := range shard.ring {
			if entry == nil {
				if !free[index] {
					t.Fatalf("shard %d: nil ring index %d is not free", shardIndex, index)
				}
				continue
			}
			ringEntries++
			if free[index] {
				t.Fatalf("shard %d: live ring index %d is marked free", shardIndex, index)
			}
			if entry.idx != index {
				t.Fatalf("shard %d: entry %d records index %d", shardIndex, index, entry.idx)
			}
			if shard.items[entry.key] != entry {
				t.Fatalf("shard %d: map and ring disagree for key %d", shardIndex, entry.key)
			}
			if ref := atomic.LoadInt32(&entry.ref); ref != 0 && ref != 1 {
				t.Fatalf("shard %d: key %d has invalid reference bit %d", shardIndex, entry.key, ref)
			}
		}
		if ringEntries != len(shard.items) {
			t.Fatalf("shard %d: ring has %d entries, map has %d", shardIndex, ringEntries, len(shard.items))
		}
		if ringEntries+len(shard.free) != len(shard.ring) {
			t.Fatalf("shard %d: entries and free indexes do not cover ring", shardIndex)
		}
		for key, entry := range shard.items {
			if entry.idx < 0 || entry.idx >= len(shard.ring) || shard.ring[entry.idx] != entry {
				t.Fatalf("shard %d: map entry %d has invalid ring index %d", shardIndex, key, entry.idx)
			}
		}
	}
	if totalCapacity != cache.Capacity() {
		t.Fatalf("shard capacities total %d, cache capacity is %d", totalCapacity, cache.Capacity())
	}
}

func FuzzClockOperations(f *testing.F) {
	f.Add([]byte{8, 0, 32, 64, 96, 128, 160, 192, 224, 255})
	f.Add([]byte{3, 224, 193, 162, 131, 100, 69, 38, 7})

	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) == 0 {
			return
		}
		if len(operations) > 1024 {
			operations = operations[:1024]
		}
		shardCount := int(operations[0]%4) + 1
		capacity := shardCount + int(operations[0]>>2)%13
		cache := MustNewClockWithCount[int, int](capacity, shardCount)
		fuzzPolicyOperations(t, operations, cache, shardCount,
			[]RemovalReason{RemovalReasonCapacity},
			func(t *testing.T) { assertClockInvariants(t, cache) })
	})
}

func FuzzTinyLFUOperations(f *testing.F) {
	f.Add([]byte{8, 0, 32, 64, 96, 128, 160, 192, 224, 255})
	f.Add([]byte{3, 224, 193, 162, 131, 100, 69, 38, 7})

	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) == 0 {
			return
		}
		if len(operations) > 1024 {
			operations = operations[:1024]
		}
		shardCount := int(operations[0]%4) + 1
		capacity := shardCount + int(operations[0]>>2)%13
		cache := MustNewTinyLFUWithCount[int, int](capacity, shardCount)
		fuzzPolicyOperations(t, operations, cache, shardCount,
			[]RemovalReason{RemovalReasonCapacity, RemovalReasonAdmission},
			func(t *testing.T) { requireTinyLFUInvariants(t, cache) })
	})
}

type expirableEntry struct {
	val    int
	expiry time.Time
}

type expirableModel struct {
	capacity int
	ttl      time.Duration
	now      time.Time
	values   map[int]expirableEntry
	order    []int // MRU -> LRU
}

func newExpirableModel(capacity int, ttl time.Duration, now time.Time) *expirableModel {
	return &expirableModel{
		capacity: capacity,
		ttl:      ttl,
		now:      now,
		values:   make(map[int]expirableEntry),
		order:    make([]int, 0, capacity),
	}
}

func (m *expirableModel) expired(expiry time.Time) bool {
	return expiryElapsed(m.now, expiry)
}

func (m *expirableModel) touch(key int) {
	for i, candidate := range m.order {
		if candidate == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.order = append([]int{key}, m.order...)
}

func (m *expirableModel) deleteKey(key int) {
	delete(m.values, key)
	for i, candidate := range m.order {
		if candidate == key {
			m.order = append(m.order[:i], m.order[i+1:]...)
			return
		}
	}
}

func (m *expirableModel) removeExpired() int {
	removed := 0
	for _, key := range append([]int(nil), m.order...) {
		if m.expired(m.values[key].expiry) {
			m.deleteKey(key)
			removed++
		}
	}
	return removed
}

func (m *expirableModel) set(key, value int, ttl time.Duration) {
	expiry := m.now.Add(ttl)
	if _, found := m.values[key]; found {
		m.values[key] = expirableEntry{val: value, expiry: expiry}
		m.touch(key)
		return
	}

	if len(m.values) >= m.capacity {
		m.removeExpired()
	}
	if len(m.values) >= m.capacity {
		oldest := m.order[len(m.order)-1]
		m.deleteKey(oldest)
	}
	m.values[key] = expirableEntry{val: value, expiry: expiry}
	m.touch(key)
}

func (m *expirableModel) get(key int) (int, bool) {
	entry, found := m.values[key]
	if !found {
		return 0, false
	}
	if m.expired(entry.expiry) {
		m.deleteKey(key)
		return 0, false
	}
	m.touch(key)
	return entry.val, true
}

func (m *expirableModel) peek(key int) (int, bool) {
	entry, found := m.values[key]
	if !found || m.expired(entry.expiry) {
		return 0, false
	}
	return entry.val, true
}

func (m *expirableModel) remove(key int) bool {
	if _, found := m.values[key]; !found {
		return false
	}
	m.deleteKey(key)
	return true
}

func (m *expirableModel) resize(capacity int) {
	live := 0
	for _, entry := range m.values {
		if !m.expired(entry.expiry) {
			live++
		}
	}
	liveToEvict := live - capacity
	if liveToEvict < 0 {
		liveToEvict = 0
	}
	for i := len(m.order) - 1; i >= 0; i-- {
		key := m.order[i]
		entry := m.values[key]
		if m.expired(entry.expiry) {
			m.deleteKey(key)
			continue
		}
		if liveToEvict > 0 {
			m.deleteKey(key)
			liveToEvict--
		}
	}
	m.capacity = capacity
}

func (m *expirableModel) clear() {
	m.values = make(map[int]expirableEntry)
	m.order = m.order[:0]
}

func (m *expirableModel) liveOrder() []int {
	live := make([]int, 0, len(m.order))
	for _, key := range m.order {
		if !m.expired(m.values[key].expiry) {
			live = append(live, key)
		}
	}
	return live
}

func assertExpirableMatchesModel(t *testing.T, cache *Expirable[int, int], model *expirableModel) {
	t.Helper()

	live := model.liveOrder()
	if got := cache.Len(); got != len(live) {
		t.Fatalf("Len: got %d, want %d", got, len(live))
	}
	if got := cache.PhysicalLen(); got != len(model.values) {
		t.Fatalf("PhysicalLen: got %d, want %d", got, len(model.values))
	}
	if keys := cache.Keys(); !reflect.DeepEqual(keys, live) {
		t.Fatalf("Keys: got %v, want %v", keys, live)
	}

	if len(cache.items) > cache.capacity {
		t.Fatalf("physical length %d exceeds capacity %d", len(cache.items), cache.capacity)
	}

	count := 0
	var previous *entry[int, int, expiryMeta]
	var minExpiry time.Time
	hasMin := false
	for current := cache.head; current != nil; current = current.next {
		count++
		if count > len(cache.items)+1 {
			t.Fatal("list contains a cycle")
		}
		if current.prev != previous {
			t.Fatal("broken previous link")
		}
		if cache.items[current.key] != current {
			t.Fatal("map and list entry disagree")
		}
		modelEntry, found := model.values[current.key]
		if !found {
			t.Fatalf("cache has unexpected key %d", current.key)
		}
		if current.val != modelEntry.val || !current.meta.expiry.Equal(modelEntry.expiry) {
			t.Fatalf("entry %d mismatch: cache=(%d,%v) model=(%d,%v)",
				current.key, current.val, current.meta.expiry, modelEntry.val, modelEntry.expiry)
		}
		if !hasMin || current.meta.expiry.Before(minExpiry) {
			minExpiry = current.meta.expiry
			hasMin = true
		}
		previous = current
	}
	if previous != cache.tail {
		t.Fatal("tail does not match final list entry")
	}
	if count != len(cache.items) {
		t.Fatalf("list length %d differs from map length %d", count, len(cache.items))
	}
	if hasMin != cache.hasNextExpiry {
		t.Fatalf("hasNextExpiry: got %t, want %t", cache.hasNextExpiry, hasMin)
	}
	// The watermark only has to be conservative: never later than the earliest
	// stored expiry, so a due expiry is never missed.
	if hasMin && cache.nextExpiry.After(minExpiry) {
		t.Fatalf("nextExpiry %v is later than earliest stored expiry %v", cache.nextExpiry, minExpiry)
	}
}

func FuzzExpirableOperations(f *testing.F) {
	f.Add([]byte{3, 1, 2, 3, 4, 5, 6, 7})
	f.Add([]byte{2, 0, 8, 16, 24, 32, 40, 48})

	f.Fuzz(func(t *testing.T, operations []byte) {
		if len(operations) == 0 {
			return
		}
		if len(operations) > 1024 {
			operations = operations[:1024]
		}

		capacity := int(operations[0]%8) + 1
		ttl := time.Duration(int(operations[0]%5)+1) * time.Second
		now := time.Unix(0, 0)
		cache := MustNewExpirable[int, int](capacity, ttl)
		cache.SetTimeNowFunc(func() time.Time { return now })
		model := newExpirableModel(capacity, ttl, now)

		for i, operation := range operations[1:] {
			key := int(operation & 15)
			switch operation % 10 {
			case 0:
				cache.Set(key, i)
				model.set(key, i, model.ttl)
			case 1:
				gotValue, gotFound := cache.Get(key)
				wantValue, wantFound := model.get(key)
				if gotValue != wantValue || gotFound != wantFound {
					t.Fatalf("Get(%d): got (%d, %t), want (%d, %t)", key, gotValue, gotFound, wantValue, wantFound)
				}
			case 2:
				gotValue, gotFound := cache.Peek(key)
				wantValue, wantFound := model.peek(key)
				if gotValue != wantValue || gotFound != wantFound {
					t.Fatalf("Peek(%d): got (%d, %t), want (%d, %t)", key, gotValue, gotFound, wantValue, wantFound)
				}
			case 3:
				if got, want := cache.Remove(key), model.remove(key); got != want {
					t.Fatalf("Remove(%d): got %t, want %t", key, got, want)
				}
			case 4:
				advance := time.Duration(int(operation%5)+1) * 500 * time.Millisecond
				now = now.Add(advance)
				model.now = now
			case 5:
				if got, want := cache.RemoveExpired(), model.removeExpired(); got != want {
					t.Fatalf("RemoveExpired: got %d, want %d", got, want)
				}
			case 6:
				newCapacity := int(operation%8) + 1
				if _, err := cache.Resize(newCapacity); err != nil {
					t.Fatal(err)
				}
				model.resize(newCapacity)
			case 7:
				cache.Clear()
				model.clear()
			case 8:
				_, want := model.peek(key)
				if got := cache.Contains(key); got != want {
					t.Fatalf("Contains(%d): got %t, want %t", key, got, want)
				}
			case 9:
				customTTL := time.Duration(int(operation%4)+1) * 250 * time.Millisecond
				cache.Set(key, -i, WithTTL(customTTL))
				model.set(key, -i, customTTL)
			}
			assertExpirableMatchesModel(t, cache, model)
		}
	})
}

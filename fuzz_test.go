package lru

import (
	"reflect"
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
	if hasMin && !cache.nextExpiry.Equal(minExpiry) {
		t.Fatalf("nextExpiry: got %v, want %v", cache.nextExpiry, minExpiry)
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

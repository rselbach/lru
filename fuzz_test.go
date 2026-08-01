package lru

import (
	"reflect"
	"testing"
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

package lru

import (
	"encoding/binary"
	"hash/maphash"
	"math"
	"reflect"
)

// shardHasher assigns keys to shards for the sharded cache types. It holds the
// per-cache random seeds, so equal keys always reach the same shard within one
// cache while the mapping stays unpredictable across processes.
type shardHasher[K comparable] struct {
	seed       maphash.Seed
	scalarSeed uint64
	// skipKeyCheck is set when K can never produce an invalid key. The zero
	// value validates, so an uninitialized hasher stays safe.
	skipKeyCheck bool
}

func newShardHasher[K comparable]() shardHasher[K] {
	seed := maphash.MakeSeed()
	return shardHasher[K]{
		seed:         seed,
		scalarSeed:   scalarSeed(seed),
		skipKeyCheck: !keysNeedValidation[K](),
	}
}

// index returns the shard index for the given key. Invalid keys go to shard 0
// rather than to hash, which has no defined encoding for them; the shard's own
// method then rejects the key.
func (s *shardHasher[K]) index(key K, shardCount int) int {
	if !s.skipKeyCheck && validateKey(key) != nil {
		return 0
	}
	return int(s.hash(key) % uint64(shardCount))
}

func (s *shardHasher[K]) hash(key K) uint64 {
	// Fast path: scalar keys already fit in one or two machine words, so they
	// are mixed directly rather than routed through maphash setup, a buffered
	// write and finalization.
	switch k := any(key).(type) {
	case int:
		return mixScalar(s.scalarSeed, uint64(int64(k)))
	case int64:
		return mixScalar(s.scalarSeed, uint64(k))
	case int32:
		return mixScalar(s.scalarSeed, uint64(int64(k)))
	case int16:
		return mixScalar(s.scalarSeed, uint64(int64(k)))
	case int8:
		return mixScalar(s.scalarSeed, uint64(int64(k)))
	case uint:
		return mixScalar(s.scalarSeed, uint64(k))
	case uint64:
		return mixScalar(s.scalarSeed, k)
	case uint32:
		return mixScalar(s.scalarSeed, uint64(k))
	case uint16:
		return mixScalar(s.scalarSeed, uint64(k))
	case uint8:
		return mixScalar(s.scalarSeed, uint64(k))
	case uintptr:
		return mixScalar(s.scalarSeed, uint64(k))
	case float64:
		return mixScalar(s.scalarSeed, normalizedFloat64Bits(k))
	case float32:
		return mixScalar(s.scalarSeed, uint64(normalizedFloat32Bits(k)))
	case complex128:
		return mixScalar(
			mixScalar(s.scalarSeed, normalizedFloat64Bits(real(k))),
			normalizedFloat64Bits(imag(k)),
		)
	case complex64:
		return mixScalar(
			mixScalar(s.scalarSeed, uint64(normalizedFloat32Bits(real(k)))),
			uint64(normalizedFloat32Bits(imag(k))),
		)
	case bool:
		if k {
			return mixScalar(s.scalarSeed, 1)
		}
		return mixScalar(s.scalarSeed, 0)
	case string:
		var h maphash.Hash
		h.SetSeed(s.seed)
		h.WriteString(k)
		return h.Sum64()
	}

	var h maphash.Hash
	h.SetSeed(s.seed)
	var buf [8]byte
	writeComparableHash(&h, reflect.ValueOf(key), &buf)
	return h.Sum64()
}

// mixScalar hashes one machine word with the murmur3 finalizer. Equal words
// always produce the same result, which is all shard selection requires.
func mixScalar(seed, value uint64) uint64 {
	value ^= seed
	value ^= value >> 33
	value *= 0xff51afd7ed558ccd
	value ^= value >> 33
	value *= 0xc4ceb9fe1a85ec53
	value ^= value >> 33
	return value
}

// scalarSeed derives the scalar mixing seed from the cache's random maphash
// seed, so scalar keys are spread as unpredictably across processes as the
// keys that still go through maphash.
func scalarSeed(seed maphash.Seed) uint64 {
	var h maphash.Hash
	h.SetSeed(seed)
	h.WriteString("lru: scalar shard seed")
	return h.Sum64()
}

func writeHashUint64(h *maphash.Hash, buf *[8]byte, value uint64) {
	binary.LittleEndian.PutUint64(buf[:], value)
	h.Write(buf[:])
}

func normalizedFloat64Bits(value float64) uint64 {
	if value == 0 {
		return 0
	}
	return math.Float64bits(value)
}

func normalizedFloat32Bits(value float32) uint32 {
	if value == 0 {
		return 0
	}
	return math.Float32bits(value)
}

// writeComparableHash hashes values according to Go equality semantics without
// invoking user-defined formatting methods. Collisions between unequal values
// are harmless; equal values must always produce identical bytes.
func writeComparableHash(h *maphash.Hash, value reflect.Value, buf *[8]byte) {
	if !value.IsValid() {
		buf[0] = 0
		h.Write(buf[:1])
		return
	}

	buf[0] = byte(value.Kind()) + 1
	h.Write(buf[:1])

	switch value.Kind() {
	case reflect.Bool:
		buf[0] = 0
		if value.Bool() {
			buf[0] = 1
		}
		h.Write(buf[:1])
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		writeHashUint64(h, buf, uint64(value.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32,
		reflect.Uint64, reflect.Uintptr:
		writeHashUint64(h, buf, value.Uint())
	case reflect.Float32:
		writeHashUint64(h, buf, uint64(normalizedFloat32Bits(float32(value.Float()))))
	case reflect.Float64:
		writeHashUint64(h, buf, normalizedFloat64Bits(value.Float()))
	case reflect.Complex64:
		complexValue := complex64(value.Complex())
		writeHashUint64(h, buf, uint64(normalizedFloat32Bits(real(complexValue))))
		writeHashUint64(h, buf, uint64(normalizedFloat32Bits(imag(complexValue))))
	case reflect.Complex128:
		complexValue := value.Complex()
		writeHashUint64(h, buf, normalizedFloat64Bits(real(complexValue)))
		writeHashUint64(h, buf, normalizedFloat64Bits(imag(complexValue)))
	case reflect.String:
		writeHashUint64(h, buf, uint64(value.Len()))
		h.WriteString(value.String())
	case reflect.Array:
		writeHashUint64(h, buf, uint64(value.Len()))
		for i := 0; i < value.Len(); i++ {
			writeComparableHash(h, value.Index(i), buf)
		}
	case reflect.Struct:
		writeHashUint64(h, buf, uint64(value.NumField()))
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).Name == "_" {
				continue
			}
			writeComparableHash(h, value.Field(i), buf)
		}
	case reflect.Interface:
		if value.IsNil() {
			writeComparableHash(h, reflect.Value{}, buf)
			return
		}
		writeComparableHash(h, value.Elem(), buf)
	case reflect.Chan, reflect.Pointer, reflect.UnsafePointer:
		writeHashUint64(h, buf, uint64(value.Pointer()))
	default:
		panic("lru: unsupported comparable key kind: " + value.Kind().String())
	}
}

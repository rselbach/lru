package lru

import "math/bits"

// sketchSeeds are the four hash-mixing seeds of the frequency sketch, one per
// counter a key maps to.
var sketchSeeds = [4]uint64{
	0xc3a5c85c97cb3127, 0xb492b66fbe98f273,
	0x9ae16a3b2f90404f, 0xcbf29ce484222325,
}

// frequencySketch is a Count-Min Sketch with 4-bit counters, sixteen packed per
// word, used by [TinyLFU] to estimate how often a key has been accessed. Each
// key maps to four counters; increment saturates them at 15 and frequency reads
// their minimum, since collisions can only inflate a counter. Once sampleSize
// increments have been observed every counter is halved, so stale popularity
// decays instead of squatting.
type frequencySketch struct {
	table      []uint64
	mask       uint64
	sampleSize int
	size       int
}

// newFrequencySketch sizes a sketch for a cache with the given capacity.
func newFrequencySketch(capacity int) *frequencySketch {
	size := 1
	for size < capacity {
		size <<= 1
	}
	return &frequencySketch{
		table:      make([]uint64, size),
		mask:       uint64(size - 1),
		sampleSize: 10 * capacity,
	}
}

func (s *frequencySketch) indexOf(hash uint64, i int) uint64 {
	h := (hash + sketchSeeds[i]) * sketchSeeds[i]
	h += h >> 32
	return h & s.mask
}

// shiftOf spreads a key's four counters across different 4-bit slots of their
// words, so two keys sharing a table row do not collide in every counter.
func shiftOf(hash uint64, i int) uint64 {
	return (((hash & 3) << 2) + uint64(i)) << 2
}

// increment adds one to the key's counters, saturating at 15.
func (s *frequencySketch) increment(hash uint64) {
	added := false
	for i := 0; i < 4; i++ {
		idx := s.indexOf(hash, i)
		shift := shiftOf(hash, i)
		if (s.table[idx]>>shift)&0xf < 0xf {
			s.table[idx] += 1 << shift
			added = true
		}
	}
	if added {
		s.size++
		if s.size >= s.sampleSize {
			s.reset()
		}
	}
}

// frequency estimates how often the key has been incremented, up to 15.
func (s *frequencySketch) frequency(hash uint64) int {
	min := 0xf
	for i := 0; i < 4; i++ {
		count := int((s.table[s.indexOf(hash, i)] >> shiftOf(hash, i)) & 0xf)
		if count < min {
			min = count
		}
	}
	return min
}

// reset halves every counter in place, bounding counts and aging out keys that
// stopped being accessed.
func (s *frequencySketch) reset() {
	odd := 0
	for i := range s.table {
		odd += bits.OnesCount64(s.table[i] & 0x1111111111111111)
		s.table[i] = (s.table[i] >> 1) & 0x7777777777777777
	}
	s.size = (s.size - odd/2) / 2
}

// clear zeroes the sketch.
func (s *frequencySketch) clear() {
	for i := range s.table {
		s.table[i] = 0
	}
	s.size = 0
}

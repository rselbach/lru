package lru

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFrequencySketch_TableSizing(t *testing.T) {
	tests := map[string]struct {
		capacity int
		want     int
	}{
		"one":            {capacity: 1, want: 1},
		"exact power":    {capacity: 64, want: 64},
		"rounds up":      {capacity: 1000, want: 1024},
		"just above pow": {capacity: 65, want: 128},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			s := newFrequencySketch(tc.capacity)
			r.Len(s.table, tc.want)
			r.Equal(uint64(tc.want-1), s.mask)
			r.Equal(10*tc.capacity, s.sampleSize)
		})
	}
}

func TestFrequencySketch_RejectsUnrepresentableCapacity(t *testing.T) {
	require.PanicsWithValue(t, ErrCapacityTooLarge, func() {
		newFrequencySketch(int(^uint(0)>>1)/10 + 1)
	})
}

func TestFrequencySketch_IncrementAndFrequency(t *testing.T) {
	r := require.New(t)
	s := newFrequencySketch(64)
	hash := mixScalar(0x1234, 42)

	r.Zero(s.frequency(hash))

	for i := 1; i <= 7; i++ {
		s.increment(hash)
		r.Equal(i, s.frequency(hash))
	}
}

func TestFrequencySketch_SaturatesAtFifteen(t *testing.T) {
	r := require.New(t)
	// A large capacity keeps sampleSize above the increment count so no reset
	// interferes with the saturation check.
	s := newFrequencySketch(1024)
	hash := mixScalar(0x1234, 7)

	for i := 0; i < 40; i++ {
		s.increment(hash)
	}
	r.Equal(15, s.frequency(hash))
}

func TestFrequencySketch_ResetHalvesCounters(t *testing.T) {
	r := require.New(t)
	s := newFrequencySketch(1024)
	hash := mixScalar(0x1234, 99)

	for i := 0; i < 9; i++ {
		s.increment(hash)
	}
	r.Equal(9, s.frequency(hash))

	s.reset()
	r.Equal(4, s.frequency(hash))
}

func TestFrequencySketch_ResetTriggersAtSampleSize(t *testing.T) {
	r := require.New(t)
	// capacity 1 gives sampleSize 10, so the tenth observed increment halves.
	s := newFrequencySketch(1)
	hash := mixScalar(0x1234, 3)

	for i := 0; i < 10; i++ {
		s.increment(hash)
	}
	// nine increments reached 9, the tenth reached 10 and was then halved
	r.Equal(5, s.frequency(hash))
}

func TestFrequencySketch_Clear(t *testing.T) {
	r := require.New(t)
	s := newFrequencySketch(64)
	hash := mixScalar(0x1234, 5)

	for i := 0; i < 5; i++ {
		s.increment(hash)
	}
	r.Equal(5, s.frequency(hash))

	s.clear()
	r.Zero(s.frequency(hash))
	r.Zero(s.size)
}

// Hot keys must stay distinguishable from cold ones through collisions, which
// is the only property admission relies on.
func TestFrequencySketch_OrdersHotAboveCold(t *testing.T) {
	r := require.New(t)
	s := newFrequencySketch(1024)

	hot := mixScalar(0x9e3779b97f4a7c15, 1)
	for i := 0; i < 12; i++ {
		s.increment(hot)
	}
	// background noise from many other keys
	for k := uint64(100); k < 400; k++ {
		s.increment(mixScalar(0x9e3779b97f4a7c15, k))
	}

	cold := mixScalar(0x9e3779b97f4a7c15, 2)
	r.Greater(s.frequency(hot), s.frequency(cold))
}

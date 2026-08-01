package lru

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCache_RejectsNonReflexiveKeys(t *testing.T) {
	r := require.New(t)
	cache := MustNew[float64, int](1)
	nan := math.NaN()

	r.PanicsWithValue(ErrInvalidKey, func() {
		cache.Set(nan, 1)
	})
	r.Zero(cache.Len())
	_, found := cache.Get(nan)
	r.False(found)

	computed := false
	_, err := cache.GetOrSet(nan, func() (int, error) {
		computed = true
		return 1, nil
	})
	r.ErrorIs(err, ErrInvalidKey)
	r.False(computed)

	_, err = cache.GetOrSetSingleflight(nan, func() (int, error) {
		computed = true
		return 1, nil
	})
	r.ErrorIs(err, ErrInvalidKey)
	r.False(computed)
	r.Nil(cache.sfGroup.calls)
}

func TestExpirable_RejectsCompositeNonReflexiveKeys(t *testing.T) {
	type key struct {
		value float64
	}

	r := require.New(t)
	cache := MustNewExpirable[key, int](1, time.Minute)
	nanKey := key{value: math.NaN()}

	r.PanicsWithValue(ErrInvalidKey, func() {
		cache.Set(nanKey, 1)
	})
	r.Zero(cache.PhysicalLen())

	_, err := cache.GetOrSetSingleflight(nanKey, func() (int, error) {
		return 1, nil
	})
	r.ErrorIs(err, ErrInvalidKey)
	r.Nil(cache.sfGroup.calls)
}

func TestSharded_RejectsNonReflexiveKeys(t *testing.T) {
	r := require.New(t)
	cache := MustNewSharded[float64, int](16)
	nan := math.NaN()

	r.PanicsWithValue(ErrInvalidKey, func() {
		cache.Set(nan, 1)
	})
	r.Zero(cache.Len())

	_, err := cache.GetOrSetSingleflight(nan, func() (int, error) {
		return 1, nil
	})
	r.ErrorIs(err, ErrInvalidKey)
}

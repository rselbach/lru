package lru

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDynamicallyComparable(t *testing.T) {
	tests := map[string]struct {
		value any
		want  bool
	}{
		"plain value":               {value: 42, want: true},
		"comparable interface":      {value: struct{ Value any }{Value: "troy"}, want: true},
		"uncomparable interface":    {value: struct{ Value any }{Value: []int{1}}, want: false},
		"nested uncomparable value": {value: [1]struct{ Value any }{{Value: map[string]int{}}}, want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, dynamicallyComparable(reflect.ValueOf(tc.value)))
		})
	}
}

func TestTypeNeedsValidation(t *testing.T) {
	type plain struct {
		name string
		id   int
	}
	type withFloat struct {
		score float64
	}
	type withAny struct {
		value any
	}
	type nestedAny struct {
		inner [2]withAny
	}
	type blankAny struct {
		name string
		_    any
	}

	tests := map[string]struct {
		typ  reflect.Type
		want bool
	}{
		"string":            {typ: reflect.TypeOf(""), want: false},
		"int":               {typ: reflect.TypeOf(0), want: false},
		"bool":              {typ: reflect.TypeOf(false), want: false},
		"uintptr":           {typ: reflect.TypeOf(uintptr(0)), want: false},
		"pointer":           {typ: reflect.TypeOf((*int)(nil)), want: false},
		"channel":           {typ: reflect.TypeOf((chan int)(nil)), want: false},
		"array of int":      {typ: reflect.TypeOf([2]int{}), want: false},
		"struct of scalars": {typ: reflect.TypeOf(plain{name: "troy", id: 1}), want: false},
		"float32":           {typ: reflect.TypeOf(float32(0)), want: true},
		"float64":           {typ: reflect.TypeOf(float64(0)), want: true},
		"complex64":         {typ: reflect.TypeOf(complex64(0)), want: true},
		"complex128":        {typ: reflect.TypeOf(complex128(0)), want: true},
		"interface":         {typ: reflect.TypeOf((*any)(nil)).Elem(), want: true},
		"struct with float": {typ: reflect.TypeOf(withFloat{score: 1}), want: true},
		"struct with any":   {typ: reflect.TypeOf(withAny{value: "troy"}), want: true},
		"array of float":    {typ: reflect.TypeOf([2]float64{}), want: true},
		"nested any in array": {
			typ:  reflect.TypeOf(nestedAny{inner: [2]withAny{{value: "troy"}}}),
			want: true,
		},
		"blank any field": {typ: reflect.TypeOf(blankAny{name: "troy"}), want: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, typeNeedsValidation(tc.typ))
		})
	}
}

// Skipping validation is only sound when every value of K is a valid key, so
// the types that validateKey can reject must never be skipped.
func TestKeysNeedValidation(t *testing.T) {
	r := require.New(t)

	r.False(keysNeedValidation[string]())
	r.False(keysNeedValidation[int]())
	r.False(keysNeedValidation[struct {
		name string
		id   int
	}]())

	r.True(keysNeedValidation[float64]())
	r.True(keysNeedValidation[namedFloat64]())
	r.True(keysNeedValidation[complex128]())
	r.True(keysNeedValidation[struct{ score float64 }]())
	// Interface-carrying keys are covered by TestTypeNeedsValidation: they
	// only satisfy comparable from go1.20 on, and go.mod pins -lang to 1.18.
}

func TestCache_SkipsValidationForAlwaysValidKeys(t *testing.T) {
	type key struct {
		region string
		id     int
	}

	r := require.New(t)
	troy := key{region: "greendale", id: 7}

	cache := MustNew[key, int](2)
	r.True(cache.skipKeyCheck)
	r.True(cache.sfGroup.skipKeyCheck)
	cache.Set(troy, 1)
	value, found := cache.Get(troy)
	r.True(found)
	r.Equal(1, value)

	sharded := MustNewSharded[key, int](2)
	r.True(sharded.hasher.skipKeyCheck)
	sharded.Set(troy, 2)
	value, found = sharded.Get(troy)
	r.True(found)
	r.Equal(2, value)

	expirable := MustNewExpirable[key, int](2, time.Minute)
	r.True(expirable.skipKeyCheck)
	expirable.Set(troy, 3)
	value, found = expirable.Get(troy)
	r.True(found)
	r.Equal(3, value)

	// Float components can be NaN, so those keys must still be validated.
	r.False(MustNew[float64, int](2).skipKeyCheck)
	r.False(MustNewSharded[float64, int](2).hasher.skipKeyCheck)
	r.False(MustNewExpirable[float64, int](2, time.Minute).skipKeyCheck)
}

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

func TestSetErrReturnsInvalidKeyForEveryCacheType(t *testing.T) {
	type setErrCache struct {
		name string
		set  func(float64, int) error
		len  func() int
	}

	lruCache := MustNew[float64, int](1)
	expirable := MustNewExpirable[float64, int](1, time.Minute)
	sharded := MustNewShardedWithCount[float64, int](1, 1)
	clock := MustNewClockWithCount[float64, int](1, 1)
	tinyLFU := MustNewTinyLFUWithCount[float64, int](1, 1)
	tests := []setErrCache{
		{name: "Cache", set: lruCache.SetErr, len: lruCache.Len},
		{name: "Expirable", set: func(key float64, value int) error {
			return expirable.SetErr(key, value)
		}, len: expirable.PhysicalLen},
		{name: "Sharded", set: sharded.SetErr, len: sharded.Len},
		{name: "Clock", set: clock.SetErr, len: clock.Len},
		{name: "TinyLFU", set: tinyLFU.SetErr, len: tinyLFU.Len},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := require.New(t)
			r.NoError(tt.set(1, 1))
			r.Equal(1, tt.len())
			r.ErrorIs(tt.set(math.NaN(), 2), ErrInvalidKey)
			r.Equal(1, tt.len())
		})
	}
}

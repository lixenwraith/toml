package toml

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestCheckedNumericConversions(t *testing.T) {
	type Count uint64
	tests := []struct {
		value  any
		target any
		want   any
		valid  bool
	}{
		{uint(42), new(int64), int64(42), true},
		{Count(math.MaxUint64), new(uint64), uint64(math.MaxUint64), true},
		{uint64(math.MaxUint64), new(int64), nil, false},
		{int64(-1), new(uint64), nil, false},
		{float64(0x1p63), new(int64), nil, false},
		{float64(-0x1p63), new(int64), int64(math.MinInt64), true},
		{float64(0x1p64), new(uint64), nil, false},
		{1.5, new(int), nil, false},
		{math.NaN(), new(int), nil, false},
		{math.Inf(1), new(uint64), nil, false},
		{float32(12), new(uint8), uint8(12), true},
		{uint64(1<<53) + 1, new(float64), nil, false},
		{int64(1<<24) + 1, new(float32), nil, false},
		{uint64(1 << 63), new(float64), float64(1 << 63), true},
		{1e100, new(float32), nil, false},
	}
	for _, tt := range tests {
		err := Decode(tt.value, tt.target)
		if (err == nil) != tt.valid {
			t.Errorf("%v (%T) -> %T: %v", tt.value, tt.value, tt.target, err)
		}
		if tt.valid && !reflect.DeepEqual(reflect.ValueOf(tt.target).Elem().Interface(), tt.want) {
			t.Errorf("got %v, want %v", tt.target, tt.want)
		}
	}
}

func TestDecodeTransactionAndNamedKeys(t *testing.T) {
	type Key string
	var m map[Key]int
	if err := Decode(map[string]any{"x": 4}, &m); err != nil || m["x"] != 4 {
		t.Fatalf("%v %#v", err, m)
	}
	type C struct {
		First  int `toml:",omitempty"`
		Second int8
	}
	c := C{7, 8}
	if err := Decode(map[string]any{"First": 11, "Second": 400}, &c); err == nil || c != (C{7, 8}) {
		t.Fatalf("partial update: %+v %v", c, err)
	}
	if err := Decode(map[string]any{"First": 11}, &c); err != nil || c.First != 11 {
		t.Fatalf("empty tag name: %+v %v", c, err)
	}
}

func TestRoundTripSupportedValues(t *testing.T) {
	input := map[string]any{
		"dot.key": map[string]any{"quoted\"key": map[string]any{"x": "\b\f\x00\x7f界"}},
		"items":   []any{map[string]any{"x": int64(3)}, map[string]any{"x": int64(4)}},
		"nested":  []any{[]any{map[string]any{"x": true}}},
		"mixed":   []any{int64(1), map[string]any{"y": "text"}},
	}
	data, err := Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewParser(data).Parse()
	if err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	if !reflect.DeepEqual(normalize(input), normalize(got)) {
		t.Fatalf("round trip: %#v", got)
	}
}

func TestMarshalRejectsInvalidValues(t *testing.T) {
	cycle := map[string]any{}
	cycle["self"] = cycle
	for _, value := range []any{
		map[string]any{"u": uint64(math.MaxUint64)},
		map[string]any{"s": string([]byte{0xff})},
		map[string]any{"123": 1},
		struct {
			A int `toml:"x"`
			B int `toml:"x"`
		}{},
		cycle,
	} {
		if _, err := Marshal(value); err == nil {
			t.Errorf("invalid %T accepted", value)
		}
	}
}

func TestParseBoundariesAndSyntax(t *testing.T) {
	m, err := NewParser([]byte("min = -9223372036854775808\nmax = 9223372036854775807")).Parse()
	if err != nil || m["min"] != int64(math.MinInt64) || m["max"] != int64(math.MaxInt64) {
		t.Fatalf("%#v %v", m, err)
	}
	for _, s := range []string{"x=9223372036854775808", "x=1 y=2", "[x] y=2", "x=[1\n2]", "x={a=1\nb=2}", "x=\"\x00\"", "x=\"\xff\"", "[" + strings.Repeat("a.", 1100) + "b]"} {
		if _, err := NewParser([]byte(s)).Parse(); err == nil {
			t.Errorf("accepted invalid input %q", s)
		}
	}
}

func FuzzIntegerConversions(f *testing.F) {
	f.Add(uint64(math.MaxUint64))
	f.Add(uint64(1<<53) + 1)
	f.Add(uint64(0))
	f.Fuzz(func(t *testing.T, n uint64) {
		var u uint64
		if err := Decode(n, &u); err != nil || u != n {
			t.Fatalf("uint64 changed: %d -> %d (%v)", n, u, err)
		}
		var i int64
		err := Decode(n, &i)
		if (err == nil) != (n <= math.MaxInt64) {
			t.Fatalf("signed boundary: %d %v", n, err)
		}
		if err == nil && uint64(i) != n {
			t.Fatal("signed conversion changed value")
		}
	})
}

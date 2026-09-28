package toml

import (
	"math"
	"reflect"
	"testing"
)

func TestDecodeNumericBoundaries(t *testing.T) {
	type count uint64
	for _, tc := range []struct {
		name string
		in   any
		out  any
		want any
		bad  bool
	}{
		{"uint", uint(42), new(int64), int64(42), false},
		{"full unsigned", uint64(math.MaxUint64), new(uint64), uint64(math.MaxUint64), false},
		{"named unsigned", count(math.MaxUint64), new(uint64), uint64(math.MaxUint64), false},
		{"float32", float32(42), new(int8), int8(42), false},
		{"fraction", 1.5, new(int64), nil, true},
		{"negative fraction", -0.5, new(uint64), nil, true},
		{"signed boundary", float64(1 << 63), new(int64), nil, true},
		{"unsigned boundary", float64(1 << 64), new(uint64), nil, true},
		{"nan", math.NaN(), new(int64), nil, true},
		{"inf", math.Inf(1), new(int64), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Decode(tc.in, tc.out)
			if tc.bad {
				if err == nil {
					t.Fatal("accepted lossy conversion")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := reflect.ValueOf(tc.out).Elem().Interface(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestDecodeTransactionalAndNamedMapKey(t *testing.T) {
	type key string
	var m map[key]int
	if err := Decode(map[string]any{"x": 3}, &m); err != nil || m["x"] != 3 {
		t.Fatalf("named map key: %v %v", m, err)
	}
	dst := struct {
		A int
		B uint8
	}{A: 1, B: 2}
	if err := Decode(map[string]any{"A": 99, "B": 256}, &dst); err == nil {
		t.Fatal("overflow accepted")
	}
	if dst.A != 1 || dst.B != 2 {
		t.Fatalf("partially changed target: %+v", dst)
	}
	tagged := struct {
		Value int `toml:",omitempty"`
	}{}
	if err := Decode(map[string]any{"Value": 7}, &tagged); err != nil || tagged.Value != 7 {
		t.Fatalf("empty tag name: %+v %v", tagged, err)
	}
}

func TestMarshalNestedRoundTrip(t *testing.T) {
	for _, input := range []string{`"a.b" = { "c d" = 1 }`, `entries = [{n = 1}, {n = 2}]`, `["true"]
n = 3`} {
		original, err := NewParser([]byte(input)).Parse()
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		got, err := NewParser(encoded).Parse()
		if err != nil {
			t.Fatalf("%s: %v", encoded, err)
		}
		if !reflect.DeepEqual(normalize(original), normalize(got)) {
			t.Fatalf("lost data: %s\ngot %#v", encoded, got)
		}
	}
	if _, err := Marshal(map[string]any{"n": uint64(math.MaxUint64)}); err == nil {
		t.Fatal("emitted integer parser cannot read")
	}
}

func FuzzNumericDecode(f *testing.F) {
	f.Add(uint64(math.MaxUint64))
	f.Add(uint64(0))
	f.Add(uint64(1 << 63))
	f.Add(uint64(255))
	f.Fuzz(func(t *testing.T, n uint64) {
		var u uint64
		if err := Decode(n, &u); err != nil || u != n {
			t.Fatalf("unsigned loss: %d %v", u, err)
		}
		var s int64
		err := Decode(n, &s)
		if n > math.MaxInt64 {
			if err == nil {
				t.Fatal("signed wrap")
			}
		} else if err != nil || uint64(s) != n {
			t.Fatalf("signed loss: %d %v", s, err)
		}
		var b uint8
		err = Decode(n, &b)
		if n > math.MaxUint8 {
			if err == nil {
				t.Fatal("narrowing wrap")
			}
		} else if err != nil || uint64(b) != n {
			t.Fatalf("byte loss: %d %v", b, err)
		}
	})
}

func TestCyclicAndUnrepresentableValues(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	if _, err := Marshal(m); err == nil {
		t.Fatal("cyclic map accepted")
	}
	type node struct {
		Next *node `toml:"self"`
	}
	var n node
	if err := Decode(m, &n); err == nil {
		t.Fatal("cyclic decode accepted")
	}
	type key string
	data, err := Marshal(map[key]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["x"] != 1 {
		t.Fatalf("lost named key: %s", data)
	}
	if _, err := Marshal(map[string]any{"1234567890123456789012345": 1}); err == nil {
		t.Fatal("numeric key emitted")
	}
}

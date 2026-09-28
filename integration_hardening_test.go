package toml

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestDecodeFixedArrayRoundTrip(t *testing.T) {
	type Item struct {
		Value uint16 `toml:"value"`
	}
	type Config struct {
		Numbers [3]int64   `toml:"numbers"`
		Items   [2]*Item   `toml:"items"`
		Matrix  [2][2]int8 `toml:"matrix"`
		Empty   [0]string  `toml:"empty"`
	}
	want := Config{
		Numbers: [3]int64{math.MinInt64, 0, math.MaxInt64},
		Items:   [2]*Item{{1}, {math.MaxUint16}},
		Matrix:  [2][2]int8{{-128, 127}, {0, 1}},
	}
	data, err := Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got Config
	if err := Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for _, input := range []any{[]any{1}, []any{1, 2, 3}, []any{1, 256}} {
		dst := [2]uint8{7, 8}
		if err := Decode(input, &dst); err == nil || dst != [2]uint8{7, 8} {
			t.Fatalf("invalid array changed target: %v (%v)", dst, err)
		}
	}
}

func TestDecodePointerDefaultsAndRollback(t *testing.T) {
	type Settings struct {
		Port uint16 `toml:"port"`
		Name string `toml:"name"`
	}
	original := &Settings{Port: 8080, Name: "default"}
	dst := original
	if err := Decode(map[string]any{"name": "updated"}, &dst); err != nil {
		t.Fatal(err)
	}
	if dst.Port != 8080 || dst.Name != "updated" || dst == original || original.Name != "default" {
		t.Fatalf("defaults or original pointer changed: dst=%+v original=%+v", dst, original)
	}
	type Config struct {
		Settings *Settings
		Map      map[string]int
		Slice    []int
		Limit    uint8
	}
	cfg := Config{original, map[string]int{"old": 1}, []int{2}, 3}
	err := Decode(map[string]any{
		"Settings": map[string]any{"port": 9090},
		"Map":      map[string]any{"new": 4},
		"Slice":    []any{5},
		"Limit":    256,
	}, &cfg)
	if err == nil || cfg.Settings != original || *original != (Settings{8080, "default"}) ||
		!reflect.DeepEqual(cfg.Map, map[string]int{"old": 1}) || !reflect.DeepEqual(cfg.Slice, []int{2}) || cfg.Limit != 3 {
		t.Fatalf("partial update: %+v (%v)", cfg, err)
	}
}

func TestDecodeCyclicInputErrors(t *testing.T) {
	cycle := map[string]any{}
	cycle["self"] = cycle
	var number uint64
	if err := Decode(cycle, &number); err == nil {
		t.Fatal("accepted cyclic map as unsigned number")
	}
	type Node struct {
		Next *Node `toml:"self"`
	}
	var node Node
	if err := Decode(cycle, &node); err == nil || node.Next != nil {
		t.Fatalf("recursive decode must fail without publication: %v", err)
	}
}

func TestNumericKeyRangeIndependence(t *testing.T) {
	for _, key := range []string{"0", "-1", "+1", "9223372036854775808", "-9223372036854775809", strings.Repeat("9", 100)} {
		if _, err := Marshal(map[string]any{key: 1}); err == nil {
			t.Errorf("encoded numeric key %q", key)
		}
		if _, err := NewParser([]byte(strconv.Quote(key) + " = 1")).Parse(); err == nil {
			t.Errorf("parsed numeric key %q", key)
		}
	}
}

func TestDecodeIntegerWidths(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[int](), reflect.TypeFor[int8](), reflect.TypeFor[int16](), reflect.TypeFor[int32](), reflect.TypeFor[int64](),
		reflect.TypeFor[uint](), reflect.TypeFor[uint8](), reflect.TypeFor[uint16](), reflect.TypeFor[uint32](), reflect.TypeFor[uint64](),
	} {
		t.Run(typ.String(), func(t *testing.T) {
			bits := typ.Bits()
			signed := typ.Kind() >= reflect.Int && typ.Kind() <= reflect.Int64
			max := uint64(math.MaxUint64) >> (64 - bits)
			if signed {
				max >>= 1
			}
			dst := reflect.New(typ)
			if err := Decode(max, dst.Interface()); err != nil {
				t.Fatal(err)
			}
			if max < math.MaxUint64 {
				before := dst.Elem().Interface()
				if err := Decode(max+1, dst.Interface()); err == nil || dst.Elem().Interface() != before {
					t.Fatalf("overflow was accepted or mutated destination: %v", err)
				}
			}
			if signed {
				min := -int64(max) - 1
				if err := Decode(min, dst.Interface()); err != nil || dst.Elem().Int() != min {
					t.Fatalf("minimum changed: %v", err)
				}
				if min > math.MinInt64 {
					if err := Decode(min-1, dst.Interface()); err == nil || dst.Elem().Int() != min {
						t.Fatalf("underflow accepted: %v", err)
					}
				}
			} else if err := Decode(int64(-1), dst.Interface()); err == nil {
				t.Fatal("negative signed integer accepted")
			}
		})
	}
}

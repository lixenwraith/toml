package toml

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestDecode_NumericOverflow(t *testing.T) {
	type T struct {
		I8  int8    `toml:"i8"`
		U8  uint8   `toml:"u8"`
		F32 float32 `toml:"f32"`
	}
	var tgt T
	if err := Decode(map[string]any{"i8": 300}, &tgt); err == nil {
		t.Errorf("300 into int8 must error, got %d", tgt.I8)
	}
	if err := Decode(map[string]any{"u8": 256}, &tgt); err == nil {
		t.Errorf("256 into uint8 must error, got %d", tgt.U8)
	}
	if err := Decode(map[string]any{"f32": 1e300}, &tgt); err == nil {
		t.Errorf("1e300 into float32 must error, got %g", tgt.F32)
	}
	// Boundaries remain valid
	if err := Decode(map[string]any{"i8": 127, "u8": 255}, &tgt); err != nil {
		t.Fatalf("boundary decode failed: %v", err)
	}
	if tgt.I8 != 127 || tgt.U8 != 255 {
		t.Errorf("boundary values: %d %d", tgt.I8, tgt.U8)
	}
}

func TestDecode_UnsupportedKinds(t *testing.T) {
	type T struct {
		C complex128 `toml:"v"`
	}
	var c T
	if err := Decode(map[string]any{"v": 1}, &c); err == nil {
		t.Error("complex128 target must error, not zero-fill")
	}
	type T2 struct {
		Ch chan int `toml:"v"`
	}
	var c2 T2
	if err := Decode(map[string]any{"v": 1}, &c2); err == nil {
		t.Error("chan target must error")
	}
}

func TestDecode_NonEmptyInterface(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("input-triggered panic: %v", r)
		}
	}()
	type T struct {
		R interface{ Read([]byte) (int, error) } `toml:"r"`
	}
	var tgt T
	if err := Decode(map[string]any{"r": map[string]any{}}, &tgt); err == nil {
		t.Error("map into non-empty interface must error")
	}
}

func TestDecode_Uint64Wrap(t *testing.T) {
	type T struct {
		V int64 `toml:"v"`
	}
	var tgt T
	if err := Decode(map[string]any{"v": uint64(math.MaxUint64)}, &tgt); err == nil {
		t.Errorf("MaxUint64 into int64 must error, got %d", tgt.V)
	}
	if err := Decode(map[string]any{"v": ^uint(0)}, &tgt); strconv.IntSize == 64 && err == nil {
		t.Errorf("uint wrap into int64 must error, got %d", tgt.V)
	}
	if err := Decode(map[string]any{"v": uint64(42)}, &tgt); err != nil || tgt.V != 42 {
		t.Errorf("in-range uint64 failed: %v %d", err, tgt.V)
	}
}

func TestMarshal_FloatFormatting(t *testing.T) {
	b, err := Marshal(map[string]any{"f": float32(3.14)})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != "f = 3.14" {
		t.Errorf("float32 noise digits: %s", got)
	}

	b, err = Marshal(map[string]any{"f": 1e100})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != "f = 1e+100" {
		t.Errorf("digit expansion instead of exponent: %s", got)
	}
	var m map[string]any
	if err := Unmarshal(b, &m); err != nil {
		t.Fatalf("re-parse of exponent form failed: %v", err)
	}
	if m["f"] != 1e100 {
		t.Errorf("round trip mismatch: %v", m["f"])
	}

	// Integral floats keep the .0 fixup
	b, _ = Marshal(map[string]any{"f": 100.0})
	if got := strings.TrimSpace(string(b)); got != "f = 100.0" {
		t.Errorf(".0 fixup lost: %s", got)
	}
}

func TestMarshal_NaNInf(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := Marshal(map[string]any{"f": v}); err == nil {
			t.Errorf("%v must fail to encode", v)
		}
	}
}

func TestMarshal_MixedArraysAndNil(t *testing.T) {
	input := map[string]any{"x": []any{map[string]any{"a": int64(1)}, int64(2)}}
	data, err := Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input, got) {
		t.Fatalf("got %#v", got)
	}
	for _, value := range []any{[]any{1, nil}, []*struct{ N int }{nil}} {
		if _, err := Marshal(map[string]any{"x": value}); err == nil {
			t.Fatal("nil array element accepted")
		}
	}
}

func TestMarshal_TagSortedKeys(t *testing.T) {
	type T struct {
		B int `toml:"z"`
		Z int `toml:"a"`
	}
	b, err := Marshal(T{B: 1, Z: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b)); got != "a = 2\nz = 1" {
		t.Errorf("keys not sorted by emitted name:\n%s", got)
	}
}

func TestLexer_UnicodeEscapes(t *testing.T) {
	var m map[string]any
	if err := Unmarshal([]byte(`s = "caf\u00E9 \U0001F600"`), &m); err != nil {
		t.Fatalf("unicode escape parse failed: %v", err)
	}
	if m["s"] != "café 😀" {
		t.Errorf("got %q", m["s"])
	}

	for _, in := range []string{
		`s = "\uD800"`,     // surrogate
		`s = "\u12"`,       // too few digits
		`s = "\U00110000"`, // > MaxRune
		`s = "\uZZZZ"`,     // non-hex
	} {
		if err := Unmarshal([]byte(in), &m); err == nil {
			t.Errorf("%s should fail", in)
		}
	}
}

func TestParser_InlineTableImmutable(t *testing.T) {
	for _, in := range []string{
		"t = { a = 1 }\nt.b = 2",
		"t = { a = 1 }\n[t]\nb = 2",
		"t = { a = 1 }\n[t.sub]\nb = 2",
	} {
		p := NewParser([]byte(in))
		if _, err := p.Parse(); err == nil {
			t.Errorf("inline table extension should fail:\n%s", in)
		}
	}
	// Intra-table dotted keys remain valid
	var m map[string]any
	if err := Unmarshal([]byte(`t = { a.b = 1, a.c = 2 }`), &m); err != nil {
		t.Errorf("dotted keys within inline table must parse: %v", err)
	}
}

func TestParser_ValueDepthLimit(t *testing.T) {
	input := "x = " + strings.Repeat("[", 5000) + strings.Repeat("]", 5000)
	p := NewParser([]byte(input))
	if _, err := p.Parse(); err == nil {
		t.Error("expected nesting depth error")
	}
}

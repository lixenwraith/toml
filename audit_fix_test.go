package toml

import (
	"errors"
	"fmt"
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

// Each dotted-key segment and header part is a map; the document's budget of
// them bounds memory, which input size alone does not (x167 for dotted keys).
func TestParser_TableBudget(t *testing.T) {
	doc := []byte("a" + strings.Repeat(".a", 101) + " = 1\n") // 101 maps
	p := NewParser(doc)
	p.MaxTables = 100
	if _, err := p.Parse(); err == nil || !strings.Contains(err.Error(), "exceeds 100 tables") {
		t.Fatalf("101 tables under a budget of 100: %v", err)
	}
	p = NewParser(doc)
	p.MaxTables = 101
	if _, err := p.Parse(); err != nil {
		t.Fatalf("within budget: %v", err)
	}
	var b strings.Builder
	for i := range DefaultMaxTables {
		fmt.Fprintf(&b, "[t%d]\n", i)
	}
	if _, err := NewParser([]byte(b.String() + "[last]\n")).Parse(); err == nil {
		t.Fatal("the default budget does not hold")
	}
}

// TOML 1.1 escapes decode as TOML 1.1 readers decode them
func TestLexer_TOML11Escapes(t *testing.T) {
	m, err := NewParser([]byte(`s = "\x61\e\xe9"`)).Parse()
	if err != nil || m["s"] != "a\x1bé" {
		t.Fatalf("got %q, %v", m["s"], err)
	}
	if _, err := NewParser([]byte(`s = "\x6"`)).Parse(); err == nil {
		t.Fatal(`\x with one hex digit accepted`)
	}
}

// Outside strings a control character can only hide text from a reader: it
// is refused in comments, and a CR is accepted only as part of CRLF.
func TestLexer_ControlCharactersOutsideStrings(t *testing.T) {
	for _, doc := range []string{
		"a = 1 # see \x1b[8m\nb = 2",
		"a = 1 # hidden\rb = 2\n",
		"a = 1\rb = 2\n",
		"a =\r1\n",
		"a = 1 # del \x7f\n",
	} {
		if _, err := NewParser([]byte(doc)).Parse(); err == nil {
			t.Errorf("%q accepted", doc)
		}
	}
	m, err := NewParser([]byte("a = 1 # c\t\r\n[t] # d\r\nb = 2\r\n")).Parse()
	if err != nil || m["a"] != int64(1) {
		t.Fatalf("CRLF document: %v %v", m, err)
	}
}

// The doubled brackets of an array-of-tables header are one delimiter
func TestParser_ArrayTableDelimitersAdjacent(t *testing.T) {
	for _, doc := range []string{"[ [a] ]\n", "[[a] ]\n", "[ [a]]\n"} {
		if _, err := NewParser([]byte(doc)).Parse(); err == nil {
			t.Errorf("%q accepted as a header", doc)
		}
	}
	for _, doc := range []string{"[[a]]\n", "[ a ]\n", "[[ a ]]\n"} {
		if _, err := NewParser([]byte(doc)).Parse(); err != nil {
			t.Errorf("%q: %v", doc, err)
		}
	}
}

// Errors stay small and never carry a string's content, which may be a
// secret (a verifier in a credentials file)
func TestErrorsBoundedWithoutStringContent(t *testing.T) {
	for _, doc := range []string{`stored_key "SECRETVALUE"`, `password = SECRETVALUE`} {
		_, err := NewParser([]byte(doc + "\n")).Parse()
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("value content in error: %v", err)
		}
	}
	_, err := NewParser([]byte("n = 1" + strings.Repeat("0", 1<<20) + "\n")).Parse()
	if err == nil || len(err.Error()) > 200 {
		t.Fatalf("error of %d bytes", len(err.Error()))
	}
}

// A decode error holds its path once, not a copy of the message per level
func TestDecodeErrorPathIsLinear(t *testing.T) {
	type deep map[string]deep
	var data any = "not a table"
	for range 500 {
		data = map[string]any{"key": data}
	}
	err := Decode(data, new(deep))
	var total int
	for e := err; e != nil; e = errors.Unwrap(e) {
		total += len(e.Error())
	}
	if err == nil || total > 3*len(err.Error()) {
		t.Fatalf("messages along the chain: %d bytes for a %d-byte error", total, len(err.Error()))
	}
}

// Header paths are bounded: deep nesting is written inline past the bound,
// so output stays linear and still round-trips
func TestMarshal_HeaderLengthBounded(t *testing.T) {
	root := map[string]any{}
	cur := root
	for range 300 {
		next := map[string]any{"v": int64(1)}
		cur["long_key_0123"] = next
		cur = next
	}
	out, err := Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 64*1024 {
		t.Fatalf("%d bytes for 300 levels", len(out))
	}
	back, err := NewParser(out).Parse()
	if err != nil || !reflect.DeepEqual(normalize(back), normalize(root)) {
		t.Fatalf("round trip: %v", err)
	}
}

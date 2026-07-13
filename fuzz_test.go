package toml

import (
	"reflect"
	"testing"
	"unicode/utf8"
)

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"",
		`key = "value"`,
		"[table]\nk = 1",
		"[[arr]]\nx = 1.5\n[[arr]]\nx = 2e3",
		"t = { a = 1, b = { c = [1, 2] } }",
		`s = "esc \u00E9 \t"`,
		"n = -0x10\nb = 0b101\no = 0o17",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		p := NewParser(data)
		_, _ = p.Parse() // property: no panic, no hang
	})
}

// normalize collapses the parser's dual slice representations
// ([]map[string]any for [[headers]], []any for inline arrays) so the
// fixpoint compares values, not construction syntax.
func normalize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, vv := range t {
			t[k] = normalize(vv)
		}
		return t
	case []map[string]any:
		out := make([]any, len(t))
		for i, m := range t {
			out[i] = normalize(m)
		}
		return out
	case []any:
		for i, vv := range t {
			t[i] = normalize(vv)
		}
		return t
	}
	return v
}

func FuzzRoundTrip(f *testing.F) {
	for _, s := range []string{
		"a = 1\nb = 2.5\nc = true\nd = \"x\"",
		"[t]\nk = [1, 2, 3]",
		"[[s]]\nn = \"a\"\n[[s]]\nn = \"b\"",
		`m = { x = 1, y = { z = "q" } }`,
		`u = "日本語 \u00E9"`,
		"big = 1e100\nsmall = 1e-100",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		// Encoder normalizes invalid UTF-8 to U+FFFD (WriteRune); byte-exact
		// round trip is only a property of valid input.
		if !utf8.Valid(data) {
			t.Skip()
		}
		m1, err := NewParser(data).Parse()
		if err != nil {
			t.Skip()
		}
		out, err := Marshal(m1)
		if err != nil {
			// Parser accepts values the encoder cannot represent
			// (mixed table/scalar arrays). Documented asymmetry.
			t.Skip()
		}
		m2, err := NewParser(out).Parse()
		if err != nil {
			t.Fatalf("emitted TOML failed to re-parse: %v\nemitted:\n%s", err, out)
		}
		if !reflect.DeepEqual(normalize(m1), normalize(m2)) {
			t.Fatalf("fixpoint violation:\nm1: %#v\nm2: %#v\nemitted:\n%s", m1, m2, out)
		}
	})
}

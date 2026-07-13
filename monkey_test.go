package toml

import (
	"strings"
	"testing"
)

func TestDecode_UnexportedFieldPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Recovered from panic: %v. Logic should skip unexported fields.", r)
		}
	}()

	data := map[string]any{"secret": "hacker"}
	type Security struct {
		secret string
		Public string `toml:"secret"`
	}

	var s Security
	_ = Decode(data, &s)
}

func TestLexer_InvalidNumbers(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"1.0a", true},    // Not valid TOML document structure
		{"1.-0", true},    // Not valid TOML document structure
		{"0xG1", true},    // Invalid hex digit
		{"+", true},       // Lone + is invalid TOML
		{"[1.2.3]", true}, // Multi-dot in numeric context
	}

	for _, tc := range tests {
		p := NewParser([]byte(tc.input))
		_, err := p.Parse()
		if tc.wantErr && err == nil {
			t.Errorf("Input %q should have failed parsing", tc.input)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("Input %q unexpected error: %v", tc.input, err)
		}
	}
}

func TestDecode_DeepPointers(t *testing.T) {
	data := map[string]any{"val": 42}
	type T struct {
		Val ******int `toml:"val"`
	}
	var tgt T
	if err := Decode(data, &tgt); err != nil {
		t.Fatalf("Deep pointer decode failed: %v", err)
	}
	if ******tgt.Val != 42 {
		t.Errorf("Expected 42, got %d", ******tgt.Val)
	}
}

func TestDecode_LargeIntPrecision(t *testing.T) {
	largeVal := int64(4611686018427387905)
	data := map[string]any{"id": int(largeVal)}

	type T struct {
		ID int64 `toml:"id"`
	}
	var tgt T
	_ = Decode(data, &tgt)

	if tgt.ID != largeVal {
		t.Errorf("Precision loss detected: got %d, want %d", tgt.ID, largeVal)
	}
}

func TestParser_NumericKeyRejection(t *testing.T) {
	inputs := [][]byte{
		[]byte(`123 = "value"`),
		[]byte(`[123]`),
		[]byte(`[a.123.b]`),
	}

	for _, in := range inputs {
		p := NewParser(in)
		if _, err := p.Parse(); err == nil {
			t.Errorf("Parser should have rejected numeric key in: %s", string(in))
		}
	}
}

func TestPanic_LexerInfinity(t *testing.T) {
	input := []byte("key = \"\x00\xff\"\n[table\x00]")
	l := NewLexer(input)
	for i := 0; i < 100; i++ {
		tok := l.NextToken()
		if tok.Type == TokenEOF {
			return
		}
	}
	t.Error("Lexer likely stuck in infinite loop on invalid input")
}

func TestPanic_DeepNesting(t *testing.T) {
	depth := 1000
	input := strings.Repeat("a.", depth) + "b = 1"
	p := NewParser([]byte(input))
	_, err := p.Parse()
	if err != nil && !strings.Contains(err.Error(), "key path conflict") {
		t.Logf("Caught expected deep nesting error: %v", err)
	}
}

func TestBreak_TableRedefinition(t *testing.T) {
	input := []byte(`
anchor = 1
[anchor]
sub = 2
`)
	p := NewParser(input)
	_, err := p.Parse()
	if err == nil {
		t.Error("Parser failed to catch redefinition of a value as a table")
	}
}

func TestBreak_MalformedScientificNotation(t *testing.T) {
	tests := []string{
		"val = 1e",
		"val = 1e+",
		"val = .5",
		"val = 1.e2",
	}
	for _, tc := range tests {
		p := NewParser([]byte(tc))
		_, err := p.Parse()
		if err == nil {
			t.Errorf("Should have failed to parse malformed float: %s", tc)
		}
	}
}

func TestBreak_SliceTypeMismatch(t *testing.T) {
	data := map[string]any{
		"list": []any{1, "string", 3},
	}
	type Target struct {
		List []int `toml:"list"`
	}
	var tgt Target
	err := Decode(data, &tgt)
	if err == nil {
		t.Error("Decoder should have failed converting string to int inside slice")
	}
}

func TestBreak_InvalidDottedKeyInInlineTable(t *testing.T) {
	input := []byte(`config = { valid.123 = "fail" }`)
	p := NewParser(input)
	_, err := p.Parse()
	if err == nil {
		t.Error("Parser allowed numeric segment in dotted inline table key")
	}
}

func TestPanic_NilInterfaceAssignment(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Panic during nil interface decoding: %v", r)
		}
	}()
	var target any
	data := map[string]any{"a": 1}
	_ = Decode(data, &target)
}

func TestStructural_NestedReentry(t *testing.T) {
	input := []byte(`
[a.b.c]
depth = 3
[a]
root_val = 1
[a.b]
mid_val = 2
`)
	p := NewParser(input)
	res, err := p.Parse()
	if err != nil {
		t.Fatalf("Valid nested reentry failed: %v", err)
	}

	a := res["a"].(map[string]any)
	if a["root_val"] != 1 {
		t.Errorf("Missing root_val: %v", a["root_val"])
	}
	b := a["b"].(map[string]any)
	if b["mid_val"] != 2 {
		t.Errorf("Missing mid_val: %v", b["mid_val"])
	}
}

func TestBreak_KeyCollisionDotted(t *testing.T) {
	input := []byte(`
a.b = 1
[a.b]
c = 2
`)
	p := NewParser(input)
	_, err := p.Parse()
	if err == nil {
		t.Error("Should have failed: redefining scalar a.b as a table")
	}
}

func TestBreak_IntegerOverflow(t *testing.T) {
	input := []byte(`val = 9223372036854775808`)
	p := NewParser(input)
	_, err := p.Parse()
	if err == nil {
		t.Error("Parser should have errored on int64 overflow")
	}
}

func TestBreak_ArrayTableShadowing(t *testing.T) {
	input := []byte(`
[conflict]
sub = 1
[[conflict]]
sub = 2
`)
	p := NewParser(input)
	_, err := p.Parse()
	if err == nil {
		t.Error("Should have failed: [conflict] followed by [[conflict]]")
	}
}

func TestBreak_RecursiveDecoder(t *testing.T) {
	type Recursive struct {
		Next *Recursive `toml:"next"`
	}
	data := map[string]any{
		"next": map[string]any{
			"next": map[string]any{
				"next": map[string]any{},
			},
		},
	}
	var target Recursive
	err := Decode(data, &target)
	if err != nil {
		t.Fatalf("Recursive decode failed: %v", err)
	}
	if target.Next.Next.Next == nil {
		t.Error("Recursive decoding depth mismatch")
	}
}

func TestBreak_DottedKeyConflictWithTable(t *testing.T) {
	input := []byte(`
[a]
b.c = 1
[a.b]
c = 2
`)
	p := NewParser(input)
	_, err := p.Parse()
	if err == nil {
		t.Error("Should have failed: duplicate definition of a.b.c")
	}
}

func TestLexer_CommentEdgeCases(t *testing.T) {
	input := []byte(`
key = "value # not a comment" # this is a comment
# Empty line with comment
   # indented comment
[table] # comment after table
`)
	p := NewParser(input)
	res, err := p.Parse()
	if err != nil {
		t.Fatalf("Lexer failed on valid comments: %v", err)
	}
	if res["key"] != "value # not a comment" {
		t.Errorf("Comment in string was incorrectly truncated: %v", res["key"])
	}
}

func TestLexer_StrictNumericValidation(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []TokenType
	}{
		{
			"Multiple dots error",
			"1.1.1",
			[]TokenType{TokenError},
		},
		{
			"Octal is valid",
			"val = 0123",
			[]TokenType{TokenIdent, TokenEqual, TokenInteger, TokenEOF},
		},
		{
			"Negative with leading zero valid",
			"val = -01",
			[]TokenType{TokenIdent, TokenEqual, TokenInteger, TokenEOF},
		},
		{
			"Float then dot then int",
			"1e1.5",
			[]TokenType{TokenFloat, TokenDot, TokenInteger, TokenEOF},
		},
		{
			"Float then ident",
			"1e1e1",
			[]TokenType{TokenFloat, TokenIdent, TokenEOF},
		},
		{
			"Float then ident with dots",
			"1.00a00",
			[]TokenType{TokenFloat, TokenIdent, TokenEOF},
		},
		{
			"Incomplete exponent error",
			"val = 1e+",
			[]TokenType{TokenIdent, TokenEqual, TokenInteger, TokenIdent, TokenError},
		},
		{
			"Zero valid",
			"val = 0",
			[]TokenType{TokenIdent, TokenEqual, TokenInteger, TokenEOF},
		},
		{
			"Negative zero valid",
			"val = -0",
			[]TokenType{TokenIdent, TokenEqual, TokenInteger, TokenEOF},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLexer([]byte(tc.input))
			var got []TokenType
			for {
				tok := l.NextToken()
				got = append(got, tok.Type)
				if tok.Type == TokenEOF || tok.Type == TokenError {
					break
				}
			}
			if len(got) != len(tc.expected) {
				t.Errorf("[%s] token count: got %d %v, want %d %v", tc.input, len(got), got, len(tc.expected), tc.expected)
				return
			}
			for i, exp := range tc.expected {
				if got[i] != exp {
					t.Errorf("[%s] token[%d]: got %v, want %v", tc.input, i, got[i], exp)
				}
			}
		})
	}
}

func TestParser_KeyPathDeepExhaustion(t *testing.T) {
	input := []byte(`
a.b.c.d.e = 1
a.b.c.f = 2
[a.b.c]
g = 3
[a.b.c.d]
h = 4
[a.b]
i = 5
`)
	p := NewParser(input)
	res, err := p.Parse()
	if err != nil {
		t.Fatalf("Failed on complex but valid nested reentry: %v", err)
	}

	a := res["a"].(map[string]any)
	b := a["b"].(map[string]any)
	if b["i"] != 5 {
		t.Errorf("Value 'i' lost in table reentry. Got %v", b["i"])
	}
	if _, ok := b["c"].(map[string]any); !ok {
		t.Errorf("Sub-map 'c' lost during parent reentry")
	}
}

func TestParser_FloatParsingErrors(t *testing.T) {
	tests := []string{
		"f = .5",
		"f = 1.",
	}

	for _, tc := range tests {
		p := NewParser([]byte(tc))
		_, err := p.Parse()
		if err == nil {
			t.Errorf("Should have failed to parse: %s", tc)
		}
	}
}

func TestLexer_HexWithE(t *testing.T) {
	// 0xDEAD must be Integer, not misclassified as Float due to 'E'
	input := "val = 0xDEAD"
	l := NewLexer([]byte(input))

	tok := l.NextToken() // val
	if tok.Type != TokenIdent {
		t.Errorf("Expected Ident, got %v", tok.Type)
	}
	tok = l.NextToken() // =
	if tok.Type != TokenEqual {
		t.Errorf("Expected Equal, got %v", tok.Type)
	}
	tok = l.NextToken() // 0xDEAD
	if tok.Type != TokenInteger {
		t.Errorf("Expected Integer for hex, got %v (%s)", tok.Type, tok.Literal)
	}
	if tok.Literal != "0xDEAD" {
		t.Errorf("Literal mismatch: %q", tok.Literal)
	}
}

func TestLexer_IPAddressAndVersion(t *testing.T) {
	// IP-like or semver must error on multi-dot
	input := "version = 1.2.3"
	l := NewLexer([]byte(input))

	tok := l.NextToken() // version
	if tok.Type != TokenIdent {
		t.Errorf("Expected Ident, got %v", tok.Type)
	}
	tok = l.NextToken() // =
	if tok.Type != TokenEqual {
		t.Errorf("Expected Equal, got %v", tok.Type)
	}
	tok = l.NextToken() // 1.2.3 should error
	if tok.Type != TokenError {
		t.Errorf("Expected Error for multi-dot, got %v (%s)", tok.Type, tok.Literal)
	}
}

func TestParser_StrictNoNumericKeys(t *testing.T) {
	tests := []struct {
		input string
		name  string
	}{
		{`123 = "val"`, "Bare integer key"},
		{`[123]`, "Integer table header"},
		{`["456"]`, "Quoted integer key"},
		{`a.1.b = "val"`, "Numeric segment in dotted key"},
	}

	for _, tc := range tests {
		p := NewParser([]byte(tc.input))
		_, err := p.Parse()
		if err == nil {
			t.Errorf("Failed %s: should have rejected numeric key", tc.name)
		}
	}
}

func TestLexer_AmbiguousNumericDotted(t *testing.T) {
	tests := []struct {
		input    string
		expected []TokenType
	}{
		{"1.a", []TokenType{TokenInteger, TokenDot, TokenIdent, TokenEOF}},
		{"1.0.0", []TokenType{TokenError}}, // Multi-dot
		{"1e1.5", []TokenType{TokenFloat, TokenDot, TokenInteger, TokenEOF}},
	}

	for _, tc := range tests {
		l := NewLexer([]byte(tc.input))
		var got []TokenType
		for {
			tok := l.NextToken()
			got = append(got, tok.Type)
			if tok.Type == TokenEOF || tok.Type == TokenError {
				break
			}
		}
		if len(got) != len(tc.expected) {
			t.Errorf("%s: got %v, want %v", tc.input, got, tc.expected)
			continue
		}
		for i, exp := range tc.expected {
			if got[i] != exp {
				t.Errorf("%s[%d]: got %v, want %v", tc.input, i, got[i], exp)
			}
		}
	}
}

func TestParser_KeyValueContext(t *testing.T) {
	input := `key = 1.1`
	p := NewParser([]byte(input))
	_, err := p.Parse()
	if err != nil {
		t.Errorf("Valid float value failed: %v", err)
	}

	input2 := `1.1 = "value"`
	p2 := NewParser([]byte(input2))
	_, err = p2.Parse()
	if err == nil {
		t.Error("Float key should have been rejected")
	}
}
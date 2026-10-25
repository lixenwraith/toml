package toml

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Key is a dotted key path, one segment per part: [a."b.c"] is Key{"a", "b.c"}
type Key []string

// String spells the key as a document writes it, quoting segments that need it
func (k Key) String() string {
	parts := make([]string, len(k))
	for i, s := range k {
		if t, err := keyText(s); err == nil {
			parts[i] = t
		} else {
			var e encoder
			e.string(s)
			parts[i] = e.buf.String()
		}
	}
	return strings.Join(parts, ".")
}

// under reports k is p or a key inside it
func (k Key) under(p Key) bool { return len(k) >= len(p) && slices.Equal(k[:len(p)], p) }

// Document is a parsed document that keeps its bytes. An edit rewrites only the
// statements it names, so comments, blank lines, order and the spelling of every
// other value stay as written, and the result is parsed again before it stands.
type Document struct {
	data  []byte
	tree  map[string]any
	stmts []statement
}

// statement is a table header or a key and its value, as the lines it spans: from
// the start of its line through its newline, a comment after it on that line
// included. A key is its path relative to scope, the header it sits under.
type statement struct {
	start, end int
	scope, key Key
	header     bool
	array      bool   // an [[array]] header
	value      [2]int // a key's value bytes
}

func (s statement) path() Key {
	if s.header {
		return s.key
	}
	return append(slices.Clip(s.scope), s.key...)
}

// ParseDocument parses data as Parse does and keeps it for editing; an edit
// writes a new slice, never into data
func ParseDocument(data []byte) (*Document, error) {
	tree, err := NewParser(data).Parse()
	if err != nil {
		return nil, err
	}
	return &Document{data: data, tree: tree, stmts: statements(data)}, nil
}

// statements splits a document the parser accepted into its statements
func statements(data []byte) []statement {
	lx := NewLexer(data)
	t := lx.NextToken()
	key := func() Key {
		var k Key
		for {
			k = append(k, t.Literal)
			if t = lx.NextToken(); t.Type != TokenDot {
				return k
			}
			t = lx.NextToken()
		}
	}
	// lineEnd takes a trailing comment and the newline, leaving t past them
	lineEnd := func() int {
		if t.Type == TokenComment {
			t = lx.NextToken()
		}
		end := t.End
		if t.Type != TokenEOF {
			t = lx.NextToken()
		}
		return end
	}
	var (
		out   []statement
		scope Key
	)
	for t.Type != TokenEOF {
		s := statement{start: bytes.LastIndexByte(data[:t.Pos], '\n') + 1, scope: scope}
		switch t.Type {
		case TokenLBracket:
			s.header, t = true, lx.NextToken()
			if t.Type == TokenLBracket {
				s.array, t = true, lx.NextToken()
			}
			s.key = key()
			for t.Type == TokenRBracket {
				t = lx.NextToken()
			}
			s.scope, scope = nil, s.key
		case TokenIdent, TokenString:
			s.key = key()
			t = lx.NextToken() // past =
			s.value[0] = t.Pos
			for depth := 0; ; {
				switch t.Type {
				case TokenLBracket, TokenLBrace:
					depth++
				case TokenRBracket, TokenRBrace:
					depth--
				}
				if t.Type != TokenNewline && t.Type != TokenComment {
					s.value[1] = t.End
				}
				if t = lx.NextToken(); depth == 0 && (t.Type == TokenNewline || t.Type == TokenComment || t.Type == TokenEOF) {
					break
				}
			}
		default: // a blank or comment line
			t = lx.NextToken()
			continue
		}
		s.end = lineEnd()
		out = append(out, s)
	}
	return out
}

// Bytes is the document as it now reads; the caller must not change it
func (d *Document) Bytes() []byte { return d.data }

// Tree is the document's values as Parse returns them, which Decode reads
func (d *Document) Tree() map[string]any { return d.tree }

// Decode stores the document's values in v as Decode does
func (d *Document) Decode(v any) error { return Decode(d.tree, v) }

// Unmapped names the keys of the document that no field of v's type takes, as
// Decode, which ignores them, would match them; each once and sorted. A key under
// a map or interface destination is taken, and one under an array of tables is
// named without its index.
func (d *Document) Unmapped(v any) []Key {
	seen := make(map[string]Key)
	unmapped(d.tree, reflect.TypeOf(v), nil, seen, 0)
	out := make([]Key, 0, len(seen))
	for _, k := range seen {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b Key) int { return slices.Compare(a, b) })
	return out
}

func unmapped(data any, t reflect.Type, at Key, seen map[string]Key, depth int) {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || depth > maxValueDepth {
		return
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := data.(map[string]any)
		if !ok {
			return
		}
		fields := make(map[string]reflect.Type)
		for _, f := range fieldsOf(t) {
			fields[f.key] = t.Field(f.index).Type
		}
		for k, v := range m {
			key := append(slices.Clip(at), k)
			if ft, ok := fields[k]; ok {
				unmapped(v, ft, key, seen, depth+1)
			} else {
				seen[key.String()] = key
			}
		}
	case reflect.Map:
		if m, ok := data.(map[string]any); ok {
			for k, v := range m {
				unmapped(v, t.Elem(), append(slices.Clip(at), k), seen, depth+1)
			}
		}
	case reflect.Slice, reflect.Array:
		switch s := data.(type) {
		case []any:
			for _, v := range s {
				unmapped(v, t.Elem(), at, seen, depth+1)
			}
		case []map[string]any:
			for _, v := range s {
				unmapped(v, t.Elem(), at, seen, depth+1)
			}
		}
	}
}

// Delete removes key and every key under it: each statement setting one and each
// table header naming one, in every table of an array of tables, every other line
// kept. It reports whether anything was removed; a key inside an inline value, a
// table or array written as a value, cannot be edited and fails.
func (d *Document) Delete(key Key) (bool, error) {
	var out []byte
	at := 0
	for _, s := range d.stmts {
		p := s.path()
		if !s.header && len(p) < len(key) && key.under(p) && d.inline(s) {
			return false, fmt.Errorf("key %s is inside the value of %s", key, p)
		}
		if p.under(key) {
			out, at = append(out, d.data[at:s.start]...), s.end
		}
	}
	if at == 0 {
		return false, nil
	}
	return true, d.replace(append(out, d.data[at:]...))
}

// inline reports a key's value is an inline table or an array
func (d *Document) inline(s statement) bool {
	c := d.data[s.value[0]]
	return c == '{' || c == '['
}

// Set gives key the value v, encoded as Marshal writes a value. A key the document
// sets has its value replaced, the rest of its line kept; a key it lacks goes after
// the last key of its table, else after the table's header, else in a table
// appended to the document. A key under an array of tables or inside an inline
// value fails, and so does a value the document then would not parse with.
func (d *Document) Set(key Key, v any) error {
	if len(key) == 0 || v == nil {
		return fmt.Errorf("a key and a value are needed")
	}
	var e encoder
	if err := e.value(reflect.ValueOf(v), 0); err != nil {
		return fmt.Errorf("key %s: %w", key, err)
	}
	value := e.buf.Bytes()
	parent := key[:len(key)-1]
	after, scope := -1, Key(nil) // where a missing key goes, and the scope it is spelled in
	for _, s := range d.stmts {
		p := s.path()
		switch {
		case s.header && s.array && key.under(p):
			return fmt.Errorf("key %s is under the array of tables %s", key, p)
		case !s.header && slices.Equal(p, key):
			return d.replace(slices.Concat(d.data[:s.value[0]], value, d.data[s.value[1]:]))
		case !s.header && len(p) < len(key) && key.under(p):
			return fmt.Errorf("key %s is inside the value of %s", key, p)
		case p.under(key):
			return fmt.Errorf("key %s is a table", key)
		case s.header && slices.Equal(p, parent):
			after, scope = s.end, p
		case !s.header && len(p) > len(parent) && p.under(parent) && parent.under(s.scope):
			if !slices.Equal(scope, parent) || slices.Equal(s.scope, parent) {
				after, scope = s.end, s.scope
			}
		}
	}
	head := ""
	switch {
	case after >= 0:
	case len(parent) == 0: // a root key goes before the first header
		after = len(d.data)
		if i := slices.IndexFunc(d.stmts, func(s statement) bool { return s.header }); i >= 0 {
			after = d.stmts[i].start
		}
	default:
		after, scope, head = len(d.data), parent, "["+parent.String()+"]\n"
		if after > 0 {
			head = "\n" + head
		}
	}
	line := slices.Concat([]byte(head+key[len(scope):].String()+" = "), value, []byte("\n"))
	if after > 0 && d.data[after-1] != '\n' {
		line = slices.Concat([]byte("\n"), line)
	}
	return d.replace(slices.Concat(d.data[:after], line, d.data[after:]))
}

// replace makes data the document once it parses
func (d *Document) replace(data []byte) error {
	next, err := ParseDocument(data)
	if err != nil {
		return fmt.Errorf("edit refused: %w", err)
	}
	*d = *next
	return nil
}

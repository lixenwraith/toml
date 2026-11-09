package toml

import (
	"bytes"
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strconv"
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
// statements it names, with the comments that belong to them, so blank lines,
// order, every other comment and the spelling of every other value stay as
// written, and the result is parsed again before it stands.
type Document struct {
	data  []byte
	tree  map[string]any
	stmts []statement
	units []unit
	pre   int // where the comments heading the file end, 0 when none
	foot  int // where those closing it start, -1 when none
}

// statement is a table header or a key and its value, as the lines it spans: from
// the start of its line through its newline, a comment after it on that line
// included. lead is where the comment lines right above it, which are its own,
// start. A key is its path relative to scope, the header it sits under.
type statement struct {
	start, end, lead int
	scope, key       Key
	header           bool
	array            bool   // an [[array]] header
	value            [2]int // a key's value bytes
}

// unit is a run of statements no blank line parts, from the first comment line
// that belongs to the run through the last, whole lines: stmts[first:last]
type unit struct {
	start, end  int
	first, last int
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
	d := layout(data)
	d.tree = tree
	return &d, nil
}

// item is a statement, or a group of comment lines no blank line parts, with the
// blank lines between it and the item before it or the file's start
type item struct {
	start, end int
	stmt       int // its index in stmts, -1 for comments
	gap        int
}

// layout splits a document the parser accepted into statements and units and
// gives each comment group to the side fewer blank lines part it from, below on
// a tie
func layout(data []byte) Document {
	d := Document{data: data, foot: -1}
	items := d.scan()
	para := make([]int, len(items)) // a statement's unit
	open := false                   // no blank line since the last unit's last item
	for k, it := range items {
		open = open && it.gap == 0
		if it.stmt < 0 {
			continue
		}
		if !open {
			d.units = append(d.units, unit{start: it.start, first: it.stmt})
		}
		para[k], open = len(d.units)-1, true
		d.units[para[k]].end, d.units[para[k]].last = it.end, it.stmt+1
	}
	for i := 0; i < len(items); {
		if items[i].stmt >= 0 {
			i++
			continue
		}
		j := i
		for j < len(items) && items[j].stmt < 0 {
			j++
		}
		split := placeComments(items, i, j)
		for _, c := range items[i:split] {
			if i == 0 {
				d.pre = c.end
			} else {
				d.units[para[i-1]].end = max(d.units[para[i-1]].end, c.end)
			}
		}
		for _, c := range items[split:j] {
			if j < len(items) {
				d.units[para[j]].start = min(d.units[para[j]].start, c.start)
			} else if d.foot < 0 {
				d.foot = c.start
			}
		}
		if split < j && j < len(items) && items[j].gap == 0 {
			d.stmts[items[j].stmt].lead = items[j-1].start
		}
		i = j
	}
	return d
}

// placeComments splits the comment groups items[i:j] at the widest run of blank
// lines around them, the first of equals: those above it go with what is above,
// the rest with what is below. The file's start counts as no blank line and its
// end as half of one, so a comment touching a statement stays with it.
func placeComments(items []item, i, j int) int {
	split, widest := i, -1
	for k := i; k <= j; k++ {
		w := 1 // the file's end
		switch {
		case k == 0:
			w = 0
		case k < len(items):
			w = 2 * items[k].gap
		}
		if w > widest {
			split, widest = k, w
		}
	}
	return split
}

// scan lexes data into its items, appending d.stmts
func (d *Document) scan() (items []item) {
	lx := Lexer{input: d.data, line: 1, lean: true}
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
		scope Key
		blank int
	)
	for t.Type != TokenEOF {
		start := bytes.LastIndexByte(d.data[:t.Pos], '\n') + 1
		s := statement{start: start, lead: start, scope: scope}
		switch t.Type {
		case TokenNewline:
			blank++
			t = lx.NextToken()
			continue
		case TokenComment:
			end := lineEnd()
			if n := len(items); n > 0 && items[n-1].stmt < 0 && blank == 0 {
				items[n-1].end = end
			} else {
				items = append(items, item{start: start, end: end, stmt: -1, gap: blank})
			}
			blank = 0
			continue
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
		default: // a key
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
		}
		s.end = lineEnd()
		items = append(items, item{start: s.start, end: s.end, stmt: len(d.stmts), gap: blank})
		d.stmts = append(d.stmts, s)
		blank = 0
	}
	return items
}

// Bytes is the document as it now reads; the caller must not change it
func (d *Document) Bytes() []byte { return d.data }

// Tree is the document's values as Parse returns them, which Decode reads
func (d *Document) Tree() map[string]any { return d.tree }

// Decode stores the document's values in v as Decode does
func (d *Document) Decode(v any) error { return Decode(d.tree, v) }

// Unmapped names the keys of the document that no field of v's type takes, as
// the function Unmapped does
func (d *Document) Unmapped(v any) []Key { return Unmapped(d.tree, v) }

// UnknownKeys is the error of a caller that refuses the keys Unmapped names
type UnknownKeys []Key

func (u UnknownKeys) Error() string {
	q := make([]string, len(u))
	for i, k := range u {
		q[i] = strconv.Quote(k.String())
	}
	if len(q) == 1 {
		return "unknown key " + q[0]
	}
	return "unknown keys " + strings.Join(q, ", ")
}

// Unmapped names the keys of data, a value Parse produced, that no field of v's
// type takes, as Decode, which ignores them, would match them; each once and
// sorted. A key under a map or interface destination is taken, and one under an
// array of tables is named without its index.
func Unmapped(data any, v any) []Key {
	seen := make(map[string]Key)
	unmapped(data, reflect.TypeOf(v), nil, seen, 0)
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

// Delete removes key and every key under it, each statement setting one and each
// header naming one, in every table of an array of tables, with the comments that
// are theirs; a run of statements going whole takes its comments and one blank run
// beside it. It reports whether anything was removed; a key inside an inline value,
// a table or array written as a value, cannot be edited and fails.
func (d *Document) Delete(key Key) (bool, error) {
	gone := make([]bool, len(d.stmts))
	some := false
	for i, s := range d.stmts {
		p := s.path()
		if !s.header && len(p) < len(key) && key.under(p) && d.inline(s) {
			return false, fmt.Errorf("key %s is inside the value of %s", key, p)
		}
		gone[i] = p.under(key)
		some = some || gone[i]
	}
	if !some {
		return false, nil
	}
	whole := func(u unit) bool { return !slices.Contains(gone[u.first:u.last], false) }
	var out []byte
	at := 0
	cut := func(from, to int) { out, at = append(out, d.data[at:from]...), to }
	for k := 0; k < len(d.units); k++ {
		u := d.units[k]
		if !whole(u) {
			for i := u.first; i < u.last; i++ {
				if gone[i] {
					cut(d.stmts[i].lead, d.stmts[i].end)
				}
			}
			continue
		}
		j := k
		for j+1 < len(d.units) && whole(d.units[j+1]) {
			j++
		}
		switch {
		case j+1 < len(d.units):
			cut(u.start, d.units[j+1].start)
		case d.foot >= 0:
			cut(u.start, d.foot)
		case k > 0:
			cut(d.units[k-1].end, len(d.data))
		default:
			cut(d.pre, len(d.data))
		}
		k = j
	}
	return true, d.replace(append(out, d.data[at:]...))
}

// inline reports a key's value is an inline table or an array
func (d *Document) inline(s statement) bool {
	c := d.data[s.value[0]]
	return c == '{' || c == '['
}

// Set gives key the value v, encoded as Marshal writes one. A key the document sets
// has its value replaced, the rest of its line kept; one it lacks goes after the
// last key of its table, else after its header, else in a table appended above the
// file's closing comments, a first root key above the first table and its comments.
// A key under an array of tables or an inline value fails, as does an unparsable edit.
func (d *Document) Set(key Key, v any) error {
	if len(key) == 0 || v == nil {
		return fmt.Errorf("a key and a value are needed")
	}
	var e encoder
	if err := e.value(reflect.ValueOf(v), 0); err != nil {
		return fmt.Errorf("key %s: %w", key, err)
	}
	value := e.buf.Bytes()
	i, at, err := d.find(key)
	switch {
	case err != nil:
		return err
	case i >= 0:
		s := d.stmts[i]
		return d.replace(slices.Concat(d.data[:s.value[0]], value, d.data[s.value[1]:]))
	}
	line := slices.Concat([]byte(key[len(at.scope):].String()+" = "), value, []byte("\n"))
	return d.splice([]*insertion{{spot: at, lines: line}})
}

// spot is where lines the document lacks go: before the byte at, under head, a new
// table's header, and above foot; a key there is spelled from scope. A spot for a
// new table comes after any other at its byte.
type spot struct {
	at         int
	scope      Key
	head, foot string
	table      bool
}

// insertion is the lines an edit writes at a spot, each ending in a newline
type insertion struct {
	spot
	lines []byte
}

// find returns the statement setting key, or -1 and the spot Set puts it in
func (d *Document) find(key Key) (int, spot, error) {
	parent := key[:len(key)-1]
	at := spot{at: -1}
	for i, s := range d.stmts {
		p := s.path()
		switch {
		case s.header && s.array && key.under(p):
			return -1, at, fmt.Errorf("key %s is under the array of tables %s", key, p)
		case !s.header && slices.Equal(p, key):
			return i, at, nil
		case !s.header && len(p) < len(key) && key.under(p):
			return -1, at, fmt.Errorf("key %s is inside the value of %s", key, p)
		case p.under(key):
			return -1, at, fmt.Errorf("key %s is a table", key)
		case s.header && slices.Equal(p, parent):
			at.at, at.scope = s.end, p
		case !s.header && len(p) > len(parent) && p.under(parent) && parent.under(s.scope):
			if !slices.Equal(at.scope, parent) || slices.Equal(s.scope, parent) {
				at.at, at.scope = s.end, s.scope
			}
		}
	}
	switch {
	case at.at >= 0:
	case len(parent) == 0 && len(d.units) > 0: // a blank line keeps the table's comments its own
		at.at, at.foot = d.units[0].start, "\n"
	case len(parent) == 0:
		at = d.newTable()
		at.table = false
	default:
		at = d.newTable()
		at.scope, at.head = parent, at.head+"["+parent.String()+"]\n"
	}
	return -1, at, nil
}

// newTable is the spot a table the document lacks goes in: above the file's
// closing comments, else at its end, a blank line apart from what is beside it
func (d *Document) newTable() spot {
	if d.foot >= 0 {
		return spot{at: d.foot, foot: "\n", table: true}
	}
	return spot{at: len(d.data), head: "\n", table: true}
}

// splice writes each insertion at its spot, those at one byte in order, none
// opening the file with a blank line, and makes the result the document once it
// parses
func (d *Document) splice(ins []*insertion) error {
	order := func(in *insertion) int {
		if in.table {
			return 2*in.at + 1
		}
		return 2 * in.at
	}
	slices.SortStableFunc(ins, func(a, b *insertion) int { return cmp.Compare(order(a), order(b)) })
	var out []byte
	from := 0
	for _, in := range ins {
		out = append(out, d.data[from:in.at]...)
		if from = in.at; len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		head := in.head
		if len(out) == 0 {
			head = strings.TrimPrefix(head, "\n")
		}
		out = append(append(append(out, head...), in.lines...), in.foot...)
	}
	return d.replace(append(out, d.data[from:]...))
}

// Fill adds each key src sets that d does not, as src writes it with the comments
// that are its own there, and names them in src's order. A key goes after the one
// src sets before it in its table when d sets that, else after its table's header,
// else where Set puts it; a table d lacks comes as its header and that header's
// comments, where Set puts a new table. A value d holds at a key or over it, an
// inline table or an array of tables, stays d's; src's arrays of tables are left out.
func (d *Document) Fill(src *Document) ([]Key, error) {
	keys, headers := make(map[string]statement), make(map[string]statement)
	for _, s := range d.stmts {
		switch k := s.path().String(); {
		case !s.header:
			keys[k] = s
		case !s.array && headers[k].end == 0:
			headers[k] = s
		}
	}
	var (
		added, arrays []Key
		ins           []*insertion
	)
	past := make(map[int]*insertion)      // what goes past a statement of d, by its end
	placed := make(map[string]*insertion) // where each key Fill adds goes, by the key
	last := make(map[string]Key)          // the key src set last in each table, by the table
	open := func(at spot) *insertion {
		if in := past[at.at]; in != nil && !at.table {
			return in
		}
		in := &insertion{spot: at}
		if ins = append(ins, in); !at.table {
			past[at.at] = in
		}
		return in
	}
	for _, s := range src.stmts {
		p := s.path()
		if s.header && s.array {
			arrays = append(arrays, p)
		}
		if slices.ContainsFunc(arrays, func(a Key) bool { return p.under(a) }) {
			continue
		}
		name, table := p.String(), p[:len(p)-1]
		prev := last[table.String()].String()
		if !s.header {
			last[table.String()] = p
		}
		if placed[name] != nil || d.holds(p, keys) {
			continue
		}
		st, set := keys[prev]
		h, headed := headers[table.String()]
		var in *insertion
		switch {
		case s.header:
			at := d.newTable()
			at.scope = p
			in = open(at)
		case prev != "" && placed[prev] != nil:
			in = placed[prev]
		case prev != "" && set:
			in = open(spot{at: st.end, scope: st.scope})
		case placed[table.String()] != nil:
			in = placed[table.String()]
		case headed:
			in = open(spot{at: h.end, scope: table})
		default:
			_, at, err := d.find(p)
			if err != nil {
				return nil, err
			}
			if in = open(at); at.table {
				placed[table.String()] = in
			}
		}
		in.lines = append(in.lines, src.lines(s, p[len(in.scope):])...)
		if placed[name] = in; !s.header {
			added = append(added, p)
		}
	}
	if len(ins) == 0 {
		return nil, nil
	}
	if err := d.splice(ins); err != nil {
		return nil, err
	}
	return added, nil
}

// holds reports d has a value at key or over it: a key it sets, keys by their
// names, that key is or is inside, or a value other than a table at a prefix of key
func (d *Document) holds(key Key, keys map[string]statement) bool {
	m := d.tree
	for i, k := range key {
		if _, ok := keys[key[:i+1].String()]; ok {
			return true
		}
		v, ok := m[k]
		if !ok {
			return false
		}
		if m, ok = v.(map[string]any); !ok {
			return true
		}
	}
	return true
}

// lines is s as d writes it, from its own comments through its line, a key spelled
// as key, indented as it is
func (d *Document) lines(s statement, key Key) []byte {
	out := d.data[s.lead:s.end]
	if !s.header && !slices.Equal(key, s.key) {
		line := d.data[s.start:s.end]
		indent := line[:len(line)-len(bytes.TrimLeft(line, " \t"))]
		out = slices.Concat(d.data[s.lead:s.start], indent, []byte(key.String()+" = "), d.data[s.value[0]:s.end])
	}
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = slices.Concat(out, []byte("\n"))
	}
	return out
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

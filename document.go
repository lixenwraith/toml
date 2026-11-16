package toml

import (
	"bytes"
	"cmp"
	"fmt"
	"maps"
	"math"
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

// Document is a parsed document that keeps its bytes. An edit rewrites only what it
// changes, a statement or the part of a value that reads otherwise, with the
// comments that belong to it, so blank lines, order, every other comment and the
// spelling of every other value stay as written, and the result is parsed again
// before it stands.
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
	unmapped(data, reflect.TypeOf(v), make(Key, 0, 16), seen, 0)
	out := make([]Key, 0, len(seen))
	for _, k := range seen {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b Key) int { return slices.Compare(a, b) })
	return out
}

// unmapped walks data beside t. at is the path so far, one array the whole
// walk appends to, so a key is copied only when it is named.
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
		fields := fieldsOf(t)
		for k, v := range m {
			key := append(at, k)
			i := len(fields) - 1
			for i >= 0 && fields[i].key != k {
				i--
			}
			if i >= 0 {
				unmapped(v, fields[i].typ, key, seen, depth+1)
			} else {
				seen[key.String()] = slices.Clone(key)
			}
		}
	case reflect.Map:
		if m, ok := data.(map[string]any); ok {
			for k, v := range m {
				unmapped(v, t.Elem(), append(at, k), seen, depth+1)
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
// beside it. A key inside an inline table goes from it, and from every table of an
// array it is reached through. It reports whether anything was removed.
func (d *Document) Delete(key Key) (bool, error) {
	was := *d
	gone := make([]bool, len(d.stmts))
	some := false
	for i, s := range d.stmts {
		gone[i] = s.path().under(key)
		some = some || gone[i]
	}
	if some {
		if err := d.drop(gone); err != nil {
			return false, err
		}
	}
	inside, err := d.dropInside(key)
	if err != nil {
		*d = was
		return false, err
	}
	return some || inside, nil
}

// dropInside removes key from each value written inline that holds it, and reports
// whether one did
func (d *Document) dropInside(key Key) (bool, error) {
	var out []byte
	at := 0
	for _, s := range d.stmts {
		p := s.path()
		if s.header || len(p) >= len(key) || !key.under(p) || !d.inline(s) {
			continue
		}
		old, err := d.valueOf(s)
		if err != nil {
			return false, err
		}
		next, ok := without(old, key[len(p):])
		if !ok {
			continue
		}
		sh := d.shapeOf(s.value[0])
		text, err := d.rewrite(&sh, old, next)
		if err != nil {
			return false, err
		}
		out, at = append(append(out, d.data[at:sh.from]...), text...), sh.to
	}
	if at == 0 {
		return false, nil
	}
	return true, d.replace(append(out, d.data[at:]...))
}

// drop cuts the statements gone marks, each with the comments that are its own, a
// run going whole with its comments and one blank run beside it
func (d *Document) drop(gone []bool) error {
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
	return d.replace(append(out, d.data[at:]...))
}

// Comments gives, for each [[key]] header in the document's order, the lines above
// it that Delete(key) takes as that table's own: its comments as written, each run
// of blank lines among or under them cut to one, nil where it has none. Written
// under a blank line and above the table's header they are that table's again. A
// key that is not an array of tables written with headers gives nil.
func (d *Document) Comments(key Key) [][]byte {
	var out [][]byte
	k := 0
	for i, s := range d.stmts {
		if !s.header || !s.array || !slices.Equal(s.key, key) {
			continue
		}
		for d.units[k].last <= i {
			k++
		}
		// a run this header opens goes whole when its headers do: a key is under its header
		from := s.lead
		if u := d.units[k]; u.first == i && !slices.ContainsFunc(d.stmts[i:u.last], func(s statement) bool { return s.header && !s.key.under(key) }) {
			from = u.start
		}
		var own []byte
		for b, blank := d.data[from:s.start], false; len(b) > 0; {
			n := bytes.IndexByte(b, '\n') + 1
			if empty := len(bytes.TrimSpace(b[:n])) == 0; !empty || !blank {
				own = append(own, b[:n]...)
				blank = empty
			}
			b = b[n:]
		}
		out = append(out, own)
	}
	return out
}

// inline reports a key's value is an inline table or an array
func (d *Document) inline(s statement) bool {
	c := d.data[s.value[0]]
	return c == '{' || c == '['
}

// Set gives key the value v, written as Marshal writes one where it is new. What
// the document holds there and reads the same stays as written, down to a key of an
// inline table, an element of an array and a statement of a table or an array of
// tables under headers. A key it lacks goes where its table's keys are, or inside
// the inline table it is reached through; one under an array of tables fails.
func (d *Document) Set(key Key, v any) error {
	if len(key) == 0 || v == nil {
		return fmt.Errorf("a key and a value are needed")
	}
	text, err := spell(v)
	if err == nil {
		v, err = parsed(text)
	}
	if err != nil {
		return fmt.Errorf("key %s: %w", key, err)
	}
	was := *d
	if pl, _ := d.find(nil, key); pl.table || pl.tables {
		// tables under headers are read from the tree, which a caller may have changed
		err = d.replace(d.data)
	}
	if err == nil {
		err = d.set(nil, key, v)
	}
	if err != nil {
		*d = was
	}
	return err
}

// set gives key within w the value v, as Parse reads one
func (d *Document) set(w where, key Key, v any) error {
	pl, err := d.find(w, key)
	switch {
	case err != nil:
		return err
	case pl.stmt >= 0:
		return d.setValue(pl.stmt, key, v)
	case pl.tables:
		return d.setTables(w, key, v)
	case pl.table:
		return d.setTable(w, key, v)
	}
	text, err := spell(v)
	if err != nil {
		return fmt.Errorf("key %s: %w", key, err)
	}
	line := slices.Concat([]byte(key[len(pl.scope):].String()+" = "), text, []byte("\n"))
	return d.splice([]*insertion{{spot: pl.spot, lines: line}})
}

// setValue gives key, which the statement at i sets or holds inside its value, the
// value v: only the part that reads otherwise is written again
func (d *Document) setValue(i int, key Key, v any) error {
	s := d.stmts[i]
	p := s.path()
	old, err := d.valueOf(s)
	if err != nil {
		return err
	}
	if len(key) > len(p) {
		var ok bool
		if v, ok = with(old, key[len(p):], v); !ok {
			return fmt.Errorf("key %s is inside the value of %s", key, p)
		}
	}
	sh := d.shapeOf(s.value[0])
	text, err := d.rewrite(&sh, old, v)
	switch {
	case err != nil:
		return fmt.Errorf("key %s: %w", key, err)
	case bytes.Equal(text, d.data[sh.from:sh.to]):
		return nil
	}
	return d.replace(slices.Concat(d.data[:sh.from], text, d.data[sh.to:]))
}

// setTable gives the table key, written under headers or dotted keys, the keys of
// v: each is set in turn, and one v lacks is deleted
func (d *Document) setTable(w where, key Key, v any) error {
	now, ok := v.(map[string]any)
	old, _ := d.read(w, key)
	was, held := old.(map[string]any)
	if !ok || !held {
		return fmt.Errorf("key %s is a table", key)
	}
	for _, k := range slices.Sorted(maps.Keys(was)) {
		if _, keep := now[k]; keep {
			continue
		}
		_, lo, hi := d.span(w)
		gone, under := make([]bool, len(d.stmts)), append(slices.Clip(key), k)
		for i := lo; i < hi; i++ {
			gone[i] = d.stmts[i].path().under(under)
		}
		if err := d.drop(gone); err != nil {
			return err
		}
	}
	names, err := order(now)
	if err != nil {
		return err
	}
	for _, k := range names {
		if err := d.set(w, append(slices.Clip(key), k), now[k]); err != nil {
			return err
		}
	}
	if _, held := d.read(w, key); held {
		return nil
	}
	return d.set(w, key, v) // its dotted keys all gone, it is written again, empty
}

// setTables gives the array of tables key, written with headers, the tables of v:
// one that reads the same keeps every byte, one changed has its keys set, one gone
// goes with the comments that are its own, and one new is written as Marshal
// writes a table, below the table before it
func (d *Document) setTables(w where, key Key, v any) error {
	old, _ := d.read(w, key)
	was, _ := elements(old)
	now, ok := elements(v)
	_, headed := old.([]map[string]any)
	if !headed || !ok || len(now) == 0 || slices.ContainsFunc(now, func(e any) bool { _, table := e.(map[string]any); return !table }) {
		return fmt.Errorf("key %s is an array of tables", key)
	}
	steps := align(was, now)
	at := func(n int) where { return append(slices.Clip(w), nth{key, n}) }
	for _, st := range slices.Backward(steps) { // last first, so an index names one table throughout
		if st.j >= 0 {
			continue
		}
		_, lo, hi := d.span(at(st.i))
		gone := make([]bool, len(d.stmts))
		for i := lo; i < hi; i++ {
			gone[i] = d.stmts[i].path().under(key)
		}
		if err := d.drop(gone); err != nil {
			return err
		}
	}
	n := 0 // the tables kept so far, which is the next one's place among them
	for _, st := range steps {
		if st.i < 0 || st.j < 0 {
			continue
		}
		if !same(was[st.i], now[st.j]) {
			if err := d.setTable(at(n), key, now[st.j]); err != nil {
				return err
			}
		}
		n++
	}
	var ins []*insertion
	n = 0
	for k := 0; k < len(steps); k++ {
		if steps[k].i >= 0 {
			if steps[k].j >= 0 {
				n++
			}
			continue
		}
		var e encoder // the new tables up to the next one kept
		for ; k < len(steps) && (steps[k].i < 0 || steps[k].j < 0); k++ {
			if steps[k].i >= 0 {
				continue
			}
			e.header(key.String(), "[[", "]]")
			if err := e.table(reflect.ValueOf(now[steps[k].j]), key.String(), 0); err != nil {
				return fmt.Errorf("key %s: %w", key, err)
			}
		}
		k--
		ins = append(ins, &insertion{spot: d.beside(at(max(n-1, 0)), n == 0), lines: e.buf.Bytes()})
	}
	if len(ins) == 0 {
		return nil
	}
	return d.splice(ins)
}

// beside is the spot a new table goes in by the table of an array of tables w
// names: above it and the comments that are its own, or below its last statement
func (d *Document) beside(w where, above bool) spot {
	own, lo, hi := d.span(w)
	if above {
		return d.above(own)
	}
	last := own
	for i := lo; i < hi; i++ {
		if d.stmts[i].path().under(w[len(w)-1].of) {
			last = i
		}
	}
	if sp, ok := d.after(last); ok {
		return sp
	}
	return d.apart(d.stmts[last].end)
}

// nth names a table of an array of tables written under headers, and where the
// tables an edit is narrowed to, outermost first
type (
	nth struct {
		of Key
		n  int
	}
	where []nth
)

// span is the statements w leaves an edit: all of them, or those from the header
// of the table it names, own, to the next header of that array
func (d *Document) span(w where) (own, lo, hi int) {
	own, lo, hi = -1, 0, len(d.stmts)
	for _, t := range w {
		n, end := 0, hi
		own = hi
		for i := lo; i < hi; i++ {
			if s := d.stmts[i]; !s.header || !s.array || !slices.Equal(s.key, t.of) {
				continue
			}
			if n == t.n {
				own = i
			} else if n > t.n {
				end = i
				break
			}
			n++
		}
		lo, hi = own, end
	}
	return own, lo, hi
}

// read is what key holds, each array of tables w names read at its table
func (d *Document) read(w where, key Key) (any, bool) {
	var cur any = d.tree
	for i, k := range key {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[k]; !ok {
			return nil, false
		}
		if len(w) > 0 && slices.Equal(w[0].of, key[:i+1]) {
			a, ok := cur.([]map[string]any)
			if !ok || w[0].n >= len(a) {
				return nil, false
			}
			cur, w = a[w[0].n], w[1:]
		}
	}
	return cur, true
}

// valueOf is what the value the statement sets reads as
func (d *Document) valueOf(s statement) (any, error) {
	return parsed(d.data[s.value[0]:s.value[1]])
}

// parsed is what Parse reads a value's text as
func parsed(text []byte) (any, error) {
	tree, err := NewParser(slices.Concat([]byte("v = "), text)).Parse()
	if err != nil {
		return nil, err
	}
	return tree["v"], nil
}

// spell is v as Marshal writes a value
func spell(v any) ([]byte, error) {
	var e encoder
	if err := e.value(reflect.ValueOf(v), 0); err != nil {
		return nil, err
	}
	return e.buf.Bytes(), nil
}

// order is a table's keys in the order Marshal writes them
func order(m map[string]any) ([]string, error) {
	fields, err := entries(reflect.ValueOf(m))
	if err != nil {
		return nil, err
	}
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = f.name
	}
	return names, nil
}

// lookup is what key holds under the table v
func lookup(v any, key Key) (any, bool) {
	for _, k := range key {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[k]; !ok {
			return nil, false
		}
	}
	return v, true
}

// with is the table t holding v at key, copied down the path; false where the
// path meets a value that is not a table
func with(t any, key Key, v any) (any, bool) {
	m, ok := t.(map[string]any)
	if !ok {
		return nil, false
	}
	m = maps.Clone(m)
	if len(key) > 1 {
		sub, held := m[key[0]]
		if !held {
			sub = map[string]any{}
		}
		if v, ok = with(sub, key[1:], v); !ok {
			return nil, false
		}
	}
	m[key[0]] = v
	return m, true
}

// without is v less key, copied down the path: a table's key, or that key of every
// table of an array; false when v holds no such key
func without(v any, key Key) (any, bool) {
	switch x := v.(type) {
	case map[string]any:
		sub, ok := x[key[0]]
		if !ok {
			return v, false
		}
		m := maps.Clone(x)
		if len(key) == 1 {
			delete(m, key[0])
			return m, true
		}
		if sub, ok = without(sub, key[1:]); !ok {
			return v, false
		}
		m[key[0]] = sub
		return m, true
	case []any:
		out, some := slices.Clone(x), false
		for i, e := range x {
			if next, ok := without(e, key); ok {
				out[i], some = next, true
			}
		}
		return out, some
	}
	return v, false
}

// same reports two values Parse gives read alike: a float by its bits, an array of
// tables as the array of its tables
func same(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if w, ok := y[k]; !ok || !same(v, w) {
				return false
			}
		}
		return true
	case float64:
		y, ok := b.(float64)
		return ok && math.Float64bits(x) == math.Float64bits(y)
	}
	x, array := elements(a)
	y, other := elements(b)
	if array || other {
		return array && other && slices.EqualFunc(x, y, same)
	}
	return a == b
}

// elements is an array's values, an array of tables' its tables
func elements(v any) ([]any, bool) {
	switch x := v.(type) {
	case []any:
		return x, true
	case []map[string]any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// shape is a value as it is written: its bytes, and the parts of an array or an
// inline table. head ends what follows the opening bracket on its line, and the
// bytes from the last part's end to the closing bracket are the lines above that.
type shape struct {
	from, to, head int
	open           byte // [ or {, 0 for a scalar
	parts          []part
}

// part is an element of an array or a key of an inline table with the bytes that
// go where it goes: from lead, the lines above it or the blanks before it on its
// line, through end, the rest of its line where another line follows
type part struct {
	lead, from, to, end int
	comma               int // the comma after it, -1 for none
	key                 Key // an inline table's
	value               shape
}

// after is where what follows the part on its line starts: past its comma, or
// its value
func (p *part) after() int { return max(p.comma+1, p.to) }

// shapeOf reads the shape of the value written at from
func (d *Document) shapeOf(from int) shape {
	c := cursor{lx: Lexer{input: d.data, pos: from, line: 1, lean: true}}
	c.next()
	return c.shape()
}

// shape reads the value at the cursor, leaving it past the value
func (c *cursor) shape() shape {
	s := shape{from: c.t.Pos, to: c.t.End, head: c.t.End}
	closer := TokenRBracket
	switch c.t.Type {
	case TokenLBracket:
		s.open = '['
	case TokenLBrace:
		s.open, closer = '{', TokenRBrace
	default:
		c.next()
		return s
	}
	if c.next(); c.t.Type == TokenComment {
		c.next()
	}
	if c.t.Type == TokenNewline {
		s.head = c.t.End
		c.next()
	}
	for at := s.head; ; {
		for c.t.Type == TokenNewline || c.t.Type == TokenComment {
			c.next()
		}
		if c.t.Type == closer {
			break
		}
		p := part{lead: at, from: c.t.Pos, comma: -1}
		if s.open == '{' {
			for p.key = append(p.key, c.t.Literal); ; p.key = append(p.key, c.t.Literal) {
				if c.next(); c.t.Type != TokenDot {
					break
				}
				c.next()
			}
			c.next() // past =
		}
		p.value = c.shape()
		p.to, p.end = p.value.to, p.value.to
		line := -1 // where the line the value ends on ends
		for ; c.t.Type == TokenNewline || c.t.Type == TokenComment; c.next() {
			if c.t.Type == TokenNewline && line < 0 {
				line = c.t.End
			}
		}
		switch {
		case c.t.Type == TokenComma:
			p.comma, p.end = c.t.Pos, c.t.End
			if c.next(); c.t.Type == TokenComment {
				c.next()
			}
			if c.t.Type == TokenNewline {
				p.end = c.t.End
				c.next()
			}
		case line >= 0:
			p.end = line
		}
		s.parts, at = append(s.parts, p), p.end
	}
	s.to = c.t.End
	c.next()
	return s
}

// rewrite is the value s written again to read v where it read old: what reads the
// same keeps its bytes, an inline table its other keys and an array its other
// elements, and the rest is written as Marshal writes a value
func (d *Document) rewrite(s *shape, old, v any) ([]byte, error) {
	if same(old, v) {
		return d.data[s.from:s.to], nil
	}
	switch now := v.(type) {
	case map[string]any:
		if was, ok := old.(map[string]any); ok && s.open == '{' {
			return d.retable(s, was, now)
		}
	case []any:
		if was, ok := old.([]any); ok && s.open == '[' && len(was) == len(s.parts) {
			return d.rearray(s, was, now)
		}
	}
	return spell(v)
}

// slot is a part of a container as it is written again: the part at i kept with
// its value now reading text, or gone; i is -1 for one added, text all of it
type slot struct {
	i    int
	gone bool
	text []byte
}

// retable is the inline table s written again to read as now: a key it sets keeps
// its place and what reads the same in it, one now lacks goes with the lines that
// are its own, and one only now has is added last
func (d *Document) retable(s *shape, was, now map[string]any) ([]byte, error) {
	var kept tables
	slots := make([]slot, len(s.parts))
	for i := range s.parts {
		p := &s.parts[i]
		v, ok := lookup(now, p.key)
		if slots[i] = (slot{i: i, gone: !ok}); !ok {
			continue
		}
		old, _ := lookup(was, p.key)
		text, err := d.rewrite(&p.value, old, v)
		if err != nil {
			return nil, err
		}
		slots[i].text, kept.at(p.key).headed = text, true
	}
	err := missing(&kept, nil, now, func(key Key, v any) error {
		text, err := spell(v)
		slots = append(slots, slot{i: -1, text: slices.Concat([]byte(key.String()+" = "), text)})
		return err
	})
	return d.emit(s, slots), err
}

// missing gives add each key of the table v that no part kept sets, in the order
// Marshal writes a table's keys; a table whose keys the parts set dotted is walked
func missing(kept *tables, at Key, v map[string]any, add func(Key, any) error) error {
	names, err := order(v)
	if err != nil {
		return err
	}
	for _, k := range names {
		key := append(slices.Clip(at), k)
		sub, table := v[k].(map[string]any)
		switch c := kept.sub[k]; {
		case c != nil && c.headed:
		case c != nil && table:
			err = missing(c, key, sub, add)
		default:
			err = add(key, v[k])
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// rearray is the array s written again to read as now: an element that reads the
// same keeps its bytes and the lines that are its own, one changed is written
// again in its place, and one gone takes its lines with it
func (d *Document) rearray(s *shape, was, now []any) ([]byte, error) {
	steps := align(was, now)
	slots := make([]slot, 0, len(steps))
	for _, st := range steps {
		sl := slot{i: st.i, gone: st.j < 0}
		var err error
		switch {
		case sl.gone:
		case st.i < 0:
			sl.text, err = spell(now[st.j])
		default:
			sl.text, err = d.rewrite(&s.parts[st.i].value, was[st.i], now[st.j])
		}
		if err != nil {
			return nil, err
		}
		slots = append(slots, sl)
	}
	return d.emit(s, slots), nil
}

// step pairs an element of an array as it was, at i, with one as it is to be, at
// j: -1 for one only the other has
type step struct{ i, j int }

// maxAlign bounds the elements align compares each with each
const maxAlign = 1 << 12

// align pairs the elements of was and now in order: those that read alike, then
// between them the tables that are kin, each by the longest run of them where
// the arrays are small, and what is left by its place
func align(was, now []any) []step {
	steps := make([]step, 0, max(len(was), len(now)))
	var pair func(i, a, j, b int, alike []func(x, y any) bool)
	pair = func(i, a, j, b int, alike []func(x, y any) bool) {
		if len(alike) == 0 {
			for ; i < a && j < b; i, j = i+1, j+1 {
				steps = append(steps, step{i, j})
			}
			for ; i < a; i++ {
				steps = append(steps, step{i, -1})
			}
			for ; j < b; j++ {
				steps = append(steps, step{-1, j})
			}
			return
		}
		is := alike[0]
		for ; i < a && j < b && is(was[i], now[j]); i, j = i+1, j+1 {
			steps = append(steps, step{i, j})
		}
		end := 0 // the pairs alike that close the run
		for a-end > i && b-end > j && is(was[a-end-1], now[b-end-1]) {
			end++
		}
		if x, y := was[i:a-end], now[j:b-end]; len(x) > 0 && len(y) > 0 && len(x)*len(y) <= maxAlign {
			w := len(y) + 1
			run := make([]int, (len(x)+1)*w) // the most pairs alike from each pair on
			for p := len(x) - 1; p >= 0; p-- {
				for q := len(y) - 1; q >= 0; q-- {
					if run[p*w+q] = max(run[(p+1)*w+q], run[p*w+q+1]); is(x[p], y[q]) {
						run[p*w+q] = run[(p+1)*w+q+1] + 1
					}
				}
			}
			for p, q, i0, j0 := 0, 0, i, j; p < len(x) && q < len(y); {
				switch {
				case is(x[p], y[q]):
					pair(i, i0+p, j, j0+q, alike[1:])
					steps = append(steps, step{i0 + p, j0 + q})
					i, j, p, q = i0+p+1, j0+q+1, p+1, q+1
				case run[(p+1)*w+q] >= run[p*w+q+1]:
					p++
				default:
					q++
				}
			}
		}
		pair(i, a-end, j, b-end, alike[1:])
		for ; end > 0; end-- {
			steps = append(steps, step{a - end, b - end})
		}
	}
	pair(0, len(was), 0, len(now), []func(x, y any) bool{same, kin})
	return steps
}

// kin reports two tables one of which was likely edited into the other: half the
// keys of the larger, or more, read the same in both
func kin(a, b any) bool {
	x, ok := a.(map[string]any)
	y, other := b.(map[string]any)
	if !ok || !other {
		return false
	}
	n := 0
	for k, v := range x {
		if w, ok := y[k]; ok && same(v, w) {
			n++
		}
	}
	return n > 0 && 2*n >= max(len(x), len(y))
}

// emit writes the container s with slots for its parts. A part keeps its lines;
// what shared a line with one gone takes its place there; one added stands as the
// part before it does, on a line of its own or beside it.
func (d *Document) emit(s *shape, slots []slot) []byte {
	n := len(s.parts)
	lead, post, own := make([][]byte, n), make([][]byte, n), make([]bool, n)
	lined := s.head > s.from+1 // the last part read ended its line, or the bracket did
	for i := range s.parts {
		p := &s.parts[i]
		lead[i], post[i] = d.data[p.lead:p.from], d.data[p.after():p.end]
		own[i], lined = lined && len(post[i]) > 0, len(post[i]) > 0
	}
	for _, sl := range slots {
		if i := sl.i; !sl.gone || i < 0 {
			continue
		} else if len(post[i]) == 0 && i+1 < n {
			lead[i+1] = lead[i]
		}
	}
	for k := len(slots) - 1; k >= 0; k-- {
		if i := slots[k].i; slots[k].gone && i > 0 && len(post[i]) > 0 && len(post[i-1]) == 0 {
			post[i-1] = post[i]
		}
	}
	tail := d.data[s.head : s.to-1]
	if n > 0 {
		tail = d.data[s.parts[n-1].end : s.to-1]
	}
	// lines says a part added goes on a line of its own, after indent and before eol
	var indent, eol []byte
	lines := false
	model := func(lead, end []byte) {
		indent, eol, lines = lead[bytes.LastIndexByte(lead, '\n')+1:], []byte("\n"), true
		if bytes.HasSuffix(end, []byte("\r\n")) {
			eol = []byte("\r\n")
		}
	}
	blank := len(bytes.TrimSpace(tail)) == 0
	if n == 0 && s.head > s.from+1 {
		if model(tail[:len(tail)-len(bytes.TrimLeft(tail, " \t"))], d.data[:s.head]); blank {
			indent = slices.Concat(tail, []byte("\t"))
		}
	}
	type cell struct{ lead, body, post []byte }
	cells := make([]cell, 0, len(slots))
	for k, sl := range slots {
		if i := sl.i; i >= 0 {
			if lines = own[i]; lines {
				model(d.data[s.parts[i].lead:s.parts[i].from], d.data[:s.parts[i].end])
			}
			if p := &s.parts[i]; !sl.gone {
				body := slices.Concat(d.data[p.from:p.value.from], sl.text, d.data[p.to:max(p.comma, p.to)])
				cells = append(cells, cell{lead[i], body, post[i]})
			}
			continue
		}
		next := -1 // the part kept after one added first
		if len(cells) == 0 {
			for _, later := range slots[k:] {
				if later.i >= 0 && !later.gone {
					next = later.i
					break
				}
			}
		}
		if next >= 0 && own[next] {
			model(d.data[s.parts[next].lead:s.parts[next].from], d.data[:s.parts[next].end])
		}
		c := cell{body: sl.text}
		switch {
		case lines:
			c.lead, c.post = indent, eol
		case len(cells) > 0: // it takes the rest of the line of the cell before it
			last := &cells[len(cells)-1]
			c.lead, c.post, last.post = []byte(" "), last.post, nil
		case next >= 0: // it takes the place of the part after it, which follows it
			c.lead, lead[next] = lead[next], []byte(" ")
		case s.open == '{':
			c.lead = []byte(" ")
		}
		cells = append(cells, c)
	}
	if n == 0 && !lines || len(cells) == 0 && blank && s.head == s.from+1 {
		if tail = nil; s.open == '{' && len(cells) > 0 {
			tail = []byte(" ")
		}
	}
	trailing := n > 0 && s.parts[n-1].comma >= 0
	out := slices.Clone(d.data[s.from:s.head])
	for k, c := range cells {
		out = append(append(out, c.lead...), c.body...)
		if k < len(cells)-1 || trailing {
			out = append(out, ',')
		}
		out = append(out, c.post...)
	}
	return append(append(out, tail...), d.data[s.to-1])
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

// place is where key stands for an edit: the statement that sets it or holds it
// inside its value, tables written under headers, or the spot one the document
// lacks goes in
type place struct {
	spot
	stmt          int  // -1 for none
	table, tables bool // key names a table, or an array of tables, under headers
}

// find is key's place among the statements of w
func (d *Document) find(w where, key Key) (place, error) {
	own, lo, hi := d.span(w)
	parent := key[:len(key)-1]
	pl := place{spot: spot{at: -1}, stmt: -1}
	last := -1 // the last statement of the table w names
	for i := lo; i < hi; i++ {
		s := d.stmts[i]
		p := s.path()
		if len(w) > 0 && p.under(w[len(w)-1].of) {
			last = i
		}
		switch {
		case s.header && s.array && i != own && key.under(p):
			if len(p) < len(key) {
				return pl, fmt.Errorf("key %s is under the array of tables %s", key, p)
			}
			pl.tables = true
		case !s.header && key.under(p):
			pl.stmt = i
			return pl, nil
		case p.under(key):
			pl.table = true
		case s.header && slices.Equal(p, parent):
			pl.at, pl.scope = s.end, p
		case !s.header && len(p) > len(parent) && p.under(parent) && parent.under(s.scope):
			if !slices.Equal(pl.scope, parent) || slices.Equal(s.scope, parent) {
				pl.at, pl.scope = s.end, s.scope
			}
		}
	}
	switch {
	case pl.at >= 0:
	case last >= 0: // a table the one w names lacks goes below its last statement
		if sp, ok := d.after(last); ok {
			pl.spot = sp
		} else {
			pl.spot = d.apart(d.stmts[last].end)
		}
		pl.scope, pl.head = parent, pl.head+"["+parent.String()+"]\n"
	case len(parent) == 0 && len(d.units) > 0: // a blank line keeps the table's comments its own
		pl.at, pl.foot = d.units[0].start, "\n"
	case len(parent) == 0:
		pl.spot = d.newTable()
		pl.spot.table = false
	default:
		pl.spot = d.newTable()
		pl.scope, pl.head = parent, pl.head+"["+parent.String()+"]\n"
	}
	return pl, nil
}

// newTable is the spot a table the document lacks goes in: above the file's
// closing comments, else at its end, a blank line apart from what is beside it
func (d *Document) newTable() spot {
	if d.foot >= 0 {
		return spot{at: d.foot, foot: "\n", table: true}
	}
	return spot{at: len(d.data), head: "\n", table: true}
}

// apart is the spot a new table goes in at at, a line start: a blank line apart
// from the lines above and below unless one already is
func (d *Document) apart(at int) spot {
	sp := spot{at: at, table: true}
	if above := d.data[:max(at-1, 0)]; at > 0 && len(bytes.TrimSpace(d.data[bytes.LastIndexByte(above, '\n')+1:at])) > 0 {
		sp.head = "\n"
	}
	if line, _, _ := bytes.Cut(d.data[at:], []byte("\n")); len(bytes.TrimSpace(line)) > 0 {
		sp.foot = "\n"
	}
	return sp
}

// head is a table src heads, by its name, as Fill places one d lacks: the last
// statement d sets under it and the first header d has for a table src heads after
// it, -1 for none
type head struct {
	name       string
	last, next int
}

// heads is the tables src heads in its order, arrays of tables and what is under
// one left out, read against d in one walk of each
func (d *Document) heads(src *Document) []head {
	type node struct {
		sub          map[string]*node
		last, header int
	}
	root := &node{}
	var (
		out   []head
		nodes []*node
		under tables
	)
	for _, s := range src.stmts {
		if s.header && s.array {
			under.at(s.key).headed = true
		}
		if !s.header || under.headedOver(s.key) {
			continue
		}
		n := root
		for _, k := range s.key {
			c := n.sub[k]
			if c == nil {
				c = &node{last: -1, header: -1}
				if n.sub == nil {
					n.sub = make(map[string]*node)
				}
				n.sub[k] = c
			}
			n = c
		}
		out, nodes = append(out, head{name: s.key.String()}), append(nodes, n)
	}
	for i, s := range d.stmts {
		p := s.path()
		if !s.header {
			p = p[:len(p)-1]
		}
		n := root
		for _, k := range p {
			if n = n.sub[k]; n == nil {
				break
			}
			n.last = i
		}
		if n != nil && n != root && s.header && !s.array && n.header < 0 {
			n.header = i
		}
	}
	next := -1
	for j := len(nodes) - 1; j >= 0; j-- {
		out[j].last, out[j].next = nodes[j].last, next
		if nodes[j].header >= 0 {
			next = nodes[j].header
		}
	}
	return out
}

// after is the spot past the statement at i and the comments its run keeps, when
// a header or the end follows, as a table placed there needs
func (d *Document) after(i int) (spot, bool) {
	if i < 0 || i+1 < len(d.stmts) && !d.stmts[i+1].header {
		return spot{}, false
	}
	if u := d.units[d.unitOf(i)]; u.last == i+1 {
		return d.apart(u.end), true
	}
	return d.apart(d.stmts[i].end), true
}

// above is the spot above the header at i and the comments that are its own
func (d *Document) above(i int) spot {
	if u := d.units[d.unitOf(i)]; u.first == i {
		return d.apart(u.start)
	}
	return d.apart(d.stmts[i].lead)
}

// unitOf is the index of the unit holding the statement at i
func (d *Document) unitOf(i int) int {
	k, _ := slices.BinarySearchFunc(d.units, i, func(u unit, i int) int {
		switch {
		case u.last <= i:
			return -1
		case u.first > i:
			return 1
		}
		return 0
	})
	return k
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

// Fill adds each key src sets that d does not and names them in src's order. One
// under a table d writes inline joins that table, written as Marshal writes a
// value. Any other comes as src writes it, with its own comments, where src has
// it among d's keys and tables. A value d holds at a key or over it, an array of
// tables among them, stays d's; src's arrays of tables are left out.
func (d *Document) Fill(src *Document) ([]Key, error) {
	was := *d
	inside, err := d.fillInside(src)
	if err == nil {
		var added []Key
		if added, err = d.fillLines(src); err == nil {
			return src.inOrder(inside, added), nil
		}
	}
	*d = was
	return nil, err
}

// inOrder is the keys of a and b, each in the order d sets them, in that order
func (d *Document) inOrder(a, b []Key) []Key {
	if len(a) == 0 || len(b) == 0 {
		return append(a, b...)
	}
	nth := make(map[string]int, len(d.stmts))
	for i, s := range d.stmts {
		if !s.header {
			nth[s.path().String()] = i
		}
	}
	out := append(a, b...)
	slices.SortStableFunc(out, func(x, y Key) int { return cmp.Compare(nth[x.String()], nth[y.String()]) })
	return out
}

// fillInside adds each key src sets that d lacks under a table d writes inline,
// outside any array of tables, to that table, and names them in src's order
func (d *Document) fillInside(src *Document) ([]Key, error) {
	at := make(map[string]int) // d's statements setting an inline table, by their keys
	var arrays tables
	for i, s := range d.stmts {
		switch {
		case s.header && s.array:
			arrays.at(s.key).headed = true
		case !s.header && d.data[s.value[0]] == '{' && !arrays.headedOver(s.path()):
			at[s.path().String()] = i
		}
	}
	var (
		added []Key
		under tables          // the arrays of tables src's headers name
		was   = map[int]any{} // what each of those statements reads as
		now   = map[int]any{} // and with the keys it gains
	)
	for _, s := range src.stmts {
		p := s.path()
		if s.header && s.array {
			under.at(p).headed = true
		}
		if s.header || len(at) == 0 || under.headedOver(p) {
			continue
		}
		for n := len(p) - 1; n > 0; n-- {
			i, ok := at[p[:n].String()]
			if !ok {
				continue
			}
			if _, read := was[i]; !read {
				v, err := d.valueOf(d.stmts[i])
				if err != nil {
					return nil, err
				}
				was[i], now[i] = v, v
			}
			v, _ := lookup(src.tree, p)
			if _, held := lookup(now[i], p[n:]); held {
				break
			}
			if next, ok := with(now[i], p[n:], v); ok {
				now[i], added = next, append(added, p)
			}
			break
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	var out []byte
	from := 0
	for _, i := range slices.Sorted(maps.Keys(now)) {
		sh := d.shapeOf(d.stmts[i].value[0])
		text, err := d.rewrite(&sh, was[i], now[i])
		if err != nil {
			return nil, err
		}
		out, from = append(append(out, d.data[from:sh.from]...), text...), sh.to
	}
	return added, d.replace(append(out, d.data[from:]...))
}

// fillLines adds each key src sets that d does not as src writes it, with its own
// comments: after the one src sets before it in its table when d sets that, else
// after its table's header, else where Set puts it. A table d lacks comes as its
// header and that header's comments after the table src heads before it, else above
// the next one src heads that d has a header for, else where Set puts a new table.
func (d *Document) fillLines(src *Document) ([]Key, error) {
	keys, headers := make(map[string]statement), make(map[string]statement)
	for _, s := range d.stmts {
		switch k := s.path().String(); {
		case !s.header:
			keys[k] = s
		case !s.array && headers[k].end == 0:
			headers[k] = s
		}
	}
	heads := d.heads(src)
	var (
		added  []Key
		arrays tables // the arrays of tables src's headers name
		ins    []*insertion
		nth    = -1 // the index in heads of the table src headed last
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
			arrays.at(p).headed = true
		}
		if arrays.headedOver(p) {
			continue
		}
		if s.header {
			nth++
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
			at, ok := spot{}, false
			if nth > 0 {
				if prev := placed[heads[nth-1].name]; prev != nil {
					// right after the table Fill placed, which hands it what it ends with
					at, ok = prev.spot, true
					at.head, prev.foot = "\n", ""
				} else {
					at, ok = d.after(heads[nth-1].last)
				}
			}
			if next := heads[nth].next; !ok && next >= 0 {
				at, ok = d.above(next), true
			}
			if !ok {
				at = d.newTable()
			}
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
			pl, err := d.find(nil, p)
			if err != nil {
				return nil, err
			}
			if in = open(pl.spot); pl.spot.table {
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

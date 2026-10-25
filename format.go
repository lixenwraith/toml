package toml

import (
	"bytes"
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"
)

// Formatter writes a document in one layout, each comment kept with what it
// belongs to as a Document reads it. The zero value indents with a tab.
type Formatter struct {
	Indent       string // one level of indentation, up to 8 tabs or spaces; a tab when empty
	IndentTables bool   // indent a table a level per table it is under whose header is above it
	Align        bool   // line up the = and the comments of keys on consecutive lines
}

// A line indents at most maxIndent levels and aligns to at most alignKey runes of
// key and alignCode of line, so no line of the result grows past its source by
// more than a fixed amount, however deep or long the source
const (
	maxIndent = 16
	alignKey  = 32
	alignCode = 80
)

// Format is Formatter{}.Format
func Format(data []byte) ([]byte, error) { return Formatter{}.Format(data) }

// Format writes data spaced as Marshal writes, each table once at its first header:
// a reopening header's keys, and a dotted key reaching a table with a header, move
// there with their comments. A blank line run becomes one, or two below a comment
// that belongs above. It refuses what Parse refuses, a comment with a rune that can
// hide text, and a result that would not read as data does.
func (f Formatter) Format(data []byte) ([]byte, error) {
	indent := cmp.Or(f.Indent, "\t")
	if strings.Trim(indent, " \t") != "" || len(indent) > 8 {
		return nil, fmt.Errorf("indent %q is not up to 8 tabs and spaces", f.Indent)
	}
	tree, err := NewParser(data).Parse()
	if err != nil {
		return nil, err
	}
	if err := hiddenInComments(data); err != nil {
		return nil, err
	}
	d := layout(data)
	p := printer{indent: indent, align: f.Align}
	p.document(&d, regroup(&d, tree), f.IndentTables)
	out := p.buf.Bytes()
	got, err := NewParser(out).Parse()
	if err != nil {
		return nil, fmt.Errorf("format refused, its result would not parse: %w", err)
	}
	if !reflect.DeepEqual(got, tree) {
		return nil, fmt.Errorf("format refused, its result would read otherwise")
	}
	return out, nil
}

// hides reports a rune that changes how the text after it displays: an
// embedding, override or isolate of bidirectional text, or a line break but LF
func hides(r rune) bool {
	return r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069 || r == 0x85 || r == 0x2028 || r == 0x2029
}

// hiddenInComments refuses a comment holding a rune hides reports; a comment has
// no escape to write it as
func hiddenInComments(data []byte) error {
	lx := NewLexer(data)
	for t := lx.NextToken(); t.Type != TokenEOF; t = lx.NextToken() {
		if t.Type != TokenComment {
			continue
		}
		if i := strings.IndexFunc(t.Literal, hides); i >= 0 {
			r, _ := utf8.DecodeRuneInString(t.Literal[i:])
			return fmt.Errorf("comment at line %d holds %U, which can hide text", t.Line, r)
		}
	}
	return nil
}

// visible spells a string's source with each rune hides reports escaped, the
// value the same: a lone backslash before one, an unknown escape kept as
// written, becomes an escaped backslash
func visible(src string) string {
	if !strings.ContainsFunc(src, hides) {
		return src
	}
	var b strings.Builder
	escaped := false
	for _, r := range src {
		switch {
		case hides(r):
			if escaped {
				b.WriteByte('\\')
			}
			fmt.Fprintf(&b, `\u%04X`, r)
			escaped = false
		case r == '\\':
			b.WriteRune(r)
			escaped = !escaped
		default:
			b.WriteRune(r)
			escaped = false
		}
	}
	return b.String()
}

// piece is a statement as Format writes it, with the comments that go where it
// goes
type piece struct {
	key    []string // its parts as spelled, relative to its section
	header bool
	array  bool
	value  *node
	note   string     // the comment after it on its line
	heads  [][]string // comment groups above it, each a blank line apart
	lead   []string   // comment lines right above it
	tail   []string   // comment lines right below it
	trail  [][]string // comment groups below it, each a blank line apart
	brk    bool       // a blank line above it
}

// node is a value: a scalar's source, or an array or inline table
type node struct {
	text     string
	open     TokenType // TokenLBracket or TokenLBrace for a container
	note     string    // a comment after the opening bracket on its line
	items    []elem
	tail     []line // comment lines after the last element
	closeOwn bool   // the closing bracket on a line of its own
}

type elem struct {
	before []line // comment lines above it
	broken bool   // on a line after what precedes it
	blank  bool   // a blank line right above it
	key    []string
	value  *node
	note   string
}

type line struct {
	text  string
	blank bool // a blank line right above it
}

// section is a table and the pieces written under its header, the root's first
type section struct {
	path   Key
	header *piece
	pieces []*piece
	into   *section // the first header of the table this one opens again
	keep   bool     // under an array of tables, where no key moves in or out
	depth  int      // the tables it is under whose headers come before it
}

// tables is a trie of the paths headers name, so finding a key's table costs
// its length: home is the section of a table's first header keys can move to
type tables struct {
	sub    map[string]*tables
	home   *section
	headed bool
}

// child is t's node for k, made if missing
func (t *tables) child(k string) *tables {
	if t.sub == nil {
		t.sub = map[string]*tables{}
	}
	c := t.sub[k]
	if c == nil {
		c = &tables{}
		t.sub[k] = c
	}
	return c
}

// regroup gives each statement's piece to the section it is written in. A key
// goes to the deepest table its path reaches that has a header, a reopened
// header's keys to the first header; heads and lead go with the first piece of a
// run, tail and trail with the last, and a run that loses its first piece keeps
// its blank line above.
func regroup(d *Document, tree map[string]any) []*section {
	pieces := make([]*piece, len(d.stmts))
	for i, s := range d.stmts {
		pieces[i] = d.piece(s)
	}
	for k, u := range d.units {
		first, last := pieces[u.first], pieces[u.last-1]
		first.heads, first.brk = d.groups(u.start, d.stmts[u.first].lead), k > 0 || d.pre > 0
		last.tail, last.trail = d.trailing(d.stmts[u.last-1].end, u.end)
	}

	root := &section{}
	sections := []*section{root}
	var trie tables
	in := make([]*section, len(d.stmts))
	cur := root
	for i, s := range d.stmts {
		if s.header {
			cur = &section{path: s.key, header: pieces[i], keep: s.array || underArray(tree, s.key)}
			t := &trie
			for n, k := range s.key {
				if t = t.child(k); t.headed && n < len(s.key)-1 {
					cur.depth++
				}
			}
			t.headed = true
			switch {
			case cur.keep:
				sections = append(sections, cur)
			case t.home == nil:
				t.home = cur
				sections = append(sections, cur)
			default:
				cur.into = t.home
			}
		}
		in[i] = cur
	}
	dst := make([]*section, len(d.stmts))
	for i, s := range d.stmts {
		src := in[i]
		dst[i] = cmp.Or(src.into, src)
		if s.header || src.keep {
			continue
		}
		at, t := 0, &trie
		for _, k := range src.path {
			t = t.sub[k]
		}
		for n, k := range s.key[:len(s.key)-1] {
			if t = t.sub[k]; t == nil {
				break
			}
			if t.home != nil {
				dst[i], at = t.home, n+1
			}
		}
		pieces[i].key = pieces[i].key[at:]
	}
	for _, u := range d.units {
		if dst[u.first] == in[u.first] {
			continue
		}
		for i := u.first + 1; i < u.last; i++ {
			if dst[i] == in[i] {
				pieces[i].brk = true
				break
			}
		}
	}

	// A reopened header's comments go above the key that touched it, else below
	// what is written before them where its keys go
	type move struct {
		p     *piece
		to    *section
		trail [][]string // with no piece, comments for the last piece of to
	}
	var moves []move
	opened := map[*section]bool{} // a reopened section that has begun in its first
	for i, s := range d.stmts {
		p, src := pieces[i], in[i]
		switch {
		case s.header && src.into == nil:
		case s.header:
			at := slices.Concat(p.lead, notes(p.note), p.tail) // the lines around the header's
			next := i + 1
			if len(at) > 0 && next < len(d.stmts) && in[next] == src && dst[next] == src.into && !pieces[next].brk {
				k := pieces[next]
				k.heads, k.lead = p.heads, slices.Concat(at, k.lead)
			} else {
				groups := slices.Clone(p.heads)
				if len(at) > 0 {
					groups = append(groups, at)
				}
				if groups = append(groups, p.trail...); len(groups) > 0 {
					moves = append(moves, move{to: src.into, trail: groups})
				}
			}
		case dst[i] == src:
			src.pieces = append(src.pieces, p)
		default:
			if dst[i] == src.into && !opened[src] {
				opened[src], p.brk = true, true
			}
			moves = append(moves, move{p: p, to: dst[i]})
		}
	}
	for _, m := range moves {
		if m.p == nil {
			last := m.to.header
			if n := len(m.to.pieces); n > 0 {
				last = m.to.pieces[n-1]
			}
			last.trail = append(last.trail, m.trail...)
			continue
		}
		if len(m.to.pieces) == 0 {
			m.p.brk = len(m.p.heads) > 0
		}
		m.to.pieces = append(m.to.pieces, m.p)
	}
	return sections
}

// underArray reports a path through an array of tables, whose headers name a
// different table each time
func underArray(tree map[string]any, path Key) bool {
	var cur any = tree
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			break
		}
		cur = m[k]
	}
	_, ok := cur.([]map[string]any)
	return ok
}

// piece reads a statement's source into its piece
func (d *Document) piece(s statement) *piece {
	c := cursor{src: d.data[s.start:s.end]}
	c.lx = Lexer{input: c.src, line: 1}
	c.next()
	p := &piece{header: s.header, array: s.array}
	if s.header {
		for c.t.Type == TokenLBracket {
			c.next()
		}
		p.key = c.key()
		for c.t.Type == TokenRBracket {
			c.next()
		}
	} else {
		p.key = c.key()
		c.next() // past =
		p.value = c.value()
	}
	if c.t.Type == TokenComment {
		p.note = comment(c.t.Literal)
	}
	if g := d.groups(s.lead, s.start); len(g) > 0 {
		p.lead = g[0]
	}
	return p
}

// comment is a comment's text as written, its trailing blanks dropped
func comment(text string) string { return "#" + strings.TrimRight(text, " \t") }

// notes is a header's comment on its line as a line of its own, if it has one
func notes(note string) []string {
	if note == "" {
		return nil
	}
	return []string{note}
}

// groups reads the comment lines of data[from:to], which holds only comment and
// blank lines, a group to each run no blank line parts
func (d *Document) groups(from, to int) [][]string {
	var out [][]string
	fresh := true
	for l := range strings.SplitSeq(string(d.data[from:to]), "\n") {
		switch l = strings.Trim(l, " \t\r"); {
		case l == "":
			fresh = true
		case fresh:
			out, fresh = append(out, []string{l}), false
		default:
			out[len(out)-1] = append(out[len(out)-1], l)
		}
	}
	return out
}

// trailing reads the comments below a run: the group touching it, then the rest
func (d *Document) trailing(from, to int) ([]string, [][]string) {
	g := d.groups(from, to)
	first, _, _ := bytes.Cut(d.data[from:to], []byte("\n"))
	if len(g) > 0 && len(bytes.Trim(first, " \t\r")) > 0 {
		return g[0], g[1:]
	}
	return nil, g
}

// cursor walks a statement's tokens
type cursor struct {
	src []byte
	lx  Lexer
	t   Token
}

func (c *cursor) next() { c.t = c.lx.NextToken() }

// spell is the token as written; a string's runes that can hide text escaped
func (c *cursor) spell() string {
	text := string(c.src[c.t.Pos:c.t.End])
	if c.t.Type == TokenString {
		return visible(text)
	}
	return text
}

// key reads a dotted key's parts as spelled, leaving the cursor past them
func (c *cursor) key() []string {
	var k []string
	for {
		k = append(k, c.spell())
		if c.next(); c.t.Type != TokenDot {
			return k
		}
		c.next()
	}
}

// value reads a value, leaving the cursor past it
func (c *cursor) value() *node {
	if c.t.Type != TokenLBracket && c.t.Type != TokenLBrace {
		n := &node{text: c.spell()}
		c.next()
		return n
	}
	n := &node{open: c.t.Type}
	closer := TokenRBracket
	if n.open == TokenLBrace {
		closer = TokenRBrace
	}
	c.next()
	var pending []line
	nl := 0 // newlines since the last element or comment
	for c.t.Type != closer {
		inside := len(n.items) > 0 || len(pending) > 0
		switch c.t.Type {
		case TokenNewline:
			nl++
		case TokenComma:
		case TokenComment:
			text := comment(c.t.Literal)
			switch {
			case nl > 0:
				pending = append(pending, line{text, nl > 1 && inside})
			case len(n.items) > 0:
				n.items[len(n.items)-1].note = text
			default:
				n.note = text
			}
			nl = 0
		default:
			e := elem{before: pending, broken: nl > 0 || len(pending) > 0, blank: nl > 1 && inside}
			pending = nil
			if n.open == TokenLBrace {
				e.key = c.key()
				c.next() // past =
			}
			e.value = c.value()
			n.items = append(n.items, e)
			nl = 0
			continue
		}
		c.next()
	}
	n.tail, n.closeOwn = pending, nl > 0
	c.next()
	return n
}

// printer writes pieces, its depth the indentation of the line being written
type printer struct {
	buf    bytes.Buffer
	indent string
	align  bool
	depth  int
}

func (p *printer) newline(depth int) {
	p.buf.WriteByte('\n')
	p.indentTo(depth)
}

func (p *printer) indentTo(depth int) {
	for range min(depth, maxIndent) {
		p.buf.WriteString(p.indent)
	}
	p.depth = depth
}

// document writes the file's opening comments, each section's pieces and the
// file's closing comments. Two blank lines follow a group of comments that
// belongs to what is above it, after a blank line, so it reads that way again.
func (p *printer) document(d *Document, sections []*section, indentTables bool) {
	pre := d.groups(0, d.pre)
	p.groups(pre, 0)
	gap := min(len(pre), 2) // blank lines after the opening comments
	var foot [][]string
	if d.foot >= 0 {
		foot = d.groups(d.foot, len(d.data))
	} else if s := lastSection(sections); s != nil {
		// Comments below the last piece, a blank line apart, read as the file's
		// closing ones; written so, they read the same again
		last := s.header
		if n := len(s.pieces); n > 0 {
			last = s.pieces[n-1]
		}
		foot, last.trail = last.trail, nil
	}
	var last *piece
	for _, s := range sections {
		depth := 0
		if indentTables {
			depth = s.depth
		}
		all := s.pieces
		if s.header != nil {
			all = append([]*piece{s.header}, all...)
		}
		widths := p.widths(all, depth)
		for i, pc := range all {
			p.blanks(gap, last, pc.brk || len(pc.heads) > 0)
			p.piece(pc, depth, widths[i])
			last = pc
		}
	}
	if len(foot) > 0 {
		p.blanks(gap, last, true)
		p.groups(foot, 0)
	}
}

// lastSection is the last section with a piece to write
func lastSection(sections []*section) *section {
	for i := len(sections) - 1; i >= 0; i-- {
		if s := sections[i]; s.header != nil || len(s.pieces) > 0 {
			return s
		}
	}
	return nil
}

// blanks writes the blank lines above what follows last: two below comment groups
// that belong to it, else one where either asks for it
func (p *printer) blanks(gap int, last *piece, brk bool) {
	n := 0
	switch {
	case last == nil:
		n = gap
	case len(last.trail) > 0:
		n = 2
	case brk || len(last.tail) > 0:
		n = 1
	}
	for range n {
		p.buf.WriteByte('\n')
	}
}

func (p *printer) groups(g [][]string, depth int) {
	for i, lines := range g {
		if i > 0 {
			p.buf.WriteByte('\n')
		}
		p.comments(lines, depth)
	}
}

func (p *printer) comments(lines []string, depth int) {
	for _, l := range lines {
		p.indentTo(depth)
		p.buf.WriteString(l)
		p.buf.WriteByte('\n')
	}
}

// width is where a piece's = and comment go when its run is aligned
type width struct{ key, code int }

// widths lines up each run of keys on consecutive lines, each on one line and
// short enough, no comment line among them
func (p *printer) widths(all []*piece, depth int) []width {
	w := make([]width, len(all))
	if !p.align {
		return w
	}
	codes := make([]string, len(all))
	for i, pc := range all {
		if pc.header || utf8.RuneCountInString(strings.Join(pc.key, ".")) > alignKey {
			continue
		}
		if c := p.code(pc, depth, alignKey); utf8.RuneCountInString(c) <= alignCode {
			codes[i] = p.code(pc, depth, 0)
		}
	}
	for i := 0; i < len(all); {
		j := i + 1
		for j < len(all) && joins(all[j-1], all[j], codes[j-1], codes[j]) {
			j++
		}
		if codes[i] == "" {
			i = j
			continue
		}
		var run width
		for k := i; k < j; k++ {
			run.key = max(run.key, utf8.RuneCountInString(strings.Join(all[k].key, ".")))
		}
		for k := i; k < j; k++ {
			run.code = max(run.code, utf8.RuneCountInString(p.code(all[k], depth, run.key)))
		}
		for k := i; k < j; k++ {
			w[k] = run
		}
		i = j
	}
	return w
}

// joins reports b on the line after a in one aligned run
func joins(a, b *piece, ac, bc string) bool {
	return ac != "" && bc != "" && !b.brk && len(b.heads)+len(b.lead) == 0 &&
		len(a.tail)+len(a.trail) == 0 && !strings.Contains(ac+bc, "\n")
}

// code is a key's line before its comment, the key padded to keyWidth
func (p *printer) code(pc *piece, depth, keyWidth int) string {
	q := printer{indent: p.indent, depth: depth}
	k := strings.Join(pc.key, ".")
	q.buf.WriteString(k)
	q.buf.WriteString(strings.Repeat(" ", max(0, keyWidth-utf8.RuneCountInString(k))))
	q.buf.WriteString(" = ")
	q.node(pc.value)
	return q.buf.String()
}

func (p *printer) piece(pc *piece, depth int, w width) {
	for _, g := range pc.heads {
		p.comments(g, depth)
		p.buf.WriteByte('\n')
	}
	p.comments(pc.lead, depth)
	p.indentTo(depth)
	code := "[" + strings.Join(pc.key, ".") + "]"
	switch {
	case pc.array:
		code = "[" + code + "]"
	case !pc.header:
		code = p.code(pc, depth, w.key)
	}
	p.buf.WriteString(code)
	if pc.note != "" {
		p.buf.WriteString(strings.Repeat(" ", max(0, w.code-utf8.RuneCountInString(code))+1))
		p.buf.WriteString(pc.note)
	}
	p.buf.WriteByte('\n')
	p.comments(pc.tail, depth)
	for _, g := range pc.trail {
		p.buf.WriteByte('\n')
		p.comments(g, depth)
	}
}

// node writes a value; a container's elements on lines of their own go a level
// deeper than the line its opening bracket is on, and it keeps a trailing comma
// exactly when its closing bracket has a line of its own
func (p *printer) node(n *node) {
	if n.open == 0 {
		p.buf.WriteString(n.text)
		return
	}
	open, close, pad := "[", "]", ""
	if n.open == TokenLBrace {
		open, close, pad = "{", "}", " "
	}
	base := p.depth
	p.buf.WriteString(open)
	note := n.note
	if note != "" {
		p.buf.WriteString(" " + note)
	}
	for i, e := range n.items {
		switch {
		case e.broken || note != "":
			for _, c := range e.before {
				p.line(c, base+1)
			}
			p.line(line{blank: e.blank}, base+1)
		case i == 0:
			p.buf.WriteString(pad)
		default:
			p.buf.WriteByte(' ')
		}
		if e.key != nil {
			p.buf.WriteString(strings.Join(e.key, ".") + " = ")
		}
		p.node(e.value)
		if i < len(n.items)-1 || n.closeOwn {
			p.buf.WriteByte(',')
		}
		if note = e.note; note != "" {
			p.buf.WriteString(" " + note)
		}
	}
	for _, c := range n.tail {
		p.line(c, base+1)
	}
	if n.closeOwn || note != "" || len(n.tail) > 0 {
		p.newline(base)
	} else if len(n.items) > 0 {
		p.buf.WriteString(pad)
	}
	p.buf.WriteString(close)
}

// line starts a line at depth inside a value, a blank one above it if asked,
// and writes the comment it holds
func (p *printer) line(l line, depth int) {
	if l.blank {
		p.buf.WriteByte('\n')
	}
	p.newline(depth)
	p.buf.WriteString(l.text)
}

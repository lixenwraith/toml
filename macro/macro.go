// Package macro expands macros in a parsed TOML document. A macro is data
// under the document's [macros] table; any table carrying a "macro" key invokes
// one and is replaced by its body with the arguments substituted. Expansion reads
// nothing but the documents it is given, so equal bytes expand to equal trees on
// every peer. Syntax: the Macros section of the module README.
package macro

import (
	"cmp"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"strconv"
	"strings"
)

// Reserved keys; keyMacros is read at a document's root only.
const (
	keyMacros = "macros"
	keyInvoke = "macro"
	keyEach   = "each"
)

// DefaultMaxValues bounds the values the expansions through one Set write.
const DefaultMaxValues = 1 << 18

// Bounds no document can exceed: invocation nesting, the nesting of a result,
// which is what Decode and Marshal take, a macro's parameters, which an
// invocation flags in one word, and the parameters bases leave open through one
// set, each as costly as a declared one though no document writes it.
const (
	maxDepth   = 32
	maxNesting = 1000
	maxParams  = 64
	maxOpen    = 1 << 16
	maxSteps   = 64 // steps of a path an error spells
)

var (
	errDefinitions = errors.New("[macros] is not allowed in this document")
	errEach        = errors.New("each must be a table of arguments")
	errEachValue   = errors.New("each expands only inside an array")
	errNesting     = fmt.Errorf("nests deeper than %d", maxNesting)
)

// kind is a parameter's type, and what a value is. A float parameter takes an
// integer too, as the decoder does.
type kind uint8

const (
	kindAny kind = iota
	kindArray
	kindBool
	kindFloat
	kindInt
	kindString
	kindTable
	kindOther // a Go value no document holds: only an any parameter takes it
)

var kindNames = [...]string{"any", "array", "bool", "float", "int", "string", "table"}

func kindOf(v any) kind {
	switch v.(type) {
	case int64, int, int8, int16, int32, uint, uint8, uint16, uint32, uint64:
		return kindInt
	case float64, float32:
		return kindFloat
	case bool:
		return kindBool
	case string:
		return kindString
	case map[string]any:
		return kindTable
	case []any, []map[string]any:
		return kindArray
	}
	return kindOther
}

func kindName(v any) string {
	if k := kindOf(v); k != kindOther {
		return kindNames[k]
	}
	return fmt.Sprintf("%T", v)
}

func (k kind) holds(v any) bool {
	got := kindOf(v)
	return k == kindAny || k == got || k == kindFloat && got == kindInt
}

type param struct {
	kind  kind
	value any  // the default as written
	def   node // the default, which may invoke
}

// macro is a definition compiled once. Its parameters are sorted by name, and a
// parameter's index is its slot in an invocation's arguments.
type macro struct {
	name     string
	names    []string // the parameters' names
	params   []param
	required uint64 // slots with no default
	body     node   // over a base, the invocation of it that a body would write
	part     string // body, or with over a base: the body's name in an error's path
	base     string // the base's name
}

func (m *macro) slot(name string) (int, bool) { return slices.BinarySearch(m.names, name) }

type op uint8

const (
	opValue  op = iota // a scalar
	opStatic           // a table or array nothing is substituted in: copied
	opArg              // "${name}": the argument itself
	opText             // text around references
	opArray
	opTable
	opInvoke // a table holding the macro key: elems are its name, then its each
)

// node is a value of a body as an invocation instantiates it; size and height
// are a static one's values and nesting
type node struct {
	op      op
	dynamic bool // opTable: a key is spelled from arguments
	slot    int
	size    int
	height  int
	value   any
	segs    []segment
	elems   []node
	fields  []field // sorted by key as written
}

type field struct {
	key  string
	segs []segment // a key spelled from arguments
	node node
}

type segment struct {
	text string
	slot int // the parameter spliced in, -1 for text
}

// Set holds the macros a group of documents may invoke. The zero value is an
// empty set; a set is not for concurrent use.
type Set struct {
	// MaxValues bounds the values every Expand through the set may write
	// together; 0 means DefaultMaxValues
	MaxValues int
	macros    map[string]*macro
	spent     int
	open      int // parameters its macros take from their bases
}

func NewSet() *Set { return &Set{} }

// ExpandDocument expands a document that defines its own macros.
func ExpandDocument(doc map[string]any) (map[string]any, error) {
	var s Set
	if err := s.Define(doc); err != nil {
		return nil, err
	}
	return s.Expand(doc)
}

// Define moves doc's [macros] into the set, which keeps them. A name defined
// twice is an error across documents too, so a library cannot silently shadow
// its user. A base is one of doc's macros, in any order of names, or one the
// set holds already.
func (s *Set) Define(doc map[string]any) error {
	raw, ok := doc[keyMacros]
	if !ok {
		return nil
	}
	delete(doc, keyMacros)
	table, ok := raw.(map[string]any)
	if !ok {
		return errors.New("macros must be a table")
	}
	if s.macros == nil {
		s.macros = make(map[string]*macro)
	}
	names := sortedKeys(table)
	waiting := map[string]*macro{} // those over a base, until it is built
	for _, name := range names {
		if _, dup := s.macros[name]; dup {
			return fmt.Errorf("macro %s is defined twice", quote(name))
		}
		m, err := define(name, table[name])
		if err != nil {
			return fmt.Errorf("macro %s: %w", quote(name), err)
		}
		if m.part == "with" {
			waiting[name] = m
		} else {
			s.macros[name] = m
		}
	}
	// A base may sort after the macro over it and be over another in turn, so a
	// chain of them is built from its far end; a name leaves waiting as it joins
	// one, so a long chain is walked once and costs no stack
	for _, name := range names {
		var chain []*macro
		for m := waiting[name]; m != nil; m = waiting[m.base] {
			chain = append(chain, m)
			delete(waiting, m.name)
		}
		for _, m := range slices.Backward(chain) {
			base := s.macros[m.base]
			if _, here := table[m.base]; base == nil && here {
				return fmt.Errorf("macro %s is its own base through %s", quote(m.base), quote(m.name))
			} else if base == nil {
				return fmt.Errorf("macro %s: unknown base %s", quote(m.name), quote(m.base))
			}
			def := table[m.name].(map[string]any)
			if err := m.build(def, base); err != nil {
				return fmt.Errorf("macro %s: %w", quote(m.name), err)
			}
			with, _ := def["with"].(map[string]any)
			open := len(base.names) - len(with)
			if open > maxOpen-s.open {
				return fmt.Errorf("macro %s: bases leave more than %d parameters open", quote(m.name), maxOpen)
			}
			s.open += open
			s.macros[m.name] = m
		}
	}
	return nil
}

// define reads a definition. One over a base comes back unbuilt: its
// parameters are its base's too, and the base may not be read yet.
func define(name string, raw any) (*macro, error) {
	def, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("definition must be a table")
	}
	for _, key := range sortedKeys(def) {
		if !slices.Contains([]string{"base", "body", "defaults", "doc", "params", "with"}, key) {
			return nil, fmt.Errorf("unknown key %s", quote(key))
		}
	}
	if _, ok := def["doc"].(string); def["doc"] != nil && !ok {
		return nil, errors.New("doc must be a string")
	}
	_, body := def["body"]
	base, over := def["base"]
	_, with := def["with"]
	m := &macro{name: name, part: "body"}
	switch {
	case body && over:
		return nil, errors.New("body and base are both given")
	case with && !over:
		return nil, errors.New("with stands only beside base")
	case !body && !over:
		return nil, errors.New("body is missing")
	case body:
		return m, m.build(def, nil)
	}
	if m.base, ok = base.(string); !ok {
		return nil, errors.New("base must be a macro's name")
	}
	m.part = "with"
	return m, nil
}

// build reads m's parameters, defaults and body from its definition. Over a
// base the body is with, an argument for each parameter of the base.
func (m *macro) build(def map[string]any, base *macro) error {
	params, ok := def["params"].(map[string]any)
	switch {
	case def["params"] != nil && !ok:
		return errors.New("params must be a table of name = type")
	case len(params) > maxParams:
		return fmt.Errorf("more than %d parameters", maxParams)
	}
	m.names, m.params = sortedKeys(params), make([]param, len(params))
	for slot, key := range m.names {
		typ, _ := params[key].(string)
		k := slices.Index(kindNames[:], typ)
		switch {
		case !validName(key) || key == keyInvoke || key == keyEach:
			return fmt.Errorf("parameter %s: not a usable name", quote(key))
		case k < 0:
			return fmt.Errorf("parameter %s: type must be one of %s", quote(key), strings.Join(kindNames[:], ", "))
		}
		m.params[slot].kind = kind(k)
	}
	m.required = 1<<len(m.params) - 1
	with, ok := def["with"].(map[string]any)
	if def["with"] != nil && !ok {
		return errors.New("with must be a table of arguments")
	}
	if base != nil {
		if err := m.adopt(base, with); err != nil {
			return err
		}
	}

	defaults, ok := def["defaults"].(map[string]any)
	if def["defaults"] != nil && !ok {
		return errors.New("defaults must be a table")
	}
	c := compiler{m: m}
	for _, key := range sortedKeys(defaults) {
		slot, ok := m.slot(key)
		if !ok {
			return fmt.Errorf("default for undeclared parameter %s", quote(key))
		}
		p := &m.params[slot]
		if !p.kind.holds(defaults[key]) {
			return fmt.Errorf("default for %s: want %s, got %s", quote(key), kindNames[p.kind], kindName(defaults[key]))
		}
		n, _, err := c.compile(defaults[key], 0)
		if err != nil {
			return at(at(err, key), "defaults")
		}
		p.value, p.def = defaults[key], n
		m.required &^= 1 << slot
	}

	// A reference to an undeclared parameter is a typo; catch it before any use
	c.template = true
	if base == nil {
		n, _, err := c.compile(def["body"], 0)
		m.body = n
		return at(err, "body")
	}
	m.body = node{op: opInvoke, elems: []node{{op: opValue, value: base.name, size: 1}}, fields: make([]field, len(base.names))}
	for slot, key := range base.names {
		f := &m.body.fields[slot]
		f.key = key
		v, set := with[key]
		if !set {
			f.node.op = opArg
			f.node.slot, _ = m.slot(key)
			continue
		}
		n, plain, err := c.compile(v, 1)
		if err != nil {
			return at(at(err, key), "with")
		}
		// Written without $ and invoking nothing, v is every invocation's
		// argument, so its type is settled now
		if k := base.params[slot].kind; plain && !k.holds(v) {
			return at(in(fmt.Errorf("argument %s: want %s, got %s", quote(key), kindNames[k], kindName(v)), base.name), "with")
		}
		f.node = n
	}
	return nil
}

// adopt gives m the parameters of base that with does not set, each with the
// base's type and default, beside the ones m declares
func (m *macro) adopt(base *macro, with map[string]any) error {
	for _, key := range sortedKeys(with) {
		if _, ok := base.slot(key); !ok {
			return at(in(fmt.Errorf("unknown argument %s", quote(key)), base.name), "with")
		}
	}
	names := slices.Clone(m.names)
	for _, key := range base.names {
		_, set := with[key]
		if _, declared := m.slot(key); declared && !set {
			return fmt.Errorf("parameter %s: the base leaves it open", quote(key))
		} else if !set {
			names = append(names, key)
		}
	}
	if len(names) > maxParams {
		return fmt.Errorf("more than %d parameters", maxParams)
	}
	slices.Sort(names)
	params, required := make([]param, len(names)), uint64(0)
	for slot, key := range names {
		if own, declared := m.slot(key); declared {
			params[slot], required = m.params[own], required|1<<slot
		} else {
			open, _ := base.slot(key)
			params[slot], required = base.params[open], required|(base.required>>open&1)<<slot
		}
	}
	m.names, m.params, m.required = names, params, required
	return nil
}

// compiler reads a macro's body into the template its invocations instantiate,
// or a default, whose text is not a template
type compiler struct {
	m        *macro
	template bool
}

// compile reads v. plain reports a node that is v as written, so that the value
// holding it can stay one static node.
func (c *compiler) compile(v any, depth int) (n node, plain bool, err error) {
	if depth > maxNesting {
		return n, false, errNesting
	}
	switch v := v.(type) {
	case string:
		if c.template && strings.Contains(v, "$") {
			return c.text(v)
		}
	case map[string]any:
		return c.table(v, depth, false)
	case []map[string]any:
		list := make([]any, len(v))
		for i, t := range v {
			list[i] = t
		}
		return c.array(list, depth)
	case []any:
		return c.array(v, depth)
	}
	return node{op: opValue, value: v, size: 1}, true, nil
}

func (c *compiler) text(s string) (node, bool, error) {
	segs, err := c.parse(s)
	switch {
	case err != nil:
		return node{}, false, err
	case len(segs) > 1:
		return node{op: opText, segs: segs}, false, nil
	case segs[0].slot < 0:
		return node{op: opValue, value: segs[0].text, size: 1}, false, nil
	}
	return node{op: opArg, slot: segs[0].slot}, false, nil
}

// parse splits a template into text and ${name} references; $$ is a literal $
func (c *compiler) parse(s string) ([]segment, error) {
	var segs []segment
	var lit strings.Builder
	for i := 0; i < len(s); {
		switch {
		case s[i] != '$':
			lit.WriteByte(s[i])
			i++
		case strings.HasPrefix(s[i:], "$$"):
			lit.WriteByte('$')
			i += 2
		case strings.HasPrefix(s[i:], "${"):
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, errors.New("unterminated ${")
			}
			name := s[i+2 : i+end]
			if !validName(name) {
				return nil, fmt.Errorf("%s does not name a parameter", quote(s[i:i+end+1]))
			}
			slot, ok := c.m.slot(name)
			if !ok {
				return nil, fmt.Errorf("undeclared parameter %s", quote(name))
			}
			if lit.Len() > 0 {
				segs = append(segs, segment{text: lit.String(), slot: -1})
				lit.Reset()
			}
			segs = append(segs, segment{slot: slot})
			i += end + 1
		default:
			return nil, errors.New("a literal $ is written $$")
		}
	}
	if lit.Len() > 0 || len(segs) == 0 {
		segs = append(segs, segment{text: lit.String(), slot: -1})
	}
	return segs, nil
}

// table reads a table: an invocation when it holds the macro key, one static
// node when nothing in it is substituted, unless keep asks for its fields
func (c *compiler) table(t map[string]any, depth int, keep bool) (node, bool, error) {
	keys := sortedKeys(t)
	_, invokes := t[keyInvoke]
	n := node{op: opTable, size: 1}
	if invokes || keep {
		n.fields = make([]field, len(keys))
	}
	for i, k := range keys {
		f := field{key: k}
		plain := true
		if c.template && strings.Contains(k, "$") {
			segs, err := c.parse(k)
			if err != nil {
				return n, false, at(err, k)
			}
			if plain = false; len(segs) == 1 && segs[0].slot < 0 {
				f.key = segs[0].text
			} else {
				f.segs, n.dynamic = segs, true
			}
		}
		var same bool
		var err error
		if lists, ok := t[k].(map[string]any); ok && k == keyEach && invokes {
			f.node, same, err = c.table(lists, depth+1, true)
		} else {
			f.node, same, err = c.compile(t[k], depth+1)
		}
		if err != nil {
			return n, false, at(err, k)
		}
		if f.node.op == opStatic {
			t[k] = f.node.value // an array of tables now reads as an array
		}
		if !(plain && same) && n.fields == nil {
			n.fields = make([]field, len(keys))
			for j, prev := range keys[:i] {
				n.fields[j] = field{key: prev, node: static(t[prev])}
			}
		}
		if n.fields != nil {
			n.fields[i] = f
		}
		n.size, n.height = n.size+f.node.size, max(n.height, f.node.height)
	}
	n.height++
	switch {
	case invokes && !n.dynamic:
		return invocation(n.fields), false, nil
	case n.fields == nil:
		return node{op: opStatic, value: t, size: n.size, height: n.height}, true, nil
	}
	return n, false, nil
}

// invocation is a table holding the macro key: its name, its each table, and
// the fields that are its arguments
func invocation(fields []field) node {
	n := node{op: opInvoke, elems: make([]node, 1, 2)}
	for _, f := range fields {
		switch f.key {
		case keyInvoke:
			n.elems[0] = f.node
		case keyEach:
			n.elems = append(n.elems, f.node)
		default:
			n.fields = append(n.fields, f)
		}
	}
	return n
}

func (c *compiler) array(a []any, depth int) (node, bool, error) {
	n := node{op: opArray, size: 1}
	for i, el := range a {
		sub, plain, err := c.compile(el, depth+1)
		if err != nil {
			return n, false, atIndex(err, i)
		}
		if sub.op == opStatic {
			a[i] = sub.value
		}
		if !plain && n.elems == nil {
			n.elems = make([]node, len(a))
			for j, prev := range a[:i] {
				n.elems[j] = static(prev)
			}
		}
		if n.elems != nil {
			n.elems[i] = sub
		}
		n.size, n.height = n.size+sub.size, max(n.height, sub.height)
	}
	n.height++
	if n.elems == nil {
		return node{op: opStatic, value: a, size: n.size, height: n.height}, true, nil
	}
	return n, false, nil
}

// static is the node of a value compile found plain
func static(v any) node {
	n := node{op: opValue, value: v}
	if n.size, n.height = measure(v); n.height > 0 {
		n.op = opStatic
	}
	return n
}

// measure is the values in v and its nesting, 0 for a scalar
func measure(v any) (size, height int) {
	size = 1
	switch v := v.(type) {
	case map[string]any:
		for _, x := range v {
			s, h := measure(x)
			size, height = size+s, max(height, h)
		}
	case []any:
		for _, x := range v {
			s, h := measure(x)
			size, height = size+s, max(height, h)
		}
	default:
		return 1, 0
	}
	return size, height + 1
}

// Expand returns doc with every invocation replaced. doc is not modified, and
// the result shares no table or array with it or with the set. Arrays come back
// as []any, whatever form the parser gave them.
func (s *Set) Expand(doc map[string]any) (map[string]any, error) {
	if _, ok := doc[keyMacros]; ok {
		return nil, errDefinitions
	}
	e := expander{set: s, limit: cmp.Or(s.MaxValues, DefaultMaxValues)}
	out, _, err := e.walk(doc, 0)
	if err != nil {
		if _, ok := err.(*pathError); !ok {
			err = &pathError{err: err}
		}
		return nil, err
	}
	table, ok := out.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("document expands to a %s, not a table", kindName(out))
	}
	return table, nil
}

type expander struct {
	set   *Set
	limit int
	stack []string // macros being expanded, outermost first
	args  []arg    // the arguments of the invocations being expanded, a call each
	keys  []string // the sorted keys of the tables being read, a run each
}

// arg is an argument and its nesting, 0 for a scalar
type arg struct {
	v any
	h int
}

// call is one invocation: its arguments are e.args[base+slot], and the lists
// of its each follow them, in slot order
type call struct {
	m     *macro
	base  int
	count int    // times the body is instantiated
	given uint64 // slots holding an argument; the rest take their default
	each  uint64 // slots bound per element of a list
}

func (e *expander) spend(n int) error {
	if n > e.limit-e.set.spent {
		e.set.spent = max(e.set.spent, e.limit)
		return fmt.Errorf("expansion exceeds %d values", e.limit)
	}
	e.set.spent += n
	return nil
}

// sorted puts t's keys, sorted, on e.keys and returns where they start. They
// are read by index, since a table under t moves the slice, and cut off after.
func (e *expander) sorted(t map[string]any) int {
	base := len(e.keys)
	for k := range t {
		e.keys = append(e.keys, k)
	}
	slices.Sort(e.keys[base:])
	return base
}

// walk copies v, a value of a document, for depth d of the result, with every
// invocation in it expanded; h is the copy's nesting
func (e *expander) walk(v any, d int) (out any, h int, err error) {
	switch v := v.(type) {
	case map[string]any:
		if _, ok := v[keyInvoke]; ok {
			return e.invokeRaw(v, d, false, nil)
		}
		if d >= maxNesting {
			return nil, 0, errNesting
		}
		out := make(map[string]any, len(v))
		base := e.sorted(v)
		for i := base; i < base+len(v); i++ {
			k := e.keys[i]
			x, xh, err := e.walk(v[k], d+1)
			if err != nil {
				return nil, 0, at(err, k)
			}
			out[k], h = x, max(h, xh)
		}
		e.keys = e.keys[:base]
		return out, h + 1, nil
	case []any:
		if d >= maxNesting {
			return nil, 0, errNesting
		}
		out := make([]any, 0, len(v))
		for i, el := range v {
			if out, h, err = e.element(el, i, d, out, h); err != nil {
				return nil, 0, err
			}
		}
		return out, h + 1, nil
	case []map[string]any:
		if d >= maxNesting {
			return nil, 0, errNesting
		}
		out := make([]any, 0, len(v))
		for i, el := range v {
			if out, h, err = e.element(el, i, d, out, h); err != nil {
				return nil, 0, err
			}
		}
		return out, h + 1, nil
	}
	return v, 0, nil
}

// element appends an array's element i to out, expanded: an invocation's
// results, an array among them spliced in, or the element itself
func (e *expander) element(el any, i, d int, out []any, h int) ([]any, int, error) {
	if t, ok := el.(map[string]any); ok {
		if _, ok := t[keyInvoke]; ok {
			_, xh, err := e.invokeRaw(t, d+1, false, &out)
			return out, max(h, xh), atIndex(err, i)
		}
	}
	x, xh, err := e.walk(el, d+1)
	return append(out, x), max(h, xh), atIndex(err, i)
}

// open finds the macro an invocation names
func (e *expander) open(name any) (*macro, error) {
	s, ok := name.(string)
	if !ok {
		return nil, errors.New("macro name must be a string")
	}
	m := e.set.macros[s]
	switch {
	case m == nil:
		return nil, fmt.Errorf("unknown macro %s", quote(s))
	case slices.Contains(e.stack, s):
		chain := make([]string, len(e.stack))
		for i, name := range e.stack {
			chain[i] = spell(name)
		}
		return nil, fmt.Errorf("macro %s invokes itself through %s", quote(s), strings.Join(chain, " → "))
	case len(e.stack) >= maxDepth:
		return nil, fmt.Errorf("macros nest deeper than %d", maxDepth)
	}
	return m, nil
}

func (e *expander) push(m *macro) call {
	base := len(e.args)
	e.args = append(e.args, make([]arg, len(m.params))...)
	return call{m: m, base: base, count: 1}
}

// invokeRaw expands the invocation a table is, in a document or once a body's
// keys are spelled: expanded says its values already are. As an array's element
// it appends its results to dst; as a value it gives one.
func (e *expander) invokeRaw(inv map[string]any, d int, expanded bool, dst *[]any) (any, int, error) {
	lists, iterated := inv[keyEach]
	if iterated && dst == nil {
		return nil, 0, errEachValue
	}
	m, err := e.open(inv[keyInvoke])
	if err != nil {
		return nil, 0, err
	}
	// Arguments are expanded where they are written, before binding
	c := e.push(m)
	base := e.sorted(inv)
	for i := base; i < base+len(inv); i++ {
		k := e.keys[i]
		if k == keyInvoke || k == keyEach {
			continue
		}
		slot, ok := m.slot(k)
		if !ok {
			return nil, 0, in(fmt.Errorf("unknown argument %s", quote(k)), m.name)
		}
		v, h := inv[k], 0
		if expanded {
			_, h = measure(v)
		} else if v, h, err = e.walk(v, d); err != nil {
			return nil, 0, at(err, k)
		}
		if err := e.put(&c, slot, v, h); err != nil {
			return nil, 0, in(err, m.name)
		}
	}
	e.keys = e.keys[:base]
	if iterated {
		h := 0
		if expanded {
			_, h = measure(lists)
		} else if lists, h, err = e.walk(lists, d); err != nil {
			return nil, 0, at(err, keyEach)
		}
		if err := e.lists(&c, lists, h); err != nil {
			return nil, 0, in(err, m.name)
		}
	}
	return e.run(&c, d, dst)
}

// invokeNode expands an invocation a body holds. An argument that is a
// reference or static is lent to the macro invoked, which copies what it places.
func (e *expander) invokeNode(n *node, f *call, d int, dst *[]any) (any, int, error) {
	if len(n.elems) > 1 && dst == nil {
		return nil, 0, errEachValue
	}
	name, _, err := e.inst(&n.elems[0], f, d)
	if err != nil {
		return nil, 0, at(err, keyInvoke)
	}
	m, err := e.open(name)
	if err != nil {
		return nil, 0, err
	}
	c := e.push(m)
	for i := range n.fields {
		fld := &n.fields[i]
		slot, ok := m.slot(fld.key)
		if !ok {
			return nil, 0, in(fmt.Errorf("unknown argument %s", quote(fld.key)), m.name)
		}
		v, h, err := e.pass(&fld.node, f, d)
		if err != nil {
			return nil, 0, at(err, fld.key)
		}
		if err := e.put(&c, slot, v, h); err != nil {
			return nil, 0, in(err, m.name)
		}
	}
	if len(n.elems) == 1 {
		return e.run(&c, d, dst)
	}
	if each := &n.elems[1]; each.op == opTable && !each.dynamic && len(each.fields) > 0 {
		for i := range each.fields {
			fld := &each.fields[i]
			v, h, err := e.pass(&fld.node, f, d)
			if err != nil {
				return nil, 0, at(at(err, fld.key), keyEach)
			}
			if err := e.list(&c, fld.key, v, h); err != nil {
				return nil, 0, in(err, m.name)
			}
		}
		return e.run(&c, d, dst)
	}
	lists, h, err := e.inst(&n.elems[1], f, d)
	if err != nil {
		return nil, 0, at(err, keyEach)
	}
	if err := e.lists(&c, lists, h); err != nil {
		return nil, 0, in(err, m.name)
	}
	return e.run(&c, d, dst)
}

// pass is an argument for an invocation in a body: a reference or a static
// value as it stands, lent, else a value made for it
func (e *expander) pass(n *node, f *call, d int) (any, int, error) {
	switch n.op {
	case opArg:
		return e.use(f, n.slot, d, false)
	case opValue, opStatic:
		return n.value, n.height, nil
	}
	return e.inst(n, f, d)
}

func (e *expander) put(c *call, slot int, v any, h int) error {
	if k := c.m.params[slot].kind; !k.holds(v) {
		return fmt.Errorf("argument %s: want %s, got %s", quote(c.m.names[slot]), kindNames[k], kindName(v))
	}
	e.args[c.base+slot] = arg{v, h}
	c.given |= 1 << slot
	return nil
}

// lists reads an each table, whose nesting is h: its arrays all one length
func (e *expander) lists(c *call, v any, h int) error {
	table, ok := v.(map[string]any)
	if !ok || len(table) == 0 {
		return errEach
	}
	base := e.sorted(table)
	// The arrays are bound with one nesting, the table's less its own level. A
	// shared value is no part of that beside each, so with one in the table the
	// arrays are measured.
	zh := h - 1
	for i := base; i < base+len(table); i++ {
		if _, zipped := table[e.keys[i]].([]any); zipped {
			continue
		}
		zh = 0
		for j := base; j < base+len(table); j++ {
			if list, zipped := table[e.keys[j]].([]any); zipped {
				_, lh := measure(list)
				zh = max(zh, lh)
			}
		}
		break
	}
	for i := base; i < base+len(table); i++ {
		x, xh := table[e.keys[i]], zh
		if _, zipped := x.([]any); !zipped {
			_, xh = measure(x) // its own nesting, as an argument beside each has
		}
		if err := e.list(c, e.keys[i], x, xh); err != nil {
			return err
		}
	}
	e.keys = e.keys[:base]
	return nil
}

// list binds one parameter of an each: to its array, an element an expansion,
// or to a value that is no array, which every expansion shares as an argument
// beside each. Keys arrive sorted, as slots are, so the arrays stand after the
// arguments in slot order.
func (e *expander) list(c *call, key string, v any, h int) error {
	slot, declared := c.m.slot(key)
	if !declared {
		return fmt.Errorf("each: unknown argument %s", quote(key))
	}
	list, zipped := v.([]any)
	switch {
	case c.given&(1<<slot) != 0:
		return fmt.Errorf("each: argument %s is also given directly", quote(key))
	case !zipped:
		if err := e.put(c, slot, v, h); err != nil {
			return fmt.Errorf("each: %w", err)
		}
		return nil
	case c.each != 0 && len(list) != c.count:
		return fmt.Errorf("each: argument %s has %d elements, the others %d", quote(key), len(list), c.count)
	}
	for i, x := range list {
		if k := c.m.params[slot].kind; !k.holds(x) {
			return fmt.Errorf("each: argument %s[%d]: want %s, got %s", quote(key), i, kindNames[k], kindName(x))
		}
	}
	e.args = append(e.args, arg{list, h})
	c.each, c.count = c.each|1<<slot, len(list)
	return nil
}

// run instantiates c's body once, or once per element of its lists, zipped, and
// ends the call. In an array each result is appended to dst, an array spliced in.
func (e *expander) run(c *call, d int, dst *[]any) (v any, h int, err error) {
	m := c.m
	if missing := m.required &^ (c.given | c.each); missing != 0 {
		name := m.names[bits.TrailingZeros64(missing)]
		return nil, 0, in(fmt.Errorf("missing argument %s", quote(name)), m.name)
	}
	c.given |= c.each
	e.stack = append(e.stack, m.name)
	for i := range c.count {
		next := c.base + len(m.params)
		for each := c.each; each != 0; each &= each - 1 {
			l := e.args[next]
			e.args[c.base+bits.TrailingZeros64(each)] = arg{l.v.([]any)[i], max(l.h-1, 0)}
			next++
		}
		x, xh, err := e.inst(&m.body, c, d)
		if err != nil {
			return nil, 0, in(at(err, m.part), m.name)
		}
		list, splice := x.([]any)
		switch {
		case dst == nil:
			v, h = x, xh
		case splice:
			*dst, h = append(*dst, list...), max(h, xh-1)
		default:
			*dst, h = append(*dst, x), max(h, xh)
		}
	}
	e.stack = e.stack[:len(e.stack)-1]
	clear(e.args[c.base:])
	e.args = e.args[:c.base]
	return v, h, nil
}

// use reads a parameter: its argument, else its default. For a place in the
// result at depth d a table or array is copied, so no two places share one; for
// another invocation it is lent as it stands.
func (e *expander) use(c *call, slot, d int, place bool) (v any, h int, err error) {
	if c.given&(1<<slot) != 0 {
		a := e.args[c.base+slot]
		v, h = a.v, a.h
	} else if def := &c.m.params[slot].def; def.op == opValue || def.op == opStatic {
		v, h = def.value, def.height
	} else {
		return e.inst(def, nil, d) // a default that invokes, expanded for this use
	}
	if !place {
		return v, h, nil
	}
	if d+h > maxNesting {
		return nil, 0, errNesting
	}
	n := 1
	if h > 0 {
		v, n = clone(v)
	}
	return v, h, e.spend(n)
}

// inst instantiates a body's node for one call, at depth d of the result
func (e *expander) inst(n *node, c *call, d int) (any, int, error) {
	switch n.op {
	case opValue:
		return n.value, 0, e.spend(1)
	case opStatic:
		if d+n.height > maxNesting {
			return nil, 0, errNesting
		}
		if err := e.spend(n.size); err != nil {
			return nil, 0, err
		}
		v, _ := clone(n.value)
		return v, n.height, nil
	case opArg:
		return e.use(c, n.slot, d, true)
	case opText:
		s, err := e.text(n.segs, c)
		if err != nil {
			return nil, 0, err
		}
		return s, 0, e.spend(1)
	case opArray:
		return e.instArray(n, c, d)
	case opTable:
		t, h, err := e.instTable(n, c, d)
		if err != nil {
			return nil, 0, err
		}
		if _, invokes := t[keyInvoke]; invokes && n.dynamic {
			return e.invokeRaw(t, d, true, nil)
		}
		return t, h, nil
	}
	return e.invokeNode(n, c, d, nil)
}

func (e *expander) instArray(n *node, c *call, d int) (any, int, error) {
	if d >= maxNesting {
		return nil, 0, errNesting
	}
	if err := e.spend(1); err != nil {
		return nil, 0, err
	}
	out, h := make([]any, 0, len(n.elems)), 0
	for i := range n.elems {
		var x any
		var xh int
		var err error
		switch el := &n.elems[i]; {
		case el.op == opInvoke:
			_, xh, err = e.invokeNode(el, c, d+1, &out)
		case el.op == opArg && c.given&(1<<el.slot) == 0 && c.m.params[el.slot].def.op == opInvoke:
			// A default that invokes expands where it is used, here as an element
			_, xh, err = e.invokeNode(&c.m.params[el.slot].def, nil, d+1, &out)
		case el.op == opTable && el.dynamic:
			var t map[string]any
			if t, xh, err = e.instTable(el, c, d+1); err != nil {
				break
			}
			if _, invokes := t[keyInvoke]; invokes {
				_, xh, err = e.invokeRaw(t, d+1, true, &out)
			} else {
				out = append(out, t)
			}
		default:
			if x, xh, err = e.inst(el, c, d+1); err == nil {
				out = append(out, x)
			}
		}
		if err != nil {
			return nil, 0, atIndex(err, i)
		}
		h = max(h, xh)
	}
	return out, h + 1, nil
}

// instTable instantiates a table's fields. A key spelled from arguments is
// checked against the others: two arguments may spell one key.
func (e *expander) instTable(n *node, c *call, d int) (map[string]any, int, error) {
	if d >= maxNesting {
		return nil, 0, errNesting
	}
	if err := e.spend(1); err != nil {
		return nil, 0, err
	}
	out, h := make(map[string]any, len(n.fields)), 0
	for i := range n.fields {
		f := &n.fields[i]
		key := f.key
		if f.segs != nil {
			var err error
			if key, err = e.text(f.segs, c); err != nil {
				return nil, 0, at(err, f.key)
			}
		}
		if _, dup := out[key]; dup && n.dynamic {
			return nil, 0, at(fmt.Errorf("key %s appears twice after substitution", quote(key)), f.key)
		}
		v, vh, err := e.inst(&f.node, c, d+1)
		if err != nil {
			return nil, 0, at(err, f.key)
		}
		out[key], h = v, max(h, vh)
	}
	return out, h + 1, nil
}

// text splices arguments into a template's text: each a string, an integer or
// a boolean
func (e *expander) text(segs []segment, c *call) (string, error) {
	var b strings.Builder
	for i := range segs {
		s := &segs[i]
		if s.slot < 0 {
			b.WriteString(s.text)
			continue
		}
		v := c.m.params[s.slot].value
		if c.given&(1<<s.slot) != 0 {
			v = e.args[c.base+s.slot].v
		}
		switch v := v.(type) {
		case string:
			b.WriteString(v)
		case int64:
			b.WriteString(strconv.FormatInt(v, 10))
		case bool:
			b.WriteString(strconv.FormatBool(v))
		default:
			if kindOf(v) != kindInt {
				return "", fmt.Errorf("%s argument %s cannot be part of a string", kindName(v), quote(c.m.names[s.slot]))
			}
			fmt.Fprint(&b, v)
		}
	}
	return b.String(), nil
}

// clone copies v, arrays as []any, and counts the values in it
func clone(v any) (any, int) {
	n := 1
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			c, m := clone(x)
			out[k], n = c, n+m
		}
		return out, n
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			c, m := clone(x)
			out[i], n = c, n+m
		}
		return out, n
	case []map[string]any:
		out := make([]any, len(v))
		for i, x := range v {
			c, m := clone(x)
			out[i], n = c, n+m
		}
		return out, n
	}
	return v, 1
}

// pathError is an error and where it happened, a step added by each value the
// expansion leaves on its way out, so a path costs nothing until one is shown
type pathError struct {
	steps []step // innermost first
	err   error
}

// step is a key, an array index when indexed, or the macro whose body the
// steps inside it are in
type step struct {
	key     string
	index   int
	indexed bool
	macro   bool
}

func (e *pathError) Unwrap() error { return e.err }

// Error names the invocation path through every macro involved:
// states.A: macro "M": body.on_enter[0]: unknown macro "N". A path of more than
// maxSteps steps shows its outer and inner halves.
func (e *pathError) Error() string {
	var b strings.Builder
	keys := 0 // steps of the path being spelled
	for i := len(e.steps) - 1; i >= 0; i-- {
		if cut := len(e.steps) - maxSteps; cut > 0 && i == len(e.steps)-maxSteps/2-1 {
			b.WriteString(" ... ")
			i, keys = maxSteps/2-1, 0
		}
		switch s := e.steps[i]; {
		case s.macro:
			if b.Len() == 0 {
				b.WriteString("document")
			}
			b.WriteString(": macro " + quote(s.key) + ": ")
			keys = 0
		case s.indexed:
			b.WriteString("[" + strconv.Itoa(s.index) + "]")
			keys++
		default:
			if keys > 0 {
				b.WriteByte('.')
			}
			b.WriteString(spell(s.key))
			keys++
		}
	}
	switch {
	case b.Len() == 0:
		b.WriteString("document: ")
	case keys > 0:
		b.WriteString(": ")
	}
	b.WriteString(e.err.Error())
	return b.String()
}

func locate(err error, s step) error {
	if err == nil {
		return nil
	}
	pe, ok := err.(*pathError)
	if !ok {
		pe = &pathError{err: err}
	}
	pe.steps = append(pe.steps, s)
	return pe
}

func at(err error, key string) error   { return locate(err, step{key: key}) }
func atIndex(err error, i int) error   { return locate(err, step{index: i, indexed: true}) }
func in(err error, macro string) error { return locate(err, step{key: macro, macro: true}) }

// quote spells text a document supplied for an error: quoted, and cut to 32 bytes
func quote(s string) string {
	if len(s) > 32 {
		return fmt.Sprintf("%q... (%d bytes)", s[:32], len(s))
	}
	return strconv.Quote(s)
}

// spell is a key or a name in an error's path: as written when it is a short
// bare key, else quoted
func spell(s string) string {
	for _, c := range s {
		if c != '_' && c != '-' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return quote(s)
		}
	}
	if s == "" || len(s) > 32 {
		return quote(s)
	}
	return s
}

func validName(s string) bool {
	for i, c := range s {
		letter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !letter && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return s != ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Package macro expands macros in a parsed TOML document, and folds a value
// back into them. A macro is data under the document's [macros] table; a table
// carrying a "macro" key invokes one and is replaced by its body, the arguments
// substituted. Both read nothing but what they are given, so equal bytes expand
// to equal trees on every peer. Syntax: the Macros section of the module README.
package macro

import (
	"cmp"
	"errors"
	"fmt"
	"math"
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

// foldSteps bounds a fold: the steps it may take, a value read, compared or
// written, for each value of its input times each value the set's macros hold
const foldSteps = 64

// part is a table or array of a value being folded: its keys sorted or its
// elements, a scalar as it stands and a table or array a part of its own
type part struct {
	table bool
	lent  bool // an array of values an each took, the fold's own until a fit keeps it
	run   bool // an element that folds into a run with its neighbours
	zero  bool // a value a key may leave out: an empty array, a table of nothing else
	keys  []string
	kids  []any
	cost  int   // the values it is written in once folded, a zero under a key none
	fit   *fit  // a table: the invocation it folds to, nil for none
	runs  []run // an array: the elements that fold together
}

// fit is a macro and the arguments under which it expands to a part, or to a
// run of an array's elements up to to
type fit struct {
	m    *macro
	args []any // by slot: a scalar, a part, or left for one its default gives
	cost int   // the values the invocation writes
	save int   // those it writes fewer than what it replaces
	n    int   // its arguments
	to   int
}

type run struct {
	from int
	fit
}

// shape is a macro that expands to a table, and the keys the table may hold as
// a set of bits, one a key by its hash: every bit where arguments spell a key
type shape struct {
	m    *macro
	keys uint64
}

// left stands for an argument an invocation leaves to its default, and unknown
// for a zero an each took of an any parameter: written 0, unless a value that
// is there says which zero it is
var (
	left    = new(part)
	unknown = &part{cost: 1, zero: true}
)

// slot is a parameter while a body is matched: free until a value is found for
// it, bound to one of the input's, blank where all that is known of an any
// parameter is that it is some zero, plain with one of the set's values, or
// given by the node an invocation or a default wrote, read in its own frame
type slot struct {
	state uint8
	def   bool // given by the parameter's default
	val   any
	node  *node
	in    frame
}

const (
	free uint8 = iota
	bound
	blank
	plain
	given
)

// frame is a macro whose body is being matched: its slots are slots[base:]
type frame struct {
	m    *macro
	base int
}

type folder struct {
	own     Set // the set's macros under a budget of the fold's own
	exp     expander
	tables  []shape  // by name: the macros a table may be an invocation of
	splices []*macro // those whose expansion an array takes element by element
	next    []int    // where in an array each of splices is tried next
	count   int      // the values read
	parts   int      // the tables and arrays among them
	named   int      // the keys of the tables
	steps   int
	limit   int
	over    bool
	careful bool // every fit is expanded before it stands
	depth   int  // invocations entered
	eager   int  // elements taken by an each whose length the body leaves open
	blanks  int  // the values the body in hand writes under keys that hold a zero
	void    int  // keys entered that hold a zero, or are left out for one
	shut    int  // parts entered that are folded, whose own keys are not written
	slots   []slot
	trail   []int  // the slots bound, so a failed expansion of an each is undone
	pool    []part // what read has yet to take: parts, their keys, their values
	keys    []string
	kids    []any
	seen    []bool
	ints    []int
	vals    []any
	lists   []part
	elems   []any
	args    []any
	fits    []fit
}

// Fold returns v, an expanded value, with every table and every run of an
// array's elements that an invocation of one of the set's macros expands to
// replaced by that invocation: the inverse of Expand, its rules in the README.
// v is not modified and the result shares nothing with it. What no macro
// expands to stays as it is; a fold that passes its bound is an error.
func (s *Set) Fold(v any) (any, error) {
	if t, ok := v.(map[string]any); ok {
		if _, ok := t[keyMacros]; ok {
			return nil, errDefinitions
		}
	}
	f := folder{own: Set{MaxValues: s.MaxValues, macros: s.macros}}
	f.exp.set = &f.own
	if err := f.tally(v, 0); err != nil {
		return nil, err
	}
	f.pool, f.keys, f.kids = make([]part, f.parts+1), make([]string, f.named), make([]any, f.count)
	root := f.read(v)
	weight := 1
	for _, name := range sortedKeys(s.macros) {
		m := s.macros[name]
		weight += 1 + m.body.weight()
		for i := range m.params {
			weight += m.params[i].def.weight()
		}
		n := s.result(m, 0)
		table, _ := n.value.(map[string]any)
		switch _, list := n.value.([]any); {
		case n.op == opArray || list:
			f.splices = append(f.splices, m)
		case table != nil:
			keys := uint64(0)
			for k := range table {
				keys |= bit(k)
			}
			f.tables = append(f.tables, shape{m, keys})
		case n.op == opTable:
			keys := uint64(0)
			for i := range n.fields {
				keys |= bit(n.fields[i].key)
			}
			if n.dynamic {
				keys = math.MaxUint64
			}
			f.tables = append(f.tables, shape{m, keys})
		}
	}
	f.next = make([]int, len(f.splices))
	if f.limit = math.MaxInt; f.count < f.limit/foldSteps/weight {
		f.limit = foldSteps * (f.count + 1) * weight
	}
	// A fit is trusted first and the whole result expanded once; only a result
	// that does not come back has every fit expanded as it is chosen
	for range 2 {
		f.fold(root, 0)
		if out := f.write(root); f.holds(out, root, nil, 0) {
			return out, nil
		}
		f.careful = true
	}
	if f.over {
		return nil, fmt.Errorf("fold exceeds %d steps", f.limit)
	}
	out, _ := clone(v)
	return out, nil
}

// weight is the values a template holds
func (n *node) weight() int {
	if n.op == opStatic {
		return n.size
	}
	w := 1
	for i := range n.elems {
		w += n.elems[i].weight()
	}
	for i := range n.fields {
		w += n.fields[i].node.weight()
	}
	return w
}

// result is the node a macro's body writes its value from, through any base
func (s *Set) result(m *macro, depth int) *node {
	n := &m.body
	if n.op == opInvoke && depth < maxDepth {
		name, _ := n.elems[0].value.(string)
		if b := s.macros[name]; b != nil {
			return s.result(b, depth+1)
		}
	}
	return n
}

// wrote counts a value the body in hand writes under a key holding a zero
func (f *folder) wrote() {
	if f.void > 0 && f.shut == 0 {
		f.blanks++
	}
}

func (f *folder) step() bool {
	if f.steps++; f.steps > f.limit {
		f.over = true
	}
	return !f.over
}

// tally counts the values of v, its tables and arrays, and the keys of its
// tables, so that read takes every part from three arrays
func (f *folder) tally(v any, d int) error {
	f.count++
	var err error
	switch v := v.(type) {
	case map[string]any:
		f.parts, f.named = f.parts+1, f.named+len(v)
		for _, x := range v {
			if err = cmp.Or(err, f.tally(x, d+1)); d >= maxNesting {
				return errNesting
			}
		}
	case []any:
		f.parts++
		for _, x := range v {
			if err = cmp.Or(err, f.tally(x, d+1)); d >= maxNesting {
				return errNesting
			}
		}
	case []map[string]any:
		f.parts++
		for _, x := range v {
			if err = cmp.Or(err, f.tally(x, d+1)); d >= maxNesting {
				return errNesting
			}
		}
	}
	return err
}

// read copies the shape of v into parts
func (f *folder) read(v any) any {
	p := &f.pool[0]
	switch v := v.(type) {
	case map[string]any:
		f.pool, p.table, p.keys, p.kids = f.pool[1:], true, f.keys[:0:len(v)], f.kids[:len(v):len(v)]
		f.keys, f.kids = f.keys[len(v):], f.kids[len(v):]
		for k := range v {
			p.keys = append(p.keys, k)
		}
		slices.Sort(p.keys)
		for i, k := range p.keys {
			p.kids[i] = f.read(v[k])
		}
	case []any:
		f.pool, p.kids, f.kids = f.pool[1:], f.kids[:len(v):len(v)], f.kids[len(v):]
		for i, x := range v {
			p.kids[i] = f.read(x)
		}
	case []map[string]any:
		f.pool, p.kids, f.kids = f.pool[1:], f.kids[:len(v):len(v)], f.kids[len(v):]
		for i, x := range v {
			p.kids[i] = f.read(x)
		}
	default:
		return v
	}
	return p
}

// fold settles what x and every part under it are written as, the inner first:
// an argument is itself a folded part, so a fit's cost is known when it is met
func (f *folder) fold(x any, d int) {
	p, ok := x.(*part)
	if !ok || f.over {
		return
	}
	for _, k := range p.kids {
		f.fold(k, d+1)
	}
	// A key holding a zero costs nothing, there or not, so a value costs the same
	// in every shape the fold calls the same, and folds the same from each
	p.fit, p.runs, p.run, p.cost, p.zero = nil, p.runs[:0], false, 1, p.table || len(p.kids) == 0
	keys := uint64(0)
	for i, k := range p.kids {
		if !p.table {
			p.cost += cost(k)
		} else if keys |= bit(p.keys[i]); !f.zero(k) {
			p.cost, p.zero = p.cost+cost(k), false
		}
	}
	if p.table {
		f.table(p, keys, d)
	} else {
		f.array(p, d)
	}
}

func cost(x any) int {
	if p, ok := x.(*part); ok {
		return p.cost
	}
	return 1
}

// table tries every macro on a table whose keys are keys. A macro writes them
// only if its own include them all, so most are refused before anything is
// matched; a table holding nothing but zeros is left as it is, or it would
// fold to an invocation holding one, without end.
func (f *folder) table(p *part, keys uint64, d int) {
	for i := 0; i < len(f.tables) && !p.zero && !f.over; i++ {
		if keys&^f.tables[i].keys != 0 {
			continue
		}
		root := f.enter(f.tables[i].m)
		if f.match(&root.m.body, root, p, true) {
			f.found(root, p.cost, 0)
		}
		f.leave(root)
	}
	if p.fit = f.choose(p, 0, d); p.fit != nil {
		p.cost = p.fit.cost
	}
}

// bit is a key's place in a set of keys
func bit(key string) uint64 {
	h := uint32(2166136261)
	for i := range len(key) {
		h = (h ^ uint32(key[i])) * 16777619
	}
	return 1 << (h >> 26)
}

// array tries every macro that expands to an array on the elements from each
// position. One whose each ran over elements is tried again only past them, so
// a long run is read once, not once from each of its elements.
func (f *folder) array(p *part, d int) {
	clear(f.next)
	for pos := 0; pos < len(p.kids) && len(f.splices) > 0; pos++ {
		for i := 0; i < len(f.splices) && !f.over; i++ {
			if f.next[i] > pos {
				continue
			}
			root := f.enter(f.splices[i])
			f.eager = 0
			if to, ok := f.splice(&root.m.body, root, p.kids, pos, len(p.kids)); ok && to > pos {
				most := 0
				for _, k := range p.kids[pos:to] {
					most += cost(k)
				}
				f.found(root, most, to)
			}
			f.leave(root)
			f.next[i] = pos + f.eager
		}
		if c := f.choose(p, pos, d); c != nil {
			for _, k := range p.kids[pos:c.to] {
				if q, ok := k.(*part); ok {
					q.run = true
				}
			}
			p.runs = append(p.runs, run{pos, *c})
			p.cost -= c.save
			pos = c.to - 1
		}
	}
}

// found notes the macro whose body just matched, unless its invocation would
// write more than the most values it replaces: its arguments, one equal to its
// default left out, and one nothing settled given its type's zero value
func (f *folder) found(fr frame, most, to int) {
	m, at, blanks := fr.m, len(f.args), f.blanks
	c := fit{m: m, cost: 2, to: to}
	for i := range m.params {
		s, v := f.slots[fr.base+i], any(left)
		switch required := m.required>>i&1 != 0; {
		case s.state != free && (required || !f.defaulted(&m.params[i], s.val, s.state == bound)):
			v = s.val
		case s.state == free && required:
			v = zero(m.params[i].kind)
		}
		if v != any(left) {
			c.cost, c.n = c.cost+cost(v), c.n+1
		}
		f.args = append(f.args, v)
	}
	// What the invocation replaces is the table as its macro writes it, the
	// zeros a struct leaves out among them, so that an entry the author wrote
	// as an invocation stays one from a struct's shape
	if c.save = most - c.cost; c.save+blanks < 0 {
		f.args = f.args[:at]
		return
	}
	c.args = f.args[at:]
	f.fits = append(f.fits, c)
}

// defaulted reports v is what leaving the argument out gives, or with here
// false that any zero is. An array is not where the default invokes: as an
// array's element that would be spliced.
func (f *folder) defaulted(p *param, v any, here bool) bool {
	if a, ok := v.(*part); ok && !a.table && p.def.op == opInvoke {
		return false
	}
	return f.match(&p.def, frame{}, v, here)
}

// choose takes the fit found for p, or for its elements from pos, that saves
// the most values, then has the fewest arguments, then the first name; when
// careful, the first of them that expands to what it replaces
func (f *folder) choose(p *part, pos, d int) (best *fit) {
	slices.SortFunc(f.fits, func(a, b fit) int {
		return cmp.Or(cmp.Compare(b.save, a.save), cmp.Compare(a.n, b.n), cmp.Compare(a.m.name, b.m.name))
	})
	for i := range f.fits {
		c := &f.fits[i]
		var kids []any
		if !p.table {
			kids = p.kids[pos:c.to]
		}
		if !f.careful || f.holds(f.invocation(c), p, kids, d) {
			best = &fit{}
			*best = *c
			best.args = slices.Clone(c.args)
			for i, v := range best.args {
				best.args[i] = keep(v)
			}
			break
		}
	}
	f.fits, f.args, f.lists, f.elems = f.fits[:0], f.args[:0], f.lists[:0], f.elems[:0]
	return best
}

// keep is an argument of a fit that stands, an array the fold lent it copied:
// the fold takes the next part's arrays from the same place
func keep(v any) any {
	p, ok := v.(*part)
	if !ok || !p.lent {
		return v
	}
	out := &part{kids: slices.Clone(p.kids), cost: p.cost, zero: p.zero}
	for i, k := range out.kids {
		out.kids[i] = keep(k)
	}
	return out
}

// holds expands v with the expander itself and reports the result is what it
// replaces: x, or kids where v is an array's element at depth d. What it spends
// counts toward the fold's bound and stays within a budget of the set's size.
func (f *folder) holds(v, x any, kids []any, d int) bool {
	if f.over {
		return false
	}
	e := &f.exp
	e.limit = min(cmp.Or(f.own.MaxValues, DefaultMaxValues), f.limit-f.steps)
	e.stack, e.args, e.keys, f.own.spent = e.stack[:0], e.args[:0], e.keys[:0], 0
	var out any
	var err error
	if kids == nil {
		out, _, err = e.walk(v, d)
	} else if out, _, err = e.element(v, 0, d, nil, 0); err == nil && len(out.([]any)) != len(kids) {
		err = errEach
	}
	if f.steps += f.own.spent; err != nil || !f.step() {
		return false
	}
	if kids == nil {
		return f.same(out, x)
	}
	for i, k := range kids {
		if !f.same(out.([]any)[i], k) {
			return false
		}
	}
	return true
}

// write is x as it folds: a value of the caller's own
func (f *folder) write(x any) any {
	p, ok := x.(*part)
	switch {
	case !ok:
		return x
	case p == unknown:
		return int64(0)
	case p.fit != nil:
		return f.invocation(p.fit)
	case p.table:
		out := make(map[string]any, len(p.keys))
		for i, k := range p.keys {
			out[k] = f.write(p.kids[i])
		}
		return out
	}
	out, runs := make([]any, 0, len(p.kids)), p.runs
	for i := 0; i < len(p.kids); i++ {
		if len(runs) > 0 && runs[0].from == i {
			out, i, runs = append(out, f.invocation(&runs[0].fit)), runs[0].to-1, runs[1:]
		} else {
			out = append(out, f.write(p.kids[i]))
		}
	}
	return out
}

func (f *folder) invocation(c *fit) any {
	out := make(map[string]any, c.n+1)
	out[keyInvoke] = c.m.name
	for i, v := range c.args {
		if v != any(left) {
			out[c.m.names[i]] = f.write(v)
		}
	}
	return out
}

// enter opens a frame for m, its parameters free
func (f *folder) enter(m *macro) frame {
	base := len(f.slots)
	if base == 0 {
		f.trail, f.blanks, f.void = f.trail[:0], 0, 0
	}
	f.slots = append(f.slots, make([]slot, len(m.params))...)
	f.depth++
	return frame{m, base}
}

func (f *folder) leave(fr frame) {
	clear(f.slots[fr.base:])
	f.slots = f.slots[:fr.base]
	f.depth--
}

// invoke opens the frame of the macro an invocation in a body names, each
// argument given by the node the invocation writes for it. It fails where the
// name is spelled from arguments, which is left alone.
func (f *folder) invoke(n *node, fr frame) (frame, bool) {
	name, _ := n.elems[0].value.(string)
	m := f.own.macros[name]
	if n.elems[0].op != opValue || m == nil || f.depth >= maxDepth {
		return frame{}, false
	}
	// Arguments and parameters are both sorted by name, so they are merged
	in, j := f.enter(m), 0
	for i := range n.fields {
		for j < len(m.names) && m.names[j] < n.fields[i].key {
			j++
		}
		if j == len(m.names) || m.names[j] != n.fields[i].key {
			f.leave(in)
			return frame{}, false
		}
		f.slots[in.base+j] = slot{state: given, node: &n.fields[i].node, in: fr}
	}
	return in, true
}

// defaults gives every parameter still free its default, but those an each
// binds; one with no default is an argument missing
func (f *folder) defaults(in frame, each uint64) bool {
	for i := range in.m.params {
		if s := &f.slots[in.base+i]; s.state == free && each>>i&1 == 0 {
			if in.m.required>>i&1 != 0 {
				return false
			}
			*s = slot{state: given, def: true, node: &in.m.params[i].def}
		}
	}
	return true
}

// match reports the node, instantiated in fr, is x, or with here false is a
// zero value a key may leave out. A free parameter it meets is bound to what
// stands there.
func (f *folder) match(n *node, fr frame, x any, here bool) bool {
	if !f.step() {
		return false
	}
	p, _ := x.(*part)
	shut := 0
	if p != nil && (p.fit != nil || p.run) {
		shut = 1
	}
	if n.op != opStatic && n.op != opArg && n.op != opInvoke {
		f.wrote()
	}
	switch n.op {
	case opValue:
		if !here {
			return zeroScalar(n.value)
		}
		return sameScalar(n.value, x)
	case opStatic:
		if !here {
			return f.blank(n.value)
		}
		return f.same(n.value, x)
	case opArg:
		return f.arg(fr, n.slot, x, here)
	case opText:
		s, ok := x.(string)
		return ok && f.text(n.segs, fr, s)
	case opArray:
		if here && (p == nil || p.table) {
			return false
		}
		var kids []any
		if here {
			kids = p.kids
		}
		f.shut += shut
		to, ok := f.elements(n, fr, kids, 0, len(kids))
		f.shut -= shut
		return ok && to == len(kids)
	case opTable:
		if here && (p == nil || !p.table) {
			return false
		}
		var keys []string
		var kids []any
		if here {
			keys, kids = p.keys, p.kids
		}
		base, at := len(f.seen), len(f.ints)
		f.seen, f.shut = append(f.seen, make([]bool, len(keys))...), f.shut+shut
		ok := f.fields(n, fr, keys, kids, base, at)
		f.seen, f.ints, f.shut = f.seen[:base], f.ints[:at], f.shut-shut
		return ok
	}
	if len(n.elems) > 1 {
		return false // each expands only inside an array
	}
	in, ok := f.invoke(n, fr)
	if !ok {
		return false
	}
	ok = f.defaults(in, 0) && f.match(&in.m.body, in, x, here)
	f.leave(in)
	return ok
}

// arg matches x against a parameter: through the node that gives it, against
// the value it holds, or, free, by binding it to x
func (f *folder) arg(fr frame, i int, x any, here bool) bool {
	switch s := f.slots[fr.base+i]; s.state {
	case given:
		return f.match(s.node, s.in, x, here)
	case plain:
		if !here {
			return f.blank(s.val)
		}
		return f.same(s.val, x)
	case bound:
		if f.wrote(); !here {
			return f.zero(s.val)
		}
		return f.equal(s.val, x)
	case blank:
		if here && !f.zero(x) {
			return false
		}
	}
	f.wrote()
	// An absent key is a zero of the parameter's type; of an any parameter it
	// says no more than that, until a key that is there says which zero
	k, state := fr.m.params[i].kind, bound
	if !here {
		if x = zero(k); k == kindAny {
			state = blank
		}
	} else if x, here = take(k, x); !here {
		return false
	}
	f.slots[fr.base+i] = slot{state: state, val: x}
	f.trail = append(f.trail, fr.base+i)
	return true
}

// undo frees the slots bound since mark that are still open
func (f *folder) undo(mark int) {
	for _, i := range f.trail[mark:] {
		if i < len(f.slots) {
			f.slots[i] = slot{}
		}
	}
	f.trail = f.trail[:mark]
}

// take is x as a parameter of kind k takes it, a whole float as the integer an
// int parameter wants, so a value folds the same whichever of the two it holds
func take(k kind, x any) (any, bool) {
	if p, ok := x.(*part); ok {
		return x, k == kindAny || k == kindTable && p.table || k == kindArray && !p.table
	}
	if _, fl, nk := number(x); k == kindInt && nk == kindFloat && float64(int64(fl)) == fl {
		return int64(fl), true
	}
	return x, k.holds(x)
}

// zero is the value an absent key stands for, to a parameter of kind k
func zero(k kind) any {
	switch k {
	case kindArray:
		return &part{cost: 1, zero: true}
	case kindTable:
		return &part{table: true, cost: 1, zero: true}
	case kindBool:
		return false
	case kindFloat:
		return 0.0
	case kindString:
		return ""
	}
	return int64(0)
}

// fields matches a table: its fields under their keys, then those whose key is
// spelled from arguments, and no key of the table left over. A field that is a
// comparison goes before one that reads a table or an array, so a macro that
// does not fit is refused before its long parts are read.
func (f *folder) fields(n *node, fr frame, keys []string, kids []any, base, at int) bool {
	// Fields and keys are both sorted, so a field's key is mostly the next one
	found, next := 0, 0
	for i := range n.fields {
		j, ok := next, next < len(keys) && keys[next] == n.fields[i].key
		if !ok {
			j, ok = slices.BinarySearch(keys, n.fields[i].key)
		}
		if ok = ok && n.fields[i].segs == nil; ok {
			f.seen[base+j], found, next = true, found+1, j+1
		} else {
			j = -1
		}
		f.ints = append(f.ints, j)
	}
	if !n.dynamic && found < len(keys) {
		return false
	}
	for _, deep := range [2]bool{false, true} {
		for i := range n.fields {
			fld := &n.fields[i]
			if fld.segs != nil || deep != (fld.node.op >= opArray) {
				continue
			}
			var x any
			j, void := f.ints[at+i], 1
			if j >= 0 {
				if x = kids[j]; !f.zero(x) {
					void = 0
				}
			}
			f.void += void
			ok := f.match(&fld.node, fr, x, j >= 0)
			if f.void -= void; !ok {
				return false
			}
		}
	}
	for i := range n.fields {
		if fld := &n.fields[i]; fld.segs != nil && !f.spelled(fld, fr, keys, kids, base) {
			return false
		}
	}
	return !slices.Contains(f.seen[base:base+len(keys)], false)
}

// spelled matches a field whose key is spelled from arguments, under the one
// key still untaken that its text fits; none, or more than one, is not settled
func (f *folder) spelled(fld *field, fr frame, keys []string, kids []any, base int) bool {
	at, mark := -1, len(f.trail)
	for j, k := range keys {
		if !f.seen[base+j] && f.text(fld.segs, fr, k) {
			if f.undo(mark); at >= 0 {
				return false
			}
			at = j
		}
	}
	if at < 0 || !f.text(fld.segs, fr, keys[at]) {
		return false
	}
	f.seen[base+at] = true
	if f.zero(kids[at]) {
		f.void++
		defer func() { f.void-- }()
	}
	return f.match(&fld.node, fr, kids[at], true)
}

// text matches s against a template's text. With every reference known it is
// the text they spell; one free reference takes what the text around it
// leaves; more than one is a split nothing settles.
func (f *folder) text(segs []segment, fr frame, s string) bool {
	open := -1
	for i := range segs {
		if t, ok := f.piece(&segs[i], fr); !ok && open >= 0 {
			return false
		} else if !ok {
			open = i
		} else if open < 0 && strings.HasPrefix(s, t) {
			s = s[len(t):]
		} else if open < 0 {
			return false
		}
	}
	if open < 0 {
		return s == ""
	}
	for i := len(segs) - 1; i > open; i-- {
		t, _ := f.piece(&segs[i], fr)
		if !strings.HasSuffix(s, t) {
			return false
		}
		s = s[:len(s)-len(t)]
	}
	var v any = s
	switch k := fr.m.params[segs[open].slot].kind; k {
	case kindInt, kindFloat:
		n, err := strconv.ParseInt(s, 10, 64)
		if v = n; err != nil || strconv.FormatInt(n, 10) != s {
			return false
		}
	case kindBool:
		if v = s == "true"; s != "true" && s != "false" {
			return false
		}
	case kindArray, kindTable:
		return false
	}
	return f.arg(fr, segs[open].slot, v, true)
}

// piece is the text a segment splices in, once it is known: its own, or its
// parameter's value, a string, an integer or a boolean
func (f *folder) piece(seg *segment, fr frame) (string, bool) {
	if seg.slot < 0 {
		return seg.text, true
	}
	s := f.slots[fr.base+seg.slot]
	for s.state == given && s.node.op == opArg {
		s = f.slots[s.in.base+s.node.slot]
	}
	if s.state == given && s.node.op == opValue {
		s.state, s.val = plain, s.node.value
	}
	switch v := s.val.(type) {
	case string:
		return v, s.state == bound || s.state == plain
	case bool:
		return strconv.FormatBool(v), s.state == bound || s.state == plain
	}
	i, _, k := number(s.val)
	return strconv.FormatInt(i, 10), k == kindInt && (s.state == bound || s.state == plain)
}

// resolve follows a reference to the node its parameter is given by, and stops
// at one whose parameter is free or holds a value
func (f *folder) resolve(n *node, fr frame) (*node, frame) {
	for n.op == opArg {
		s := f.slots[fr.base+n.slot]
		if s.state != given {
			break
		}
		n, fr = s.node, s.in
	}
	return n, fr
}

// elements matches a body's array against kids[pos:end] and returns where it
// stops. An invocation takes what it expands to, and leaves an element for each
// plain one after it.
func (f *folder) elements(n *node, fr frame, kids []any, pos, end int) (int, bool) {
	rest := 0
	for i := range n.elems {
		if n.elems[i].op != opInvoke {
			rest++
		}
	}
	for i := range n.elems {
		el, in, ok := &n.elems[i], fr, false
		if el.op != opInvoke {
			rest--
		}
		// A default that invokes expands where it is used, here as an element
		if s := f.slots; el.op == opArg && s[fr.base+el.slot].def && s[fr.base+el.slot].node.op == opInvoke {
			el, in = s[fr.base+el.slot].node, frame{}
		}
		switch {
		case el.op == opInvoke:
			pos, ok = f.each(el, in, kids, pos, end-rest)
		case pos < end-rest && f.match(el, in, kids[pos], true):
			pos, ok = pos+1, true
		}
		if !ok {
			return pos, false
		}
	}
	return pos, true
}

// splice matches what an invocation's body gives an array: the elements of an
// array one by one, any other value as one element
func (f *folder) splice(n *node, fr frame, kids []any, pos, end int) (int, bool) {
	if !f.step() {
		return pos, false
	}
	var list []any
	switch n.op {
	case opArray:
		return f.elements(n, fr, kids, pos, end)
	case opInvoke:
		in, ok := f.invoke(n, fr)
		if !ok || len(n.elems) > 1 {
			return pos, false
		}
		if ok = f.defaults(in, 0); ok {
			pos, ok = f.splice(&in.m.body, in, kids, pos, end)
		}
		f.leave(in)
		return pos, ok
	case opArg:
		switch s := f.slots[fr.base+n.slot]; s.state {
		case given:
			return f.splice(s.node, s.in, kids, pos, end)
		case bound:
			if p, ok := s.val.(*part); ok && !p.table {
				for _, k := range p.kids {
					if pos >= end || !f.equal(k, kids[pos]) {
						return pos, false
					}
					pos++
				}
				return pos, true
			}
		case plain:
			list, _ = s.val.([]any)
		case free, blank:
			return pos, false // one element or several: nothing settles which
		}
	case opStatic:
		list, _ = n.value.([]any)
	}
	if list != nil {
		for _, v := range list {
			if pos >= end || !f.same(v, kids[pos]) {
				return pos, false
			}
			pos++
		}
		return pos, true
	}
	if pos >= end || !f.match(n, fr, kids[pos], true) {
		return pos, false
	}
	return pos + 1, true
}

// each matches an invocation that is an array's element: its body once, or
// once for each element of the arrays in its each. Where every array is an
// argument still free, the body is matched for as long as it fits, and each
// such argument comes back as the values it took.
func (f *folder) each(n *node, fr frame, kids []any, pos, end int) (int, bool) {
	in, ok := f.invoke(n, fr)
	if !ok {
		return pos, false
	}
	defer f.leave(in)
	if len(n.elems) == 1 {
		if !f.defaults(in, 0) {
			return pos, false
		}
		return f.splice(&in.m.body, in, kids, pos, end)
	}
	lists, lf := f.resolve(&n.elems[1], fr)
	if lists.op != opTable || lists.dynamic || len(lists.fields) == 0 {
		return pos, false
	}
	var each, loose uint64 // the slots each binds; the fields whose argument is free
	count, open, at, arrays := -1, 0, len(f.ints), false
	defer func() { f.ints = f.ints[:at] }()
	for i := range lists.fields {
		j, ok := in.m.slot(lists.fields[i].key)
		if !ok || f.slots[in.base+j].state != free {
			return pos, false
		}
		f.ints = append(f.ints, in.base+j)
		each |= 1 << j
		switch _, n := f.nth(&lists.fields[i].node, lf, 0); {
		case n == isFree:
			arg, fr := f.resolve(&lists.fields[i].node, lf)
			k := fr.m.params[arg.slot].kind
			loose, open, arrays = loose|1<<i, open+1, arrays || k == kindAny || k == kindArray
		case n >= 0 && count >= 0 && n != count:
			return pos, false
		case n >= 0:
			count = n
		}
	}
	if !f.defaults(in, each) {
		return pos, false
	}
	// With no array in it, each expands once; so it does where none of its
	// arguments still free can be one
	if count < 0 && !arrays {
		count = 1
	}
	known := count >= 0
	// Where the run stands before any expansion and after each, to come back to
	base, done, unset, stood := len(f.vals), 0, loose, len(f.ints)
	f.ints = append(f.ints, pos, len(f.trail), f.blanks)
	defer func() { f.vals = f.vals[:base] }()
	for ; !known || done < count; done++ {
		mark, blanks := len(f.trail), f.blanks
		for i := range lists.fields {
			f.slots[f.ints[at+i]], _ = f.nth(&lists.fields[i].node, lf, done)
		}
		next, ok := f.splice(&in.m.body, in, kids, pos, end)
		if !ok && known {
			return pos, false
		} else if !ok || next == pos && !known {
			f.undo(mark)
			f.blanks = blanks
			break
		}
		for i := range lists.fields {
			if j := f.ints[at+i]; loose>>i&1 != 0 {
				v := f.slots[j].val
				switch f.slots[j].state {
				case free:
					v = zero(in.m.params[j-in.base].kind)
				case blank:
					v, unset = unknown, unset&^(1<<i)
				default:
					unset &^= 1 << i
				}
				f.vals = append(f.vals, v)
			}
		}
		pos = next
		f.ints = append(f.ints, pos, len(f.trail), f.blanks)
	}
	if !known {
		f.eager += done
	}
	// A run goes as far as its arguments can spell: one that took an element
	// too many gives it back, and the one before, until they can
	for ; ; done-- {
		if f.gather(lists, lf, loose, unset, f.vals[base:base+done*open], done, known) {
			return pos, true
		} else if known || done == 0 {
			return pos, false
		}
		back := f.ints[stood+3*(done-1):]
		f.undo(back[1])
		pos, f.blanks = back[0], back[2]
	}
}

const (
	isShared = -1 - iota // what nth reports of a value that is no array
	isFree
)

// nth is what an each gives a parameter in its expansion i, and the length of
// the array that comes from: isShared for a value every expansion takes,
// isFree for an argument nothing has settled yet
func (f *folder) nth(n *node, fr frame, i int) (slot, int) {
	n, fr = f.resolve(n, fr)
	var list []any
	switch n.op {
	case opArg:
		s := f.slots[fr.base+n.slot]
		if s.state == free {
			return slot{}, isFree
		}
		if p, ok := s.val.(*part); ok && !p.table {
			if i < len(p.kids) {
				s.val = p.kids[i]
			}
			return s, len(p.kids)
		}
		if list, _ = s.val.([]any); list == nil {
			return s, isShared
		}
	case opArray:
		if i < len(n.elems) {
			return slot{state: given, node: &n.elems[i], in: fr}, len(n.elems)
		}
		return slot{}, len(n.elems)
	case opStatic:
		list, _ = n.value.([]any)
	}
	if list == nil {
		return slot{state: given, node: n, in: fr}, isShared
	}
	if i < len(list) {
		return slot{state: plain, val: list[i]}, len(list)
	}
	return slot{}, len(list)
}

// gather binds the arguments an each left free to the values its expansions
// took, done of them for each loose field in turn: their array, or one value
// where they all agree and the parameter takes one, which a field in unset,
// that no expansion read, leaves free. Where no array is left to count the
// expansions by, the first argument that can be one is.
func (f *folder) gather(lists *node, lf frame, loose, unset uint64, vals []any, done int, known bool) bool {
	open, counted := bits.OnesCount64(loose), known || done == 1
	if f.steps += done; !f.step() {
		return false
	}
	var arrays uint64 // the loose fields, in their order, whose values stay an array
	for pass := range 2 {
		c := 0
		for i := range lists.fields {
			if loose>>i&1 == 0 {
				continue
			}
			n, fr := f.resolve(&lists.fields[i].node, lf)
			k := fr.m.params[n.slot].kind
			switch {
			case pass == 0:
				agree := done > 0
				for j := 1; j < done && agree; j++ {
					agree = f.equal(vals[c], vals[j*open+c])
				}
				if agree {
					a, _ := vals[c].(*part)
					_, agree = take(k, vals[c])
					agree = agree && (a == nil || a.table) || a == unknown && k != kindArray
				}
				if !agree && done > 0 {
					arrays, counted = arrays|1<<c, true
				}
			case !counted && (k == kindAny || k == kindArray), arrays>>c&1 != 0:
				at := len(f.elems)
				f.elems = append(f.elems, make([]any, done)...)
				f.lists = append(f.lists, part{kids: f.elems[at:len(f.elems):len(f.elems)], cost: 1, lent: true, zero: done == 0})
				list := &f.lists[len(f.lists)-1]
				for j := range done {
					list.kids[j] = vals[j*open+c]
					list.cost += cost(list.kids[j])
				}
				if counted = true; !f.arg(fr, n.slot, list, true) {
					return false
				}
			case done > 0 && unset>>i&1 == 0:
				// The value they agree on is the first that says what it is
				v := any(unknown)
				for j := 0; j < done && v == any(unknown); j++ {
					v = vals[j*open+c]
				}
				if !f.arg(fr, n.slot, v, v != any(unknown)) {
					return false
				}
			}
			c++
		}
	}
	return counted
}

// same reports v, a value of the set or of an expansion, is x under the fold's
// rule: v may hold a key x leaves out, where it holds a zero value, and an
// integer may stand for the float of its value
func (f *folder) same(v, x any) bool {
	if f.wrote(); !f.step() {
		return false
	}
	p, _ := x.(*part)
	table, _ := v.(map[string]any)
	list, isList := v.([]any)
	switch {
	case p == unknown:
		return f.blank(v)
	case table == nil && !isList:
		return p == nil && sameScalar(v, x)
	case p == nil || p.table == isList || isList && len(list) != len(p.kids):
		return false
	case p.fit != nil || p.run:
		f.shut++
		defer func() { f.shut-- }()
	}
	for i := range list {
		if !f.same(list[i], p.kids[i]) {
			return false
		}
	}
	for i, k := range p.keys {
		w, ok := table[k]
		void := 0
		if f.zero(p.kids[i]) {
			void = 1
		}
		f.void += void
		ok = ok && f.same(w, p.kids[i])
		if f.void -= void; !ok {
			return false
		}
	}
	// Every key is read whatever a map's order, so a fold's steps are its
	// input's alone
	ok := true
	f.void++
	for k, w := range table {
		if _, held := slices.BinarySearch(p.keys, k); len(p.keys) < len(table) && !held {
			ok = f.blank(w) && ok
		}
	}
	f.void--
	return ok
}

// equal is same of a, a value of the fold's input written where b stands
func (f *folder) equal(a, b any) bool {
	if !f.step() {
		return false
	}
	p, _ := a.(*part)
	q, _ := b.(*part)
	switch {
	case p == unknown || q == unknown:
		return f.zero(a) && f.zero(b)
	case p == nil || q == nil:
		return p == nil && q == nil && sameScalar(a, b)
	case p.table != q.table || !p.table && len(p.kids) != len(q.kids):
		return false
	case !p.table:
		for i := range p.kids {
			if !f.equal(p.kids[i], q.kids[i]) {
				return false
			} else if p.kids[i] == any(unknown) {
				p.kids[i] = q.kids[i]
			}
		}
		return true
	}
	j := 0
	for i, k := range p.keys {
		if j < len(q.keys) && q.keys[j] == k {
			if !f.equal(p.kids[i], q.kids[j]) {
				return false
			}
			j++
		} else if !f.zero(p.kids[i]) {
			return false
		}
	}
	return j == len(q.keys)
}

// zero reports x, a value of the fold's input, is one a key may leave out
func (f *folder) zero(x any) bool {
	if p, ok := x.(*part); ok {
		return p.zero
	}
	return zeroScalar(x)
}

// blank is zero of a value of the set or of an expansion, read whole and
// counted where it stands under a key
func (f *folder) blank(v any) bool {
	f.wrote()
	switch v := v.(type) {
	case map[string]any:
		ok := f.step()
		for _, w := range v {
			ok = f.blank(w) && ok
		}
		return ok
	case []any:
		return len(v) == 0
	}
	return zeroScalar(v)
}

func zeroScalar(v any) bool {
	switch v := v.(type) {
	case string:
		return v == ""
	case bool:
		return !v
	}
	i, fl, k := number(v)
	return k != kindOther && i == 0 && fl == 0
}

// sameScalar reports two scalars are one value, an integer and the float of
// exactly its value among them
func sameScalar(a, b any) bool {
	ai, af, ak := number(a)
	bi, bf, bk := number(b)
	switch {
	case ak == kindOther || bk == kindOther:
		k := kindOf(a)
		return (k == kindString || k == kindBool) && a == b
	case ak == bk:
		return ai == bi && af == bf
	case ak == kindInt:
		return int64(bf) == ai && float64(ai) == bf
	}
	return int64(af) == bi && float64(bi) == af
}

// number reads an integer or a float of any Go type a document may hold
func number(v any) (int64, float64, kind) {
	switch v := v.(type) {
	case int64:
		return v, 0, kindInt
	case float64:
		return 0, v, kindFloat
	case int:
		return int64(v), 0, kindInt
	case int8:
		return int64(v), 0, kindInt
	case int16:
		return int64(v), 0, kindInt
	case int32:
		return int64(v), 0, kindInt
	case uint:
		return int64(v), 0, kindInt
	case uint8:
		return int64(v), 0, kindInt
	case uint16:
		return int64(v), 0, kindInt
	case uint32:
		return int64(v), 0, kindInt
	case uint64:
		return int64(v), 0, kindInt
	case float32:
		return 0, float64(v), kindFloat
	}
	return 0, 0, kindOther
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

// Package macro expands macros in a parsed TOML document. A macro is data
// under the document's [macros] table; any table carrying a "macro" key invokes
// one and is replaced by its body with the arguments substituted. Expansion reads
// nothing but the documents it is given, so equal bytes expand to equal trees on
// every peer. Syntax: the Macros section of the module README.
package macro

import (
	"errors"
	"fmt"
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

// Bounds a peer-supplied document cannot exceed: invocation nesting, and the
// values every expansion through one Set may produce together.
const (
	maxDepth = 32
	maxNodes = 1 << 18
)

var errDefinitions = errors.New("[macros] is not allowed in this document")

// Parameter types an argument is checked against. A float parameter takes an
// integer too, as the decoder does.
const (
	typeAny    = "any"
	typeArray  = "array"
	typeBool   = "bool"
	typeFloat  = "float"
	typeInt    = "int"
	typeString = "string"
	typeTable  = "table"
)

var paramTypes = []string{typeAny, typeArray, typeBool, typeFloat, typeInt, typeString, typeTable}

type param struct {
	typ      string
	def      any
	optional bool
}

type macro struct {
	params map[string]param
	body   any
}

// Set holds the macros a group of documents may invoke.
type Set struct {
	macros map[string]*macro
	nodes  int // values substitution has produced, across every Expand
}

func NewSet() *Set { return &Set{macros: make(map[string]*macro)} }

// ExpandDocument expands a document that defines its own macros.
func ExpandDocument(doc map[string]any) (map[string]any, error) {
	s := NewSet()
	if err := s.Define(doc); err != nil {
		return nil, err
	}
	return s.Expand(doc)
}

// Define moves doc's [macros] into the set. A name defined twice is an error
// across documents too, so a library cannot silently shadow its user.
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
	for _, name := range sortedKeys(table) {
		if _, dup := s.macros[name]; dup {
			return fmt.Errorf("macro %q is defined twice", name)
		}
		m, err := define(table[name])
		if err != nil {
			return fmt.Errorf("macro %q: %w", name, err)
		}
		s.macros[name] = m
	}
	return nil
}

func define(raw any) (*macro, error) {
	def, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New("definition must be a table")
	}
	for _, key := range sortedKeys(def) {
		if !slices.Contains([]string{"body", "defaults", "doc", "params"}, key) {
			return nil, fmt.Errorf("unknown key %q", key)
		}
	}
	if _, ok := def["doc"].(string); def["doc"] != nil && !ok {
		return nil, errors.New("doc must be a string")
	}
	body, ok := def["body"]
	if !ok {
		return nil, errors.New("body is missing")
	}
	m := &macro{params: make(map[string]param), body: body}

	params, ok := def["params"].(map[string]any)
	if def["params"] != nil && !ok {
		return nil, errors.New("params must be a table of name = type")
	}
	for _, name := range sortedKeys(params) {
		typ, _ := params[name].(string)
		switch {
		case !validName(name) || name == keyInvoke || name == keyEach:
			return nil, fmt.Errorf("parameter %q: not a usable name", name)
		case !slices.Contains(paramTypes, typ):
			return nil, fmt.Errorf("parameter %q: type must be one of %s", name, strings.Join(paramTypes, ", "))
		}
		m.params[name] = param{typ: typ}
	}

	defaults, ok := def["defaults"].(map[string]any)
	if def["defaults"] != nil && !ok {
		return nil, errors.New("defaults must be a table")
	}
	for _, name := range sortedKeys(defaults) {
		p, ok := m.params[name]
		if !ok {
			return nil, fmt.Errorf("default for undeclared parameter %q", name)
		}
		if !typeHolds(p.typ, defaults[name]) {
			return nil, fmt.Errorf("default for %q: want %s, got %s", name, p.typ, kindOf(defaults[name]))
		}
		p.def, p.optional = defaults[name], true
		m.params[name] = p
	}

	// A reference to an undeclared parameter is a typo; catch it before any use
	if err := m.checkReferences(body, "body"); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *macro) checkReferences(v any, path string) error {
	check := func(s, path string) error {
		segs, err := parseTemplate(s)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for _, seg := range segs {
			if _, ok := m.params[seg.text]; seg.ref && !ok {
				return fmt.Errorf("%s: undeclared parameter %q", path, seg.text)
			}
		}
		return nil
	}
	if s, ok := v.(string); ok {
		return check(s, path)
	}
	if t, ok := v.(map[string]any); ok {
		for _, k := range sortedKeys(t) {
			if err := check(k, join(path, k)); err != nil {
				return err
			}
			if err := m.checkReferences(t[k], join(path, k)); err != nil {
				return err
			}
		}
	}
	list, _ := elements(v)
	for i, el := range list {
		if err := m.checkReferences(el, index(path, i)); err != nil {
			return err
		}
	}
	return nil
}

// Expand returns doc with every invocation replaced; doc itself is not modified.
// Arrays come back as []any, whatever form the parser gave them.
func (s *Set) Expand(doc map[string]any) (map[string]any, error) {
	if _, ok := doc[keyMacros]; ok {
		return nil, errDefinitions
	}
	e := &expander{set: s}
	out, err := e.value(doc, "")
	if err != nil {
		return nil, err
	}
	table, ok := out.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("document expands to a %s, not a table", kindOf(out))
	}
	return table, nil
}

type expander struct {
	set   *Set
	stack []string // macros being expanded, outermost first
}

// value copies v, replacing invocations. In an array, an expansion that is
// itself an array is spliced in, and so is each result of an each.
func (e *expander) value(v any, path string) (any, error) {
	if list, ok := elements(v); ok {
		out := make([]any, 0, len(list))
		for i, el := range list {
			inv, ok := el.(map[string]any)
			if _, invokes := inv[keyInvoke]; !ok || !invokes {
				x, err := e.value(el, index(path, i))
				if err != nil {
					return nil, err
				}
				out = append(out, x)
				continue
			}
			results, err := e.invoke(inv, index(path, i))
			if err != nil {
				return nil, err
			}
			for _, r := range results {
				if spliced, ok := r.([]any); ok {
					out = append(out, spliced...)
				} else {
					out = append(out, r)
				}
			}
		}
		return out, nil
	}
	t, ok := v.(map[string]any)
	if !ok {
		return v, nil
	}
	if _, ok := t[keyInvoke]; ok {
		if _, each := t[keyEach]; each {
			return nil, fmt.Errorf("%s: each expands only inside an array", at(path))
		}
		results, err := e.invoke(t, path)
		if err != nil {
			return nil, err
		}
		return results[0], nil
	}
	out := make(map[string]any, len(t))
	for _, k := range sortedKeys(t) {
		x, err := e.value(t[k], join(path, k))
		if err != nil {
			return nil, err
		}
		out[k] = x
	}
	return out, nil
}

// invoke binds one invocation's arguments and expands its body once, or once
// per element of the arrays its each table names, zipped.
func (e *expander) invoke(inv map[string]any, path string) ([]any, error) {
	name, ok := inv[keyInvoke].(string)
	if !ok {
		return nil, fmt.Errorf("%s: macro name must be a string", at(path))
	}
	m, ok := e.set.macros[name]
	switch {
	case !ok:
		return nil, fmt.Errorf("%s: unknown macro %q", at(path), name)
	case slices.Contains(e.stack, name):
		return nil, fmt.Errorf("%s: macro %q invokes itself through %s", at(path), name, strings.Join(e.stack, " → "))
	case len(e.stack) >= maxDepth:
		return nil, fmt.Errorf("%s: macros nest deeper than %d", at(path), maxDepth)
	}
	fail := func(err error) ([]any, error) { return nil, fmt.Errorf("%s: macro %q: %w", at(path), name, err) }

	// Arguments are expanded where they are written, before binding
	args := make(map[string]any, len(m.params))
	for _, k := range sortedKeys(inv) {
		if k == keyInvoke || k == keyEach {
			continue
		}
		p, ok := m.params[k]
		if !ok {
			return fail(fmt.Errorf("unknown argument %q", k))
		}
		v, err := e.value(inv[k], join(path, k))
		if err != nil {
			return nil, err
		}
		if !typeHolds(p.typ, v) {
			return fail(fmt.Errorf("argument %q: want %s, got %s", k, p.typ, kindOf(v)))
		}
		args[k] = v
	}
	each, count, err := e.eachArgs(inv, m, args, join(path, keyEach))
	if err != nil {
		return fail(err)
	}
	for _, k := range sortedKeys(m.params) {
		_, given := args[k]
		_, iterated := each[k]
		switch p := m.params[k]; {
		case given || iterated:
		case p.optional:
			args[k] = p.def
		default:
			return fail(fmt.Errorf("missing argument %q", k))
		}
	}

	e.stack = append(e.stack, name)
	defer func() { e.stack = e.stack[:len(e.stack)-1] }()
	results := make([]any, 0, count)
	for i := range count {
		for _, k := range sortedKeys(each) {
			args[k] = each[k][i]
		}
		body, err := e.subst(m.body, args, "body")
		if err != nil {
			return fail(err)
		}
		out, err := e.value(body, "body")
		if err != nil {
			return fail(err)
		}
		results = append(results, out)
	}
	return results, nil
}

// eachArgs reads an invocation's each table: parameter = array, all one length.
// Without one, the body expands once.
func (e *expander) eachArgs(inv map[string]any, m *macro, args map[string]any, path string) (map[string][]any, int, error) {
	raw, ok := inv[keyEach]
	if !ok {
		return nil, 1, nil
	}
	expanded, err := e.value(raw, path)
	if err != nil {
		return nil, 0, err
	}
	table, ok := expanded.(map[string]any)
	if !ok || len(table) == 0 {
		return nil, 0, errors.New("each must be a table of parameter = array")
	}
	each, count := make(map[string][]any, len(table)), -1
	for _, k := range sortedKeys(table) {
		p, declared := m.params[k]
		_, given := args[k]
		list, isArray := table[k].([]any)
		switch {
		case !declared:
			return nil, 0, fmt.Errorf("each: unknown argument %q", k)
		case given:
			return nil, 0, fmt.Errorf("each: argument %q is also given directly", k)
		case !isArray:
			return nil, 0, fmt.Errorf("each: argument %q must be an array", k)
		case count >= 0 && len(list) != count:
			return nil, 0, fmt.Errorf("each: argument %q has %d elements, the others %d", k, len(list), count)
		}
		for i, v := range list {
			if !typeHolds(p.typ, v) {
				return nil, 0, fmt.Errorf("each: argument %q[%d]: want %s, got %s", k, i, p.typ, kindOf(v))
			}
		}
		each[k], count = list, len(list)
	}
	return each, count, nil
}

// subst copies v with ${name} replaced. A string that is one reference becomes
// the argument itself, of any type; elsewhere an argument is spliced into the
// text and must be a string, an integer or a boolean. Nil args copies v verbatim,
// which is how an argument is copied: its text is never a template.
func (e *expander) subst(v any, args map[string]any, path string) (any, error) {
	if e.set.nodes++; e.set.nodes > maxNodes {
		return nil, fmt.Errorf("expansion exceeds %d values", maxNodes)
	}
	template := func(s, path string) ([]segment, error) {
		if args == nil {
			return []segment{{text: s}}, nil
		}
		segs, err := parseTemplate(s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return segs, nil
	}
	if list, ok := elements(v); ok {
		out := make([]any, len(list))
		for i, el := range list {
			x, err := e.subst(el, args, index(path, i))
			if err != nil {
				return nil, err
			}
			out[i] = x
		}
		return out, nil
	}
	switch v := v.(type) {
	case string:
		segs, err := template(v, path)
		if err != nil {
			return nil, err
		}
		if len(segs) == 1 && segs[0].ref {
			// Every use gets its own copy, so a consumer editing one cannot reach another
			return e.subst(args[segs[0].text], nil, path)
		}
		return interpolate(segs, args, path)
	case map[string]any:
		out := make(map[string]any, len(v))
		for _, k := range sortedKeys(v) {
			segs, err := template(k, join(path, k))
			if err != nil {
				return nil, err
			}
			key, err := interpolate(segs, args, join(path, k))
			if err != nil {
				return nil, err
			}
			if _, dup := out[key]; dup {
				return nil, fmt.Errorf("%s: key %q appears twice after substitution", join(path, k), key)
			}
			if out[key], err = e.subst(v[k], args, join(path, k)); err != nil {
				return nil, err
			}
		}
		return out, nil
	default:
		return v, nil
	}
}

func interpolate(segs []segment, args map[string]any, path string) (string, error) {
	var b strings.Builder
	for _, seg := range segs {
		if !seg.ref {
			b.WriteString(seg.text)
			continue
		}
		switch v := args[seg.text].(type) {
		case string:
			b.WriteString(v)
		case int64:
			b.WriteString(strconv.FormatInt(v, 10))
		case bool:
			b.WriteString(strconv.FormatBool(v))
		default:
			return "", fmt.Errorf("%s: %s argument %q cannot be part of a string", path, kindOf(v), seg.text)
		}
	}
	return b.String(), nil
}

type segment struct {
	text string
	ref  bool // text names a parameter
}

// parseTemplate splits s into literal text and ${name} references; $$ is a literal $.
func parseTemplate(s string) ([]segment, error) {
	if !strings.Contains(s, "$") {
		return []segment{{text: s}}, nil
	}
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
				return nil, fmt.Errorf("${%s} does not name a parameter", name)
			}
			if lit.Len() > 0 {
				segs = append(segs, segment{text: lit.String()})
				lit.Reset()
			}
			segs = append(segs, segment{text: name, ref: true})
			i += end + 1
		default:
			return nil, errors.New("a literal $ is written $$")
		}
	}
	if lit.Len() > 0 || len(segs) == 0 {
		segs = append(segs, segment{text: lit.String()})
	}
	return segs, nil
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

func typeHolds(typ string, v any) bool {
	k := kindOf(v)
	return typ == typeAny || typ == k || (typ == typeFloat && k == typeInt)
}

func kindOf(v any) string {
	switch v.(type) {
	case int64:
		return typeInt
	case float64:
		return typeFloat
	case bool:
		return typeBool
	case string:
		return typeString
	case map[string]any:
		return typeTable
	case []any, []map[string]any:
		return typeArray
	default:
		return fmt.Sprintf("%T", v)
	}
}

// elements views either array form the parser produces as []any.
func elements(v any) ([]any, bool) {
	switch v := v.(type) {
	case []any:
		return v, true
	case []map[string]any:
		out := make([]any, len(v))
		for i, t := range v {
			out[i] = t
		}
		return out, true
	default:
		return nil, false
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func index(path string, i int) string { return fmt.Sprintf("%s[%d]", path, i) }

func at(path string) string {
	if path == "" {
		return "document"
	}
	return path
}

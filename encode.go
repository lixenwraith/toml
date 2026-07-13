package toml

import (
	"bytes"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Marshal returns the TOML encoding of v
//
// Marshal supports struct and map types as the root object
// It follows standard TOML formatting:
//   - Comments and whitespace are not preserved from original decoding
//   - Dates are not supported (as per requirements).
//   - Nil pointers are skipped
//   - Unexported fields are skipped
//   - Fields with `omitempty` are skipped if zero
//   - Struct fields/Map keys are sorted alphabetically for determinism
//   - Fully numerical keys are rejected
func Marshal(v any) ([]byte, error) {
	val := reflect.ValueOf(v)

	// Dereference pointer if necessary
	if val.Kind() == reflect.Ptr {
		if val.IsNil() {
			return nil, fmt.Errorf("marshal: cannot marshal nil pointer")
		}
		val = val.Elem()
	}

	// Root must be a Table (Struct or Map)
	if val.Kind() != reflect.Struct && val.Kind() != reflect.Map {
		return nil, fmt.Errorf("marshal: root must be struct or map, got %v", val.Kind())
	}

	buf := new(bytes.Buffer)
	enc := &encoder{w: buf}

	if err := enc.encodeTable(val, ""); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

type encoder struct {
	w *bytes.Buffer
}

// encodeTable writes the fields of a struct or map.
// It uses a two-pass approach:
// 1. Write all scalar values (primitives, inline arrays).
// 2. Recurse and write nested tables (structs, maps, arrays of tables).
// This ensures valid TOML where keys are defined before sub-tables.
func (e *encoder) encodeTable(rv reflect.Value, prefix string) error {
	// 1. Gather keys
	keys, err := e.getSortedKeys(rv)
	if err != nil {
		return err
	}

	// 2. Separation: Identify which keys are scalars (printed now) vs tables (printed later)
	var scalars []string
	var tables []string

	for _, k := range keys {
		fieldVal := e.resolveValue(rv, k)
		if !fieldVal.IsValid() {
			continue // Skip invalid/nil
		}

		// Check if we should skip (omitempty, unexported handled in getSortedKeys)
		if e.shouldSkip(rv, k, fieldVal) {
			continue
		}

		// isTable validates slice homogeneity
		isTab, err := e.isTable(fieldVal)
		if err != nil {
			return fmt.Errorf("key %q: %w", e.getKeyName(rv, k), err)
		}
		if isTab {
			tables = append(tables, k)
		} else {
			scalars = append(scalars, k)
		}
	}

	// 3. Pass 1: Write Scalars
	for _, k := range scalars {
		val := e.resolveValue(rv, k)
		keyName := e.getKeyName(rv, k)

		if err := e.writeKey(keyName); err != nil {
			return err
		}
		if _, err := e.w.WriteString(" = "); err != nil {
			return err
		}
		if err := e.encodeValue(val); err != nil {
			return fmt.Errorf("key %q: %w", keyName, err)
		}
		e.w.WriteString("\n")
	}

	// 4. Pass 2: Write Tables
	for _, k := range tables {
		val := e.resolveValue(rv, k)
		keyName := e.getKeyName(rv, k)

		// Determine full path for header
		fullKey := keyName
		if prefix != "" {
			fullKey = prefix + "." + keyName
		}

		// Handle specific table types
		switch val.Kind() {
		case reflect.Struct, reflect.Map:
			// [header]
			e.w.WriteString("\n")
			e.w.WriteString("[" + fullKey + "]\n")
			if err := e.encodeTable(val, fullKey); err != nil {
				return err
			}

		case reflect.Slice, reflect.Array:
			// [[header]]
			for i := 0; i < val.Len(); i++ {
				elem := val.Index(i)
				// Dereference pointer elements in slice
				if elem.Kind() == reflect.Ptr {
					if elem.IsNil() {
						continue
					}
					elem = elem.Elem()
				}

				e.w.WriteString("\n")
				e.w.WriteString("[[" + fullKey + "]]\n")
				if err := e.encodeTable(elem, fullKey); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// encodeValue writes a single primitive value or inline array
func (e *encoder) encodeValue(v reflect.Value) error {
	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			e.w.WriteString("true")
		} else {
			e.w.WriteString("false")
		}

	case reflect.String:
		e.encodeString(v.String())

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.w.WriteString(strconv.FormatInt(v.Int(), 10))

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		e.w.WriteString(strconv.FormatUint(v.Uint(), 10))

	case reflect.Float32, reflect.Float64:
		f := v.Float()
		// NaN/Inf have no TOML representation; emitting them produces
		// output the parser rejects
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("cannot encode %v as TOML float", f)
		}
		// bitSize matches kind (float32 emitted noise digits);
		// 'g' avoids ~100-digit 'f' expansions for large exponents
		bits := 64
		if v.Kind() == reflect.Float32 {
			bits = 32
		}
		str := strconv.FormatFloat(f, 'g', -1, bits)
		if !strings.ContainsAny(str, ".eE") {
			str += ".0"
		}
		e.w.WriteString(str)

	case reflect.Slice, reflect.Array:
		// Inline array: [1, 2, "3"]
		e.w.WriteString("[")
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				e.w.WriteString(", ")
			}
			if err := e.encodeValue(v.Index(i)); err != nil {
				return err
			}
		}
		e.w.WriteString("]")

	case reflect.Interface:
		if v.IsNil() {
			return nil // Should be handled by caller usually
		}
		return e.encodeValue(v.Elem())

	default:
		return fmt.Errorf("unsupported type: %v", v.Kind())
	}
	return nil
}

// --- Helpers ---

// getSortedKeys returns all field names (struct) or keys (map) sorted
func (e *encoder) getSortedKeys(rv reflect.Value) ([]string, error) {
	var keys []string

	if rv.Kind() == reflect.Map {
		for _, key := range rv.MapKeys() {
			if key.Kind() != reflect.String {
				return nil, fmt.Errorf("map key must be string, got %v", key.Kind())
			}
			keys = append(keys, key.String())
		}
	} else if rv.Kind() == reflect.Struct {
		typ := rv.Type()
		for i := 0; i < rv.NumField(); i++ {
			field := typ.Field(i)
			// Skip unexported
			if field.PkgPath != "" {
				continue
			}
			// Skip if tag is "-"
			tag := field.Tag.Get("toml")
			if tag == "-" {
				continue
			}
			keys = append(keys, field.Name)
		}
	}
	// Sort by emitted key (tag-resolved), not Go field name.
	// Identity for maps (getKeyName returns the key itself).
	sort.Slice(keys, func(i, j int) bool {
		return e.getKeyName(rv, keys[i]) < e.getKeyName(rv, keys[j])
	})
	return keys, nil
}

// resolveValue extracts the value from struct field or map key
// It also handles interface unwrapping for map[string]any.
func (e *encoder) resolveValue(container reflect.Value, key string) reflect.Value {
	var val reflect.Value
	if container.Kind() == reflect.Map {
		val = container.MapIndex(reflect.ValueOf(key))
	} else {
		val = container.FieldByName(key)
	}

	// Unwrap interface if needed
	if val.Kind() == reflect.Interface && !val.IsNil() {
		val = val.Elem()
	}

	// Dereference pointer if needed (but keep nil ptrs for checking)
	if val.Kind() == reflect.Ptr && !val.IsNil() {
		val = val.Elem()
	}

	return val
}

// getKeyName resolves the TOML key name (handles struct tags)
// For maps, the key name is the key itself
func (e *encoder) getKeyName(container reflect.Value, realName string) string {
	if container.Kind() == reflect.Map {
		return realName
	}
	// Struct: lookup tag
	field, _ := container.Type().FieldByName(realName)
	tag := field.Tag.Get("toml")
	if tag != "" {
		parts := strings.Split(tag, ",")
		if parts[0] != "" {
			return parts[0]
		}
	}
	return realName
}

// shouldSkip returns true if the field should be omitted (nil ptr, omitempty)
func (e *encoder) shouldSkip(container reflect.Value, realName string, val reflect.Value) bool {
	// Skip nil pointers / interfaces
	if (val.Kind() == reflect.Ptr || val.Kind() == reflect.Interface) && val.IsNil() {
		return true
	}

	// Maps don't have tags, so we only skip nil values
	if container.Kind() == reflect.Map {
		return false
	}

	// Structs: check omitempty
	field, _ := container.Type().FieldByName(realName)
	tag := field.Tag.Get("toml")
	if strings.Contains(tag, "omitempty") && isEmptyValue(val) {
		return true
	}

	return false
}

// isTable determines if a value renders as [Table] / [[Array of Tables]].
// Slices must be homogeneous: mixing table and scalar elements is an error
// (parser accepts such arrays; encoder cannot represent them — documented
// limitation). Nil pointer elements are allowed in table slices and skipped
// at emission; nil interface elements are an error.
func (e *encoder) isTable(v reflect.Value) (bool, error) {
	if v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Struct, reflect.Map:
		return true, nil
	case reflect.Slice, reflect.Array:
		tables, scalars := 0, 0
		for i := 0; i < v.Len(); i++ {
			elem := v.Index(i)
			if elem.Kind() == reflect.Interface {
				if elem.IsNil() {
					return false, fmt.Errorf("nil element at index %d", i)
				}
				elem = elem.Elem()
			}
			if elem.Kind() == reflect.Ptr {
				if elem.IsNil() {
					tables++ // nil pointers are only legal in [[table]] context (skipped)
					continue
				}
				elem = elem.Elem()
			}
			switch elem.Kind() {
			case reflect.Struct, reflect.Map:
				tables++
			default:
				scalars++
			}
		}
		if tables > 0 && scalars > 0 {
			return false, fmt.Errorf("mixed table/scalar elements in array")
		}
		return tables > 0, nil
	}
	return false, nil
}

func isEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Ptr:
		return v.IsNil()
	}
	return false
}

func (e *encoder) writeKey(s string) error {
	if isBareKey(s) {
		_, err := e.w.WriteString(s)
		return err
	}
	e.encodeString(s)
	return nil
}

func (e *encoder) encodeString(s string) {
	e.w.WriteString("\"")
	for _, r := range s {
		switch r {
		case '"':
			e.w.WriteString(`\"`)
		case '\\':
			e.w.WriteString(`\\`)
		case '\n':
			e.w.WriteString(`\n`)
		case '\r':
			e.w.WriteString(`\r`)
		case '\t':
			e.w.WriteString(`\t`)
		case '\b':
			e.w.WriteString(`\b`)
		case '\f':
			e.w.WriteString(`\f`)
		default:
			if r < 0x20 || r == 0x7F {
				e.w.WriteString(fmt.Sprintf(`\u%04X`, r))
			} else {
				e.w.WriteRune(r)
			}
		}
	}
	e.w.WriteString("\"")
}

// isBareKey determines if a string can be written as a bare key
// It follows the TOML spec (A-Za-z0-9_-) but also respects the provided Lexer's behavior
// If the Lexer would interpret the string as a Number or Boolean, it must be quoted because the Parser expects TokenIdent (or TokenString) for keys
func isBareKey(s string) bool {
	if s == "" {
		return false
	}

	// 1. Valid bare key characters: A-Za-z0-9_-
	for _, r := range s {
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}

	// 2. Avoid collision with Booleans
	// Lexer emits TokenBool for these, Parser expects TokenIdent.
	if s == "true" || s == "false" {
		return false
	}

	// 3. Avoid collision with Numbers
	// The provided Lexer triggers number parsing if the token starts with a digit,
	// or a '-' followed by a digit.
	// Since the Lexer never produces TokenIdent for these cases (it produces TokenInteger/Float),
	// and the Parser rejects numeric tokens as keys, we must quote them.
	c0 := s[0]
	if c0 >= '0' && c0 <= '9' {
		return false
	}
	if c0 == '-' && len(s) > 1 {
		c1 := s[1]
		if c1 >= '0' && c1 <= '9' {
			return false
		}
	}

	return true
}


package toml

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Marshal encodes a struct or string-keyed map with deterministic key order.
// Nil pointer/interface fields are omitted; nil pointer/interface array elements,
// cycles/excessive nesting,
// non-finite floats, and integers outside TOML's signed 64-bit range error.
// Comments and whitespace are not retained. Datetimes are not supported.
func Marshal(v any) ([]byte, error) {
	var e encoder
	if err := e.table(reflect.ValueOf(v), "", 0); err != nil {
		return nil, err
	}
	return e.buf.Bytes(), nil
}

type encoder struct{ buf bytes.Buffer }
type entry struct {
	key   string
	value reflect.Value
}

func indirect(v reflect.Value) (reflect.Value, error) {
	for depth := 0; v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer); depth++ {
		if depth > maxValueDepth {
			return reflect.Value{}, fmt.Errorf("pointer nesting exceeds %d", maxValueDepth)
		}
		if v.IsNil() {
			return reflect.Value{}, nil
		}
		v = v.Elem()
	}
	return v, nil
}

func entries(v reflect.Value) ([]entry, error) {
	v, err := indirect(v)
	if err != nil {
		return nil, err
	}
	if !v.IsValid() {
		return nil, fmt.Errorf("nil table")
	}
	var out []entry
	switch v.Kind() {
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map keys must be strings")
		}
		out = make([]entry, 0, v.Len())
		it := v.MapRange()
		for it.Next() {
			out = append(out, entry{it.Key().String(), it.Value()})
		}
	case reflect.Struct:
		t := v.Type()
		out = make([]entry, 0, t.NumField())
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			parts := strings.Split(f.Tag.Get("toml"), ",")
			if parts[0] == "-" {
				continue
			}
			key := f.Name
			if parts[0] != "" {
				key = parts[0]
			}
			if slices.Contains(parts[1:], "omitempty") && isEmptyValue(v.Field(i)) {
				continue
			}
			out = append(out, entry{key, v.Field(i)})
		}
	default:
		return nil, fmt.Errorf("expected struct or map, got %s", v.Kind())
	}
	slices.SortFunc(out, func(a, b entry) int { return cmp.Compare(a.key, b.key) })
	result := out[:0]
	previous := ""
	for i, x := range out {
		if _, err := keyText(x.key); err != nil {
			return nil, err
		}
		if i > 0 && previous == x.key {
			return nil, fmt.Errorf("duplicate TOML key %q", x.key)
		}
		previous = x.key
		value, err := indirect(x.value)
		if err != nil {
			return nil, err
		}
		if value.IsValid() {
			result = append(result, entry{x.key, value})
		}
	}
	return result, nil
}

func (e *encoder) table(v reflect.Value, prefix string, depth int) error {
	if depth > maxValueDepth {
		return fmt.Errorf("encode nesting exceeds %d (possible cycle)", maxValueDepth)
	}
	fields, err := entries(v)
	if err != nil {
		return err
	}
	var tables []entry
	for _, x := range fields {
		if isTable(x.value) || isTableArray(x.value) {
			tables = append(tables, x)
			continue
		}
		key, _ := keyText(x.key)
		e.buf.WriteString(key + " = ")
		if err := e.value(x.value, depth+1); err != nil {
			return fmt.Errorf("key %q: %w", x.key, err)
		}
		e.buf.WriteByte('\n')
	}
	for _, x := range tables {
		key, _ := keyText(x.key)
		if prefix != "" {
			key = prefix + "." + key
		}
		if isTable(x.value) {
			e.buf.WriteString("\n[" + key + "]\n")
			if err := e.table(x.value, key, depth+1); err != nil {
				return err
			}
		} else {
			for i := 0; i < x.value.Len(); i++ {
				e.buf.WriteString("\n[[" + key + "]]\n")
				if err := e.table(x.value.Index(i), key, depth+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func isTable(v reflect.Value) bool { return v.Kind() == reflect.Map || v.Kind() == reflect.Struct }
func isTableArray(v reflect.Value) bool {
	if v.Kind() != reflect.Slice && v.Kind() != reflect.Array || v.Len() == 0 {
		return false
	}
	for i := 0; i < v.Len(); i++ {
		x, err := indirect(v.Index(i))
		if err != nil || !x.IsValid() || !isTable(x) {
			return false
		}
	}
	return true
}

func (e *encoder) value(v reflect.Value, depth int) error {
	if depth > maxValueDepth {
		return fmt.Errorf("encode nesting exceeds %d (possible cycle)", maxValueDepth)
	}
	v, err := indirect(v)
	if err != nil {
		return err
	}
	if !v.IsValid() {
		return fmt.Errorf("nil array element has no TOML representation")
	}
	switch v.Kind() {
	case reflect.Bool:
		e.buf.WriteString(strconv.FormatBool(v.Bool()))
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("invalid UTF-8 string")
		}
		e.string(v.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		e.buf.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if v.Uint() > math.MaxInt64 {
			return fmt.Errorf("unsigned integer %d exceeds TOML int64 range", v.Uint())
		}
		e.buf.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("non-finite float")
		}
		s := strconv.FormatFloat(f, 'g', -1, v.Type().Bits())
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		e.buf.WriteString(s)
	case reflect.Slice, reflect.Array:
		e.buf.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				e.buf.WriteString(", ")
			}
			if err := e.value(v.Index(i), depth+1); err != nil {
				return err
			}
		}
		e.buf.WriteByte(']')
	case reflect.Struct, reflect.Map:
		fields, err := entries(v)
		if err != nil {
			return err
		}
		e.buf.WriteByte('{')
		for i, x := range fields {
			if i > 0 {
				e.buf.WriteString(", ")
			}
			key, _ := keyText(x.key)
			e.buf.WriteString(key + " = ")
			if err := e.value(x.value, depth+1); err != nil {
				return err
			}
		}
		e.buf.WriteByte('}')
	default:
		return fmt.Errorf("unsupported type %s", v.Type())
	}
	return nil
}

func keyText(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("invalid UTF-8 key")
	}
	if numericKey(s) {
		return "", fmt.Errorf("numeric keys are forbidden: %q", s)
	}
	if isBareKey(s) {
		return s, nil
	}
	var e encoder
	e.string(s)
	return e.buf.String(), nil
}

func (e *encoder) string(s string) {
	e.buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			e.buf.WriteString(`\"`)
		case '\\':
			e.buf.WriteString(`\\`)
		case '\n':
			e.buf.WriteString(`\n`)
		case '\r':
			e.buf.WriteString(`\r`)
		case '\t':
			e.buf.WriteString(`\t`)
		case '\b':
			e.buf.WriteString(`\b`)
		case '\f':
			e.buf.WriteString(`\f`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&e.buf, `\u%04X`, r)
			} else {
				e.buf.WriteRune(r)
			}
		}
	}
	e.buf.WriteByte('"')
}

func isBareKey(s string) bool {
	if s == "" || s == "true" || s == "false" {
		return false
	}
	if s[0] >= '0' && s[0] <= '9' || len(s) > 1 && s[0] == '-' && s[1] >= '0' && s[1] <= '9' {
		return false
	}
	for _, r := range s {
		if !isAlpha(r) && !isDigit(r) && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func isEmptyValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	default:
		return v.IsZero()
	}
}

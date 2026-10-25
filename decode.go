package toml

import (
	"cmp"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
)

// Unmarshal parses TOML data and stores the result in the value pointed to by v.
func Unmarshal(data []byte, v any) error {
	p := NewParser(data)
	parsedMap, err := p.Parse()
	if err != nil {
		return err
	}
	return Decode(parsedMap, v)
}

// Decode maps parser values to typed destinations using reflection.
// It uses toml tags, falling back to field names, and leaves v unchanged on error.
// Interface destinations retain the input value, including container references.
func Decode(data any, v any) error {
	val := reflect.ValueOf(v)
	if val.Kind() != reflect.Ptr || val.IsNil() {
		return fmt.Errorf("target must be a non-nil pointer")
	}

	// Stage changes; pointer, map and slice branches allocate before writing.
	next := reflect.New(val.Elem().Type()).Elem()
	next.Set(val.Elem())
	if err := decodeValue(data, next); err != nil {
		return err
	}
	val.Elem().Set(next)
	return nil
}

func decodeValue(data any, val reflect.Value) error {
	return decodeAt(data, val, 0)
}

// pathError names where decoding failed. Each level adds its segment instead
// of wrapping the message, which would copy the message once per level.
type pathError struct {
	path []string // innermost first
	err  error
}

func (e *pathError) Error() string {
	var b strings.Builder
	for i := len(e.path) - 1; i >= 0; i-- {
		b.WriteString(e.path[i])
		b.WriteString(": ")
	}
	b.WriteString(e.err.Error())
	return b.String()
}

func (e *pathError) Unwrap() error { return e.err }

func atPath(segment string, err error) error {
	if pe, ok := err.(*pathError); ok {
		pe.path = append(pe.path, segment)
		return pe
	}
	return &pathError{path: []string{segment}, err: err}
}

func decodeAt(data any, val reflect.Value, depth int) error {
	if depth > maxValueDepth {
		return fmt.Errorf("decode nesting exceeds %d", maxValueDepth)
	}
	if data == nil {
		return nil
	}

	switch val.Kind() {
	case reflect.Ptr:
		elemType := val.Type().Elem()
		newVal := reflect.New(elemType)
		if !val.IsNil() {
			newVal.Elem().Set(val.Elem())
		}
		if err := decodeAt(data, newVal.Elem(), depth+1); err != nil {
			return err
		}
		val.Set(newVal)

	case reflect.Struct:
		dataMap, ok := data.(map[string]any)
		if !ok {
			return fmt.Errorf("expected map for struct, got %T", data)
		}
		return decodeStruct(dataMap, val, depth+1)

	case reflect.Slice, reflect.Array:
		dataSlice, ok := data.([]any)
		if !ok {
			if mapSlice, ok := data.([]map[string]any); ok {
				dataSlice = make([]any, len(mapSlice))
				for i, m := range mapSlice {
					dataSlice[i] = m
				}
			} else {
				return fmt.Errorf("expected slice, got %T", data)
			}
		}

		var newSlice reflect.Value
		if val.Kind() == reflect.Array {
			if len(dataSlice) != val.Len() {
				return fmt.Errorf("expected array length %d, got %d", val.Len(), len(dataSlice))
			}
			newSlice = reflect.New(val.Type()).Elem()
		} else {
			newSlice = reflect.MakeSlice(val.Type(), len(dataSlice), len(dataSlice))
		}
		for i := 0; i < len(dataSlice); i++ {
			if err := decodeAt(dataSlice[i], newSlice.Index(i), depth+1); err != nil {
				return atPath(fmt.Sprintf("index %d", i), err)
			}
		}
		val.Set(newSlice)

	case reflect.Map:
		if val.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("only map[string]T is supported")
		}
		dataMap, ok := data.(map[string]any)
		if !ok {
			return fmt.Errorf("expected map, got %T", data)
		}
		newMap := reflect.MakeMapWithSize(val.Type(), len(dataMap))
		// One key and one element, zeroed per entry, which SetMapIndex copies
		key := reflect.New(val.Type().Key()).Elem()
		elem := reflect.New(val.Type().Elem()).Elem()
		for k, vData := range dataMap {
			elem.SetZero()
			if err := decodeAt(vData, elem, depth+1); err != nil {
				return atPath("map key "+brief(k), err)
			}
			key.SetString(k)
			newMap.SetMapIndex(key, elem)
		}
		val.Set(newMap)

	case reflect.Interface:
		// Unchecked Set panics for non-empty interface targets (io.Reader etc.)
		dv := reflect.ValueOf(data)
		if !dv.Type().AssignableTo(val.Type()) {
			return fmt.Errorf("cannot assign %T to interface %s", data, val.Type())
		}
		val.Set(dv)

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, ok := toInt64(data)
		if !ok {
			return fmt.Errorf("cannot convert %T to int", data)
		}
		// SetInt truncates silently on narrower kinds
		if val.OverflowInt(i) {
			return fmt.Errorf("value %d overflows %s", i, val.Type())
		}
		val.SetInt(i)

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u, ok := toUint64(data)
		if !ok {
			// Formatting a rejected container's value could recurse through a cycle.
			return fmt.Errorf("cannot convert %T to uint without loss", data)
		}
		// Overflow check
		if val.OverflowUint(u) {
			return fmt.Errorf("value %d overflows %s", u, val.Type())
		}
		val.SetUint(u)

	case reflect.Float32, reflect.Float64:
		f, ok := toFloat(data, val.Type().Bits())
		if !ok {
			return fmt.Errorf("cannot convert %T to float", data)
		}
		// float64 -> float32 range check
		if val.OverflowFloat(f) {
			return fmt.Errorf("value %g overflows %s", f, val.Type())
		}
		val.SetFloat(f)

	case reflect.String:
		s, ok := data.(string)
		if !ok {
			return fmt.Errorf("cannot convert %T to string", data)
		}
		val.SetString(s)

	case reflect.Bool:
		b, ok := data.(bool)
		if !ok {
			return fmt.Errorf("cannot convert %T to bool", data)
		}
		val.SetBool(b)

	default:
		// Reject unsupported kinds instead of silent zero value
		return fmt.Errorf("unsupported target kind %s", val.Kind())
	}

	return nil
}

func decodeStruct(data map[string]any, val reflect.Value, depth int) error {
	for _, f := range fieldsOf(val.Type()) {
		// Look up in data map (case sensitive)
		if vData, ok := data[f.key]; ok {
			if err := decodeAt(vData, val.Field(f.index), depth+1); err != nil {
				return atPath(val.Type().Name()+"."+f.name, err)
			}
		}
	}
	return nil
}

// field is a struct field a document can set: exported and not tagged "-"
type field struct {
	index     int
	key       string // the tag's name, else the Go name
	name      string // the Go name
	omitempty bool
}

var fieldCache sync.Map // reflect.Type -> []field

// fieldsOf lists the fields of struct type t a document names, in declaration
// order; read once per type, as the tags never change
func fieldsOf(t reflect.Type) []field {
	if f, ok := fieldCache.Load(t); ok {
		return f.([]field)
	}
	out := make([]field, 0, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		name, opts, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if !f.IsExported() || name == "-" {
			continue
		}
		x := field{index: i, key: cmp.Or(name, f.Name), name: f.Name}
		for o := range strings.SplitSeq(opts, ",") {
			x.omitempty = x.omitempty || o == "omitempty"
		}
		out = append(out, x)
	}
	f, _ := fieldCache.LoadOrStore(t, out)
	return f.([]field)
}

// Numeric conversions check before conversion: rounded float64(MaxInt64) is
// 2^63, so the upper bound must be exclusive. No float intermediary for integers.
func toInt64(v any) (int64, bool) {
	r := reflect.ValueOf(v)
	if !r.IsValid() {
		return 0, false
	}
	switch r.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return r.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u := r.Uint()
		return int64(u), u <= math.MaxInt64
	case reflect.Float32, reflect.Float64:
		f := r.Float()
		if math.IsNaN(f) || f < -0x1p63 || f >= 0x1p63 || math.Trunc(f) != f {
			return 0, false
		}
		return int64(f), true
	}
	return 0, false
}

func toUint64(v any) (uint64, bool) {
	r := reflect.ValueOf(v)
	if !r.IsValid() {
		return 0, false
	}
	switch r.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return r.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i := r.Int()
		return uint64(i), i >= 0
	case reflect.Float32, reflect.Float64:
		f := r.Float()
		if math.IsNaN(f) || f < 0 || f >= 0x1p64 || math.Trunc(f) != f {
			return 0, false
		}
		return uint64(f), true
	}
	return 0, false
}

func toFloat(v any, width int) (float64, bool) {
	r := reflect.ValueOf(v)
	if !r.IsValid() {
		return 0, false
	}
	var f float64
	switch r.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i := r.Int()
		f = float64(i)
		if width == 32 {
			f = float64(float32(f))
		}
		if f >= 0x1p63 || f < -0x1p63 || int64(f) != i {
			return 0, false
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u := r.Uint()
		f = float64(u)
		if width == 32 {
			f = float64(float32(f))
		}
		if f >= 0x1p64 || uint64(f) != u {
			return 0, false
		}
	case reflect.Float32, reflect.Float64:
		f = r.Float()
	default:
		return 0, false
	}
	return f, !math.IsNaN(f) && !math.IsInf(f, 0)
}

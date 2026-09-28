package toml

import (
	"fmt"
	"math"
	"reflect"
	"strings"
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
				return fmt.Errorf("index %d: %w", i, err)
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
		elemType := val.Type().Elem()
		for k, vData := range dataMap {
			newVal := reflect.New(elemType).Elem()
			if err := decodeAt(vData, newVal, depth+1); err != nil {
				return fmt.Errorf("map key %s: %w", k, err)
			}
			newMap.SetMapIndex(reflect.ValueOf(k).Convert(val.Type().Key()), newVal)
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
	typ := val.Type()

	for i := 0; i < val.NumField(); i++ {
		field := val.Field(i)
		fieldType := typ.Field(i)

		// Safety check: skip unexported fields that cannot be set
		if !field.CanSet() {
			continue
		}

		// Determine key name
		key := fieldType.Name
		if tag := fieldType.Tag.Get("toml"); tag != "" {
			parts := strings.Split(tag, ",")
			if parts[0] == "-" {
				continue
			}
			if parts[0] != "" {
				key = parts[0]
			}
		}

		// Look up in data map (case sensitive)
		if vData, ok := data[key]; ok {
			if err := decodeAt(vData, field, depth+1); err != nil {
				return fmt.Errorf("%s.%s: %w", typ.Name(), fieldType.Name, err)
			}
		}
	}
	return nil
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

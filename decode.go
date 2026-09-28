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

// Decode maps a generic map[string]any to a struct/slice/etc using reflection.
// It prioritizes `toml` tags and falls back to field names.
func Decode(data any, v any) error {
	val := reflect.ValueOf(v)
	if val.Kind() != reflect.Ptr || val.IsNil() {
		return fmt.Errorf("target must be a non-nil pointer")
	}

	// Decode into a temporary value so errors never partially change the target.
	tmp := reflect.New(val.Elem().Type()).Elem()
	tmp.Set(val.Elem())
	if err := decodeValue(data, tmp, 0); err != nil {
		return err
	}
	val.Elem().Set(tmp)
	return nil
}

func decodeValue(data any, val reflect.Value, depth int) error {
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
		if err := decodeValue(data, newVal.Elem(), depth+1); err != nil {
			return err
		}
		val.Set(newVal)

	case reflect.Struct:
		dataMap, ok := data.(map[string]any)
		if !ok {
			return fmt.Errorf("expected map for struct, got %T", data)
		}
		return decodeStruct(dataMap, val, depth)

	case reflect.Slice:
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

		newSlice := reflect.MakeSlice(val.Type(), len(dataSlice), len(dataSlice))
		for i := 0; i < len(dataSlice); i++ {
			if err := decodeValue(dataSlice[i], newSlice.Index(i), depth+1); err != nil {
				return err
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
		newMap := reflect.MakeMap(val.Type())
		elemType := val.Type().Elem()
		for k, vData := range dataMap {
			newVal := reflect.New(elemType).Elem()
			if err := decodeValue(vData, newVal, depth+1); err != nil {
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
			return fmt.Errorf("cannot convert %T to uint without loss", data)
		}
		if val.OverflowUint(u) {
			return fmt.Errorf("value %d overflows %s", u, val.Type())
		}
		val.SetUint(u)

	case reflect.Float32, reflect.Float64:
		f, ok := toFloat(data)
		if !ok {
			return fmt.Errorf("cannot convert %T to float", data)
		}
		// float64 -> float32 range check
		if math.IsNaN(f) || math.IsInf(f, 0) || val.OverflowFloat(f) {
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
			if err := decodeValue(vData, field, depth+1); err != nil {
				return fmt.Errorf("%s.%s: %w", typ.Name(), fieldType.Name, err)
			}
		}
	}
	return nil
}

// Numeric conversion uses distinct signed and unsigned paths. Check floats before
// conversion: converting an out-of-range float to an integer is not portable.
func toInt64(data any) (int64, bool) {
	v := reflect.ValueOf(data)
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u := v.Uint()
		return int64(u), u <= math.MaxInt64
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.Trunc(f) != f || f < -0x1p63 || f >= 0x1p63 {
			return 0, false
		}
		return int64(f), true
	}
	return 0, false
}

func toUint64(data any) (uint64, bool) {
	v := reflect.ValueOf(data)
	switch v.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i := v.Int()
		return uint64(i), i >= 0
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.Trunc(f) != f || f < 0 || f >= 0x1p64 {
			return 0, false
		}
		return uint64(f), true
	}
	return 0, false
}

func toFloat(data any) (float64, bool) {
	v := reflect.ValueOf(data)
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(v.Uint()), true
	case reflect.Float32, reflect.Float64:
		return v.Float(), true
	}
	return 0, false
}

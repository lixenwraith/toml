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

	return decodeValue(data, val.Elem())
}

func decodeValue(data any, val reflect.Value) error {
	if data == nil {
		return nil
	}

	switch val.Kind() {
	case reflect.Ptr:
		elemType := val.Type().Elem()
		newVal := reflect.New(elemType)
		if err := decodeValue(data, newVal.Elem()); err != nil {
			return err
		}
		val.Set(newVal)

	case reflect.Struct:
		dataMap, ok := data.(map[string]any)
		if !ok {
			return fmt.Errorf("expected map for struct, got %T", data)
		}
		return decodeStruct(dataMap, val)

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
			if err := decodeValue(dataSlice[i], newSlice.Index(i)); err != nil {
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
			if err := decodeValue(vData, newVal); err != nil {
				return fmt.Errorf("map key %s: %w", k, err)
			}
			newMap.SetMapIndex(reflect.ValueOf(k), newVal)
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
		i, ok := toInt64(data)
		if !ok {
			return fmt.Errorf("cannot convert %T to uint", data)
		}
		if i < 0 {
			return fmt.Errorf("cannot convert negative value %d to uint", i)
		}
		// Overflow check
		if val.OverflowUint(uint64(i)) {
			return fmt.Errorf("value %d overflows %s", i, val.Type())
		}
		val.SetUint(uint64(i))

	case reflect.Float32, reflect.Float64:
		f, ok := toFloat(data)
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

func decodeStruct(data map[string]any, val reflect.Value) error {
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
			key = parts[0]
		}

		// Look up in data map (case sensitive)
		if vData, ok := data[key]; ok {
			if err := decodeValue(vData, field); err != nil {
				return fmt.Errorf("%s.%s: %w", typ.Name(), fieldType.Name, err)
			}
		}
	}
	return nil
}

// toInt64 converts numeric types to int64
func toInt64(v any) (int64, bool) {
	switch i := v.(type) {
	case int:
		return int64(i), true
	case int8:
		return int64(i), true
	case int16:
		return int64(i), true
	case int32:
		return int64(i), true
	case int64:
		return i, true
	case uint:
		// Reject wrap
		if uint64(i) > math.MaxInt64 {
			return 0, false
		}
	case uint8:
		return int64(i), true
	case uint16:
		return int64(i), true
	case uint32:
		return int64(i), true
	case uint64:
		// Reject values that wrap negative through int64
		if i > math.MaxInt64 {
			return 0, false
		}
		return int64(i), true
	case float64:
		return int64(i), true
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch i := v.(type) {
	case int:
		return float64(i), true
	case int8:
		return float64(i), true
	case int16:
		return float64(i), true
	case int32:
		return float64(i), true
	case int64:
		return float64(i), true
	case uint:
		return float64(i), true
	case uint8:
		return float64(i), true
	case uint16:
		return float64(i), true
	case uint32:
		return float64(i), true
	case uint64:
		return float64(i), true
	case float64:
		return i, true
	}
	return 0, false
}


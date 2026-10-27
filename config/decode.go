package config

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// supported field kinds: basic types (bool/string/int*/uint*/float*),
// pointers and slices of them, and nested structs.
type decoder struct {
	// sources maps dotted raw key path to the winning layer.
	sources map[string]string
	// tag struct tag name providing the setting key.
	tag string
	// rejectUnknown turns unknown keys into fatal issues.
	rejectUnknown bool

	issues []Issue
}

// decodeData converts the merged raw data into the fresh struct value.
//
// Every incompatibility is collected as an IssueTypeError keyed by the
// setting name, instead of silently leaving the field at its default/zero
// value. Unknown data keys are collected as well.
func (d *decoder) decodeData(rv reflect.Value, data map[string]any) {
	d.structInto(rv, "", "", data, make(map[string]bool))
}

func (d *decoder) structInto(rv reflect.Value, cfgPath, rawPath string, data map[string]any, used map[string]bool) {
	rt := rv.Type()

	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if sf.Anonymous {
			fv := rv.Field(i)
			if fv.Kind() == reflect.Ptr {
				if fv.IsNil() {
					fv.Set(reflect.New(fv.Type().Elem()))
				}
				fv = fv.Elem()
			}
			if fv.Kind() == reflect.Struct {
				// anonymous struct: flatten its fields into this level,
				// sharing the used-key set of the current table.
				d.structInto(fv, cfgPath, rawPath, data, used)
				continue
			}
		}
		if !sf.IsExported() {
			continue
		}

		name, skip := fieldKey(sf, d.tag)
		if skip {
			continue
		}

		raw, actualKey, ok := lookupKey(data, name)
		if !ok {
			continue
		}
		used[actualKey] = true

		fieldCfgPath := joinIf(cfgPath, name)
		fieldRawPath := joinIf(rawPath, actualKey)
		d.assign(rv.Field(i), raw, fieldCfgPath, fieldRawPath)
	}

	// keys that no field accepts.
	for key := range data {
		if used[key] {
			continue
		}
		path := joinIf(rawPath, key)
		is := Issue{
			Kind:    IssueUnknownKey,
			Setting: path,
			Winner:  d.sources[path],
			Raw:     data[key],
			Msg:     "no struct field accepts this key",
		}
		if d.rejectUnknown {
			is.Msg = "no struct field accepts this key (reject-unknown enabled)"
		}
		d.issues = append(d.issues, is)
	}
}

// fieldKey resolves the settings key of a field.
func fieldKey(sf reflect.StructField, tag string) (name string, skip bool) {
	tagVal := sf.Tag.Get(tag)
	if tagVal == "-" {
		return "", true
	}
	if tagVal != "" {
		name = tagVal
		if i := strings.IndexByte(name, ','); i >= 0 {
			name = strings.TrimSpace(name[:i])
		}
	}
	if name == "" {
		name = sf.Name
	}
	return name, false
}

// lookupKey finds key exactly first, then case-insensitively.
// It returns the actual key present in data.
func lookupKey(data map[string]any, key string) (raw any, actualKey string, ok bool) {
	if raw, ok = data[key]; ok {
		return raw, key, true
	}
	for k := range data {
		if strings.EqualFold(k, key) {
			return data[k], k, true
		}
	}
	return nil, "", false
}

// assign converts one winning raw value into fv.
func (d *decoder) assign(fv reflect.Value, raw any, cfgPath, rawPath string) {
	winner := d.sources[rawPath]

	// pointers: allocate and unwrap; null clears the pointer.
	if fv.Kind() == reflect.Ptr {
		if raw == nil {
			fv.Set(reflect.Zero(fv.Type()))
			return
		}
		if fv.IsNil() {
			fv.Set(reflect.New(fv.Type().Elem()))
		}
		d.assign(fv.Elem(), raw, cfgPath, rawPath)
		return
	}

	switch fv.Kind() {
	case reflect.Struct:
		t, ok := asTable(raw)
		if !ok {
			d.typeError(cfgPath, rawPath, winner, fv.Type(), raw, "expect a nested table")
			return
		}
		d.structInto(fv, cfgPath, rawPath, t, make(map[string]bool))
	case reflect.Slice:
		if raw == nil {
			fv.Set(reflect.Zero(fv.Type()))
			return
		}
		d.assignSlice(fv, raw, cfgPath, rawPath, winner)
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		val, err := coerceBasic(raw, fv.Kind())
		if err != nil {
			d.typeError(cfgPath, rawPath, winner, fv.Type(), raw, err.Error())
			return
		}
		fv.Set(val)
	default:
		d.typeError(cfgPath, rawPath, winner, fv.Type(), raw, "unsupported field type, only basic types/basic slices/nested structs are supported")
	}
}

// assignSlice handles []T where T is a basic type (or pointer to one).
func (d *decoder) assignSlice(fv reflect.Value, raw any, cfgPath, rawPath, winner string) {
	elemKind := fv.Type().Elem().Kind()
	if elemKind == reflect.Ptr {
		elemKind = fv.Type().Elem().Elem().Kind()
	}
	if !isBasicKind(elemKind) {
		d.typeError(cfgPath, rawPath, winner, fv.Type(), raw, "slice element type must be a basic type")
		return
	}

	var items []any
	switch v := raw.(type) {
	case []any:
		items = v
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			// empty value clears the list instead of yielding one "".
			fv.Set(reflect.MakeSlice(fv.Type(), 0, 0))
			return
		}
		// KV layers provide comma-separated lists.
		parts := strings.Split(v, ",")
		items = make([]any, 0, len(parts))
		for _, p := range parts {
			items = append(items, strings.TrimSpace(p))
		}
	default:
		d.typeError(cfgPath, rawPath, winner, fv.Type(), raw, "expect an array or a comma-separated string")
		return
	}

	slice := reflect.MakeSlice(fv.Type(), len(items), len(items))
	for i, item := range items {
		elem := slice.Index(i)
		target := elem
		if elem.Kind() == reflect.Ptr {
			target = reflect.New(elem.Type().Elem())
		}
		val, err := coerceBasic(item, target.Kind())
		if err != nil {
			d.typeError(fmt.Sprintf("%s[%d]", cfgPath, i), fmt.Sprintf("%s[%d]", rawPath, i),
				winner, fv.Type().Elem(), item, err.Error())
			return
		}
		target.Set(val)
		if elem.Kind() == reflect.Ptr {
			elem.Set(target)
		}
	}
	fv.Set(slice)
}

func (d *decoder) typeError(cfgPath, rawPath, winner string, want reflect.Type, raw any, msg string) {
	d.issues = append(d.issues, Issue{
		Kind:    IssueTypeError,
		Setting: cfgPath,
		Winner:  winner,
		Want:    want.String(),
		Raw:     raw,
		Msg:     msg,
	})
}

func isBasicKind(k reflect.Kind) bool {
	switch k {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// coerceBasic converts one raw value to the declared basic kind.
//
// Accepted shapes (anything else is an error, never a silent fallback):
//
//   - numbers: JSON numbers (json.Number, keeping precision), Go numeric
//     values in in-memory layers, or decimal/0x strings;
//   - bool:   true/false or the strings "true","false","1","0";
//   - string: a string or the exact textual form of a JSON number.
//
// Integer targets reject non-integral and out-of-range values.
func coerceBasic(raw any, kind reflect.Kind) (reflect.Value, error) {
	switch kind {
	case reflect.String:
		switch v := raw.(type) {
		case string:
			return reflect.ValueOf(v), nil
		case json.Number:
			return reflect.ValueOf(v.String()), nil
		default:
			return reflect.Value{}, fmt.Errorf("cannot use %s as string", describe(raw))
		}

	case reflect.Bool:
		switch v := raw.(type) {
		case bool:
			return reflect.ValueOf(v), nil
		case string:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return reflect.Value{}, fmt.Errorf("cannot parse %q as bool", v)
			}
			return reflect.ValueOf(b), nil
		case json.Number:
			b, err := strconv.ParseBool(v.String())
			if err != nil {
				return reflect.Value{}, fmt.Errorf("cannot parse %q as bool", v.String())
			}
			return reflect.ValueOf(b), nil
		default:
			return reflect.Value{}, fmt.Errorf("cannot use %s as bool", describe(raw))
		}

	case reflect.Float32, reflect.Float64:
		bits := 64
		if kind == reflect.Float32 {
			bits = 32
		}
		f, err := toFloat(raw, bits)
		if err != nil {
			return reflect.Value{}, err
		}
		if kind == reflect.Float32 {
			return reflect.ValueOf(float32(f)), nil
		}
		return reflect.ValueOf(f), nil

	default:
		// signed / unsigned integers
		if i, err := toInt(raw, kind); err == nil {
			return i, nil
		} else {
			return reflect.Value{}, err
		}
	}
}

func toFloat(raw any, bits int) (float64, error) {
	switch v := raw.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, fmt.Errorf("cannot parse %q as float", v.String())
		}
		return checkFloatRange(f, bits)
	case string:
		f, err := strconv.ParseFloat(v, bits)
		if err != nil {
			return 0, fmt.Errorf("cannot parse %q as float%d", v, bits)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("cannot use %s as float", describe(raw))
	}
}

func checkFloatRange(f float64, bits int) (float64, error) {
	if bits == 32 && (math.IsInf(f, 0) || f > math.MaxFloat32 || f < -math.MaxFloat32) {
		return 0, fmt.Errorf("value %v overflows float32", f)
	}
	return f, nil
}

func toInt(raw any, kind reflect.Kind) (reflect.Value, error) {
	unsigned := kind >= reflect.Uint && kind <= reflect.Uint64
	bits := intBits(kind)

	text, ok := numberText(raw)
	if !ok {
		return reflect.Value{}, fmt.Errorf("cannot use %s as %s", describe(raw), kind)
	}

	if unsigned {
		u, err := strconv.ParseUint(text, 0, bits)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("cannot parse %q as %s", text, kind)
		}
		return reflect.ValueOf(u).Convert(reflect.TypeOf(uint64(0))).Convert(intKindType(kind)), nil
	}

	n, err := strconv.ParseInt(text, 0, bits)
	if err != nil {
		return reflect.Value{}, fmt.Errorf("cannot parse %q as %s", text, kind)
	}
	return reflect.ValueOf(n).Convert(intKindType(kind)), nil
}

// numberText extracts an integral textual form. Float inputs must already
// be integral values; float precision is rejected for integers.
func numberText(raw any) (string, bool) {
	switch v := raw.(type) {
	case string:
		return v, true
	case json.Number:
		s := v.String()
		if strings.ContainsAny(s, ".eE") {
			f, err := v.Float64()
			if err != nil || f != math.Trunc(f) {
				return "", false
			}
			return strconv.FormatInt(int64(f), 10), true
		}
		return s, true
	case int:
		return strconv.Itoa(v), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case uint64:
		return strconv.FormatUint(v, 10), true
	case float64:
		if v != math.Trunc(v) {
			return "", false
		}
		return strconv.FormatInt(int64(v), 10), true
	default:
		return "", false
	}
}

func intBits(kind reflect.Kind) int {
	switch kind {
	case reflect.Int8, reflect.Uint8:
		return 8
	case reflect.Int16, reflect.Uint16:
		return 16
	case reflect.Int32, reflect.Uint32:
		return 32
	case reflect.Int64, reflect.Uint64:
		return 64
	default: // int, uint: assume 64-bit; range checked by ParseInt bitSize 64
		return 64
	}
}

// intKindType maps a kind to a concrete reflect.Type for Convert.
func intKindType(kind reflect.Kind) reflect.Type {
	switch kind {
	case reflect.Int:
		return reflect.TypeOf(int(0))
	case reflect.Int8:
		return reflect.TypeOf(int8(0))
	case reflect.Int16:
		return reflect.TypeOf(int16(0))
	case reflect.Int32:
		return reflect.TypeOf(int32(0))
	case reflect.Int64:
		return reflect.TypeOf(int64(0))
	case reflect.Uint:
		return reflect.TypeOf(uint(0))
	case reflect.Uint8:
		return reflect.TypeOf(uint8(0))
	case reflect.Uint16:
		return reflect.TypeOf(uint16(0))
	case reflect.Uint32:
		return reflect.TypeOf(uint32(0))
	default:
		return reflect.TypeOf(uint64(0))
	}
}

func describe(raw any) string {
	if raw == nil {
		return "null"
	}
	return fmt.Sprintf("%v(%T)", raw, raw)
}

func joinIf(prefix, part string) string {
	if prefix == "" {
		return part
	}
	return joinPath(prefix, part)
}

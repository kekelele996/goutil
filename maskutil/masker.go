// Package maskutil masks sensitive values (passwords, access tokens, phone
// numbers, etc.) inside arbitrary Go values, while keeping the data shape for
// troubleshooting and external output.
//
// The masking works directly on the source value through reflection, so:
//
//   - struct field names and struct tags are available for matching;
//   - named/custom types are preserved in the masked copy;
//   - the source value is never modified - a deep copy is returned;
//   - values nested in structs, maps, slices, arrays, interfaces and pointers
//     are all handled;
//   - cyclic/shared references terminate safely (repeated references are
//     rendered as nil).
//
// All output entries (Mask, JSON, String) are derived from the same masked
// copy, so the result is consistent across log printing and external data.
package maskutil

import (
	"fmt"
	"reflect"
	"strings"
	"time"
)

// DefaultPlaceholder is the fixed replacement for sensitive values.
const DefaultPlaceholder = "******"

var timeType = reflect.TypeOf(time.Time{})

// MaskFunc allows custom masking decisions. It is invoked for every struct
// field and every string-keyed map value before the built-in rules.
//
//   - path is the dotted path from the root, eg "Profile.Addresses.City" or
//     "Headers" for a map key.
//   - name is the current struct field name or map key (empty for root/slice
//     elements).
//   - rv is the original value about to be copied.
//
// Return ok=true to mask the value: replace is fitted into the target type
// (string placeholder, typed value, etc.); return ok=false to fall back to the
// built-in rules.
type MaskFunc func(path, name string, rv reflect.Value) (replace any, ok bool)

// Masker holds the masking rules. Configure it through New and OptionFunc,
// then it is safe for concurrent use by multiple goroutines.
type Masker struct {
	// placeholder is the default fixed replacement.
	placeholder string
	// names maps normalized sensitive names (field name, map key or name-tag
	// value) to a per-name placeholder; empty placeholder means use default.
	names map[string]string
	// nameTags are struct tags whose first segment participates in name
	// matching, defaults to "json".
	nameTags []string
	// markTags are struct tags whose mere presence marks a field sensitive,
	// eg `sensitive:""`. A non-empty tag value (eg `sensitive:"phone"`)
	// selects the per-name placeholder of that value.
	markTags []string
	// maskFuncs are custom decision funcs, tried first.
	maskFuncs []MaskFunc
}

// New creates a Masker with the given options.
func New(fns ...OptionFunc) *Masker {
	m := &Masker{
		placeholder: DefaultPlaceholder,
		names:       make(map[string]string),
		nameTags:    []string{"json"},
	}

	for _, fn := range fns {
		fn(m)
	}
	return m
}

// Placeholder returns the default fixed replacement.
func (m *Masker) Placeholder() string {
	return m.placeholder
}

// Mask returns a deep-copied value of v with sensitive values replaced.
// The input v is never modified.
func (m *Masker) Mask(v any) (out any, err error) {
	if v == nil {
		return nil, nil
	}

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("maskutil: mask value panic: %v", r)
		}
	}()

	st := &state{
		m:       m,
		visited: make(map[visit]bool),
	}

	cloned := st.clone(reflect.ValueOf(v))
	if !cloned.IsValid() {
		return nil, nil //nolint:nilnil
	}
	return cloned.Interface(), nil
}

// MustMask is like Mask but panics on error.
func (m *Masker) MustMask(v any) any {
	out, err := m.Mask(v)
	if err != nil {
		panic(err)
	}
	return out
}

// visit identifies a referenceable value for cyclic detection.
type visit struct {
	addr uintptr
	typ  reflect.Type
}

// state is the per-run traversal state.
type state struct {
	m       *Masker
	visited map[visit]bool
	path    []string
}

func (s *state) curPath(name string) string {
	if name == "" {
		return strings.Join(s.path, ".")
	}
	if len(s.path) == 0 {
		return name
	}
	return strings.Join(s.path, ".") + "." + name
}

// clone deep-copies rv without any name-based masking decision. Name matching
// is handled by the callers (struct fields and map keys), where the name is
// known.
func (s *state) clone(rv reflect.Value) reflect.Value {
	if !rv.IsValid() {
		return rv
	}

	t := rv.Type()

	switch t.Kind() {
	case reflect.Ptr:
		if rv.IsNil() {
			return reflect.Zero(t)
		}

		key := visit{addr: rv.Pointer(), typ: t}
		if s.visited[key] {
			// cyclic/shared reference: terminate with nil.
			return reflect.Zero(t)
		}
		s.visited[key] = true

		nv := reflect.New(t.Elem())
		nv.Elem().Set(forTarget(s.clone(rv.Elem()), t.Elem()))
		return nv

	case reflect.Interface:
		if rv.IsNil() {
			return reflect.Zero(t)
		}
		// Re-boxing the cloned concrete value keeps the dynamic/custom type.
		return s.clone(rv.Elem())

	case reflect.Struct:
		// time.Time (and named aliases of it) has only unexported fields;
		// copy the value as-is to keep a valid datetime and the named type.
		if t == timeType {
			return rv
		}
		if t.ConvertibleTo(timeType) {
			return rv.Convert(timeType).Convert(t)
		}

		nv := reflect.New(t).Elem()
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if sf.PkgPath != "" {
				continue // unexported field cannot be set
			}
			nv.Field(i).Set(forTarget(s.cloneField(sf, rv.Field(i)), sf.Type))
		}
		return nv

	case reflect.Map:
		if rv.IsNil() {
			return reflect.Zero(t)
		}

		key := visit{addr: rv.Pointer(), typ: t}
		if s.visited[key] {
			// cyclic/shared reference: terminate with nil.
			return reflect.Zero(t)
		}

		nm := reflect.MakeMapWithSize(t, rv.Len())
		if rv.Len() > 0 {
			s.visited[key] = true
		}

		for _, k := range rv.MapKeys() {
			nm.SetMapIndex(s.clone(k), forTarget(s.cloneMapValue(k, rv.MapIndex(k)), t.Elem()))
		}
		return nm

	case reflect.Slice:
		if rv.IsNil() {
			return reflect.Zero(t)
		}

		key := visit{addr: rv.Pointer(), typ: t}
		if s.visited[key] {
			// cyclic/shared reference: terminate with nil.
			return reflect.Zero(t)
		}

		ns := reflect.MakeSlice(t, rv.Len(), rv.Len())
		if rv.Len() > 0 {
			s.visited[key] = true
		}
		for i := 0; i < rv.Len(); i++ {
			ns.Index(i).Set(forTarget(s.clone(rv.Index(i)), t.Elem()))
		}
		return ns

	case reflect.Array:
		na := reflect.New(t).Elem()
		for i := 0; i < rv.Len(); i++ {
			na.Index(i).Set(forTarget(s.clone(rv.Index(i)), t.Elem()))
		}
		return na

	default:
		// Scalar values (string, numbers, bool, chan, func, ...) are
		// immutable when shared, or not meaningful to deep copy.
		return rv
	}
}

// cloneField copies a struct field value, applying field-level masking rules.
func (s *state) cloneField(sf reflect.StructField, fv reflect.Value) reflect.Value {
	path := s.curPath(sf.Name)

	if rep, ok := s.matchField(path, sf, fv); ok {
		return fitValue(fv.Type(), rep)
	}

	s.path = append(s.path, sf.Name)
	cloned := s.clone(fv)
	s.path = s.path[:len(s.path)-1]
	return cloned
}

// cloneMapValue copies a map value, applying name rules when the key is a
// string.
func (s *state) cloneMapValue(key, mv reflect.Value) reflect.Value {
	name := ""
	if key.Kind() == reflect.String {
		name = key.String()
	}

	path := s.curPath(name)

	if name != "" {
		if rep, ok := s.matchName(path, name, mv); ok {
			return fitValue(mv.Type(), rep)
		}
	}

	if name != "" {
		s.path = append(s.path, name)
	}
	cloned := s.clone(mv)
	if name != "" {
		s.path = s.path[:len(s.path)-1]
	}
	return cloned
}

// matchField decides the replacement for a struct field. It returns the raw
// replacement value (usually the placeholder string) and ok=true.
func (s *state) matchField(path string, sf reflect.StructField, fv reflect.Value) (any, bool) {
	// 1. custom funcs
	for _, fn := range s.m.maskFuncs {
		if rep, ok := fn(path, sf.Name, fv); ok {
			return rep, true
		}
	}

	// 2. Go field name
	if ph, ok := s.m.placeholderFor(sf.Name); ok {
		return ph, true
	}

	// 3. name-tag values, eg json:"password"
	for _, tag := range s.m.nameTags {
		tv, ok := sf.Tag.Lookup(tag)
		if !ok {
			continue
		}
		name := tagFirstName(tv)
		if name == "" || name == "-" {
			continue
		}
		if ph, ok := s.m.placeholderFor(name); ok {
			return ph, true
		}
	}

	// 4. presence tags, eg `sensitive:""` / `sensitive:"phone"`
	for _, tag := range s.m.markTags {
		tv, ok := sf.Tag.Lookup(tag)
		if !ok || tv == "-" {
			continue
		}

		ph := s.m.placeholder
		name := tagFirstName(tv)
		if name != "" && name != "true" {
			if rp, ok := s.m.names[normalizeName(name)]; ok && rp != "" {
				ph = rp
			}
		}
		return ph, true
	}

	return nil, false
}

// matchName decides the replacement for a string map key.
func (s *state) matchName(path, name string, rv reflect.Value) (any, bool) {
	for _, fn := range s.m.maskFuncs {
		if rep, ok := fn(path, name, rv); ok {
			return rep, true
		}
	}

	if ph, ok := s.m.placeholderFor(name); ok {
		return ph, true
	}
	return nil, false
}

// placeholderFor looks up the replacement placeholder for a sensitive name.
func (m *Masker) placeholderFor(name string) (string, bool) {
	ph, ok := m.names[normalizeName(name)]
	if !ok {
		return "", false
	}
	if ph == "" {
		return m.placeholder, true
	}
	return ph, true
}

// forTarget normalizes a cloned value before it is stored into target.
//
// A typed nil (nil pointer/map/slice/chan/func) boxed into an interface slot
// would become a non-nil interface containing a nil value; render it as a nil
// interface instead so cyclic/shared references (and nil values) surface as
// plain null in every output entry.
func forTarget(v reflect.Value, target reflect.Type) reflect.Value {
	if !v.IsValid() || v.Kind() == reflect.Interface || target.Kind() != reflect.Interface {
		return v
	}

	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		if v.IsNil() {
			return reflect.Zero(target)
		}
	}
	return v
}

// fitValue converts a raw replacement into a value assignable to t.
//
// String placeholders are fitted to string-kind types (including named custom
// string types), wrapped in pointers, boxed into interface fields, or encoded
// as []byte. Types that cannot hold the replacement receive the zero value,
// so the sensitive value never leaks.
func fitValue(t reflect.Type, rep any) reflect.Value {
	if rep == nil {
		return reflect.Zero(t)
	}

	rv := reflect.ValueOf(rep)
	if rt := rv.Type(); rt == t || rt.AssignableTo(t) {
		return rv
	}

	switch t.Kind() {
	case reflect.String:
		if rv.Kind() == reflect.String {
			return rv.Convert(t)
		}
	case reflect.Interface:
		return rv
	case reflect.Ptr:
		nv := reflect.New(t.Elem())
		nv.Elem().Set(fitValue(t.Elem(), rep))
		return nv
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 && rv.Kind() == reflect.String {
			bv := reflect.ValueOf([]byte(rv.String()))
			if bv.Type().ConvertibleTo(t) {
				return bv.Convert(t)
			}
		}
	}

	if rv.Type().ConvertibleTo(t) {
		return rv.Convert(t)
	}
	return reflect.Zero(t)
}

// normalizeName lower-cases and trims a name for matching. Matching is
// case-insensitive (eg "Password", "password" and "PASSWORD" are equivalent).
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// tagFirstName returns the first comma-separated segment of a struct tag
// value, mirroring encoding/json's name parsing.
func tagFirstName(tagVal string) string {
	if i := strings.IndexByte(tagVal, ','); i >= 0 {
		tagVal = tagVal[:i]
	}
	return strings.TrimSpace(tagVal)
}

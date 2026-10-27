package maskutil

import (
	"encoding/json"
	"fmt"
)

// OptionFunc configures a Masker.
type OptionFunc func(m *Masker)

// WithPlaceholder sets the fixed replacement for sensitive values.
func WithPlaceholder(placeholder string) OptionFunc {
	return func(m *Masker) {
		m.placeholder = placeholder
	}
}

// WithFields adds sensitive names. A name matches a struct field name, a map
// key or a name-tag value (eg the json tag name); matching is case
// insensitive. Matched values use the default placeholder.
func WithFields(names ...string) OptionFunc {
	return func(m *Masker) {
		for _, name := range names {
			m.names[normalizeName(name)] = ""
		}
	}
}

// WithRule adds a sensitive name with a dedicated placeholder.
//
//	WithRule("phone", "1**********")
func WithRule(name, placeholder string) OptionFunc {
	return func(m *Masker) {
		m.names[normalizeName(name)] = placeholder
	}
}

// WithRules adds sensitive name -> placeholder rules in bulk.
func WithRules(rules map[string]string) OptionFunc {
	return func(m *Masker) {
		for name, ph := range rules {
			m.names[normalizeName(name)] = ph
		}
	}
}

// WithNameTags sets the struct tags whose first segment participates in name
// matching (defaults to "json"). For example, with the field
//
//	Phone string `json:"mobile"`
//
// and the sensitive name "mobile", the field is masked.
func WithNameTags(tags ...string) OptionFunc {
	return func(m *Masker) {
		m.nameTags = append(m.nameTags[:0:0], tags...)
	}
}

// WithMarkTags sets tags whose mere presence marks a field sensitive:
//
//	Token string `sensitive:""`
//
// The tag value may reference a registered rule for its own placeholder:
//
//	Phone string `sensitive:"phone"` // uses the placeholder of rule "phone"
//
// "-" explicitly opts out.
func WithMarkTags(tags ...string) OptionFunc {
	return func(m *Masker) {
		m.markTags = append(m.markTags[:0:0], tags...)
	}
}

// WithMaskFunc appends a custom masking decision func. Custom funcs are tried
// before the built-in name/tag rules.
func WithMaskFunc(fn MaskFunc) OptionFunc {
	return func(m *Masker) {
		maskFuncs := make([]MaskFunc, 0, len(m.maskFuncs)+1)
		maskFuncs = append(maskFuncs, m.maskFuncs...)
		maskFuncs = append(maskFuncs, fn)
		m.maskFuncs = maskFuncs
	}
}

// CommonFields are commonly sensitive field/map-key names: credentials,
// tokens, contact information, etc.
var CommonFields = []string{
	"password",
	"passwd",
	"pwd",
	"secret",
	"token",
	"access_token",
	"accesstoken",
	"refresh_token",
	"api_key",
	"apikey",
	"authorization",
	"credential",
	"credentials",
	"private_key",
	"phone",
	"mobile",
	"tel",
	"telephone",
}

// WithCommonFields adds the CommonFields preset to the Masker.
func WithCommonFields() OptionFunc {
	return WithFields(CommonFields...)
}

// Default returns a Masker preloaded with the CommonFields preset and the
// DefaultPlaceholder.
func Default() *Masker {
	return New(WithCommonFields())
}

/*************************************************************
  output entries - all derived from the same masked deep copy
 *************************************************************/

// JSON masks v and JSON-marshals the masked copy. It is equivalent to
// json.Marshal(m.Mask(v)), so structured output and JSON output are always
// consistent.
func (m *Masker) JSON(v any) ([]byte, error) {
	masked, err := m.Mask(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(masked)
}

// MustJSON is like JSON but panics on error.
func (m *Masker) MustJSON(v any) []byte {
	bts, err := m.JSON(v)
	if err != nil {
		panic(err)
	}
	return bts
}

// JSONIndent is like JSON but indents the output with indent.
func (m *Masker) JSONIndent(v any, prefix, indent string) ([]byte, error) {
	masked, err := m.Mask(v)
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(masked, prefix, indent)
}

// String masks v and renders it for log output. It is derived from the same
// masked copy as JSON, so the two entries agree on every value.
func (m *Masker) String(v any) string {
	masked, err := m.Mask(v)
	if err != nil {
		return fmt.Sprintf("maskutil: %v", err)
	}
	return fmt.Sprintf("%+v", masked)
}

/*************************************************************
  package-level helpers using option funcs
 *************************************************************/

// Mask creates a one-off Masker from fns and masks v.
func Mask(v any, fns ...OptionFunc) (any, error) {
	return New(fns...).Mask(v)
}

// JSON creates a one-off Masker from fns and JSON-marshals the masked v.
func JSON(v any, fns ...OptionFunc) ([]byte, error) {
	return New(fns...).JSON(v)
}

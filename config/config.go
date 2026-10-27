// Package config provides ordered, layered settings loading for services
// that keep one common settings file plus one per environment
// (dev/staging/prod).
//
// Design:
//
//   - Layers are read and deep-merged in a fixed order; later layers win.
//     Nested tables merge key by key, leaf values replace leaf values.
//   - Files are parsed into in-memory maps only. The process environment
//     (os.Environ) is never read as a layer nor modified.
//   - The merged values are converted strictly into the basic field types
//     declared on a struct. A bad value is reported by setting name with
//     the winning layer, instead of silently falling back to the default.
//   - The Report lists every setting defined by more than one layer and
//     states which layer finally provides it (Report.SourceOf).
//   - Load/Reload always build a brand-new struct from scratch; it is
//     swapped in only after the whole load succeeds. A failed reload never
//     leaves half-applied values and never disturbs the previously usable
//     settings.
//
// Supported formats: JSON objects (.json) and dotenv/INI-like KEY=VALUE
// contents (.env/.ini/.properties/.conf), where dotted keys expand into
// nested tables. Field types may be bool, string, sized int/uint/float,
// pointers and slices of those, and nested structs.
package config

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

// DefaultTag is the default struct tag holding the setting key.
const DefaultTag = "config"

// Option customizes a Loader.
type Option func(l *Loader)

// WithTag sets the struct tag providing setting keys. Default: "config".
// Fields without a usable tag fall back to their Go field name.
func WithTag(tag string) Option {
	return func(l *Loader) {
		if tag != "" {
			l.tag = tag
		}
	}
}

// WithDefaults provides the baseline values every load starts from.
// d must be a value (or pointer) of the same struct type as the load
// target. Missing keys keep these defaults; a present-but-mistyped key is
// still a type error, it never silently falls back.
func WithDefaults(d any) Option {
	return func(l *Loader) {
		l.defaults = checkDefaults(d, l.rt)
	}
}

// WithRejectUnknown makes keys that no struct field accepts fatal.
// By default they are only listed in the Report.
func WithRejectUnknown() Option {
	return func(l *Loader) {
		l.reject = true
	}
}

// Loader binds a target struct pointer to an ordered set of layers and
// applies fresh, fully-validated values atomically.
type Loader struct {
	target   reflect.Value // *T, user-owned
	rt       reflect.Type  // T
	layers   []Layer
	tag      string
	defaults reflect.Value // *T, independent copy
	reject   bool

	// loadMu serializes full reloads; mu guards the final swap only.
	loadMu sync.Mutex
	mu     sync.RWMutex
	snap   *Snapshot
}

// Snapshot is one successfully applied configuration generation.
// It is immutable: holding one after a Reload keeps returning the values
// that were valid when the snapshot was taken.
type Snapshot struct {
	cfg    any // *T, independent from the user target
	report *Report
	loadAt time.Time
}

// Config returns the decoded struct pointer (*T).
func (s *Snapshot) Config() any { return s.cfg }

// Report returns findings of the load that produced this snapshot.
func (s *Snapshot) Report() *Report { return s.report }

// LoadAt returns when this generation was applied.
func (s *Snapshot) LoadAt() time.Time { return s.loadAt }

// New validates target and layers and returns a Loader without loading.
//
// target must be a non-nil pointer to a struct. Layer names must be unique;
// empty names default to the file base name.
func New(target any, layers []Layer, opts ...Option) (*Loader, error) {
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Ptr || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("config: target must be a non-nil pointer to a struct, got %T", target)
	}
	if len(layers) == 0 {
		return nil, fmt.Errorf("config: at least one layer is required")
	}

	names := make(map[string]bool, len(layers))
	for i := range layers {
		ly := &layers[i]
		if ly.Name == "" {
			if ly.Path != "" {
				ly.Name = filepath.Base(ly.Path)
			} else {
				ly.Name = fmt.Sprintf("layer%d", i+1)
			}
		}
		if names[ly.Name] {
			return nil, fmt.Errorf("config: duplicated layer name %q", ly.Name)
		}
		names[ly.Name] = true
		if ly.Text == "" && ly.Path == "" {
			return nil, fmt.Errorf("config: layer %q needs Path or Text", ly.Name)
		}
	}

	l := &Loader{
		target: rv,
		rt:     rv.Elem().Type(),
		layers: layers,
		tag:    DefaultTag,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

func checkDefaults(d any, rt reflect.Type) reflect.Value {
	dv := reflect.ValueOf(d)
	if dv.Kind() == reflect.Ptr {
		if dv.IsNil() || dv.Elem().Type() != rt {
			panic(fmt.Sprintf("config: defaults type %T does not match target %s", d, rt))
		}
		cp := reflect.New(rt)
		cp.Elem().Set(dv.Elem())
		return cp
	}
	if dv.Type() != rt {
		panic(fmt.Sprintf("config: defaults type %T does not match target %s", d, rt))
	}
	cp := reflect.New(rt)
	cp.Elem().Set(dv)
	return cp
}

// Load performs the first load. It behaves like Reload.
func (l *Loader) Load() error {
	return l.reload()
}

// Reload re-reads every layer, re-merges from scratch and decodes into a
// brand-new struct.
//
// The new generation is swapped into the target only if the whole load
// succeeds. On any error the target keeps its previous, usable values and
// the previous Snapshot stays valid; there is no half-applied state.
// Reloads are serialized and block Snapshot reads only for the final swap.
func (l *Loader) Reload() error {
	return l.reload()
}

func (l *Loader) reload() error {
	l.loadMu.Lock()
	defer l.loadMu.Unlock()

	// ---- phase 1: build everything new without touching the target ----
	report := &Report{
		Order:   make([]string, 0, len(l.layers)),
		Files:   make(map[string]string, len(l.layers)),
		sources: make(map[string]string),
	}

	parsed := make([]parsedLayer, 0, len(l.layers))
	var readErrs []error
	for _, ly := range l.layers {
		report.Order = append(report.Order, ly.Name)

		data, err, skipped := ly.read()
		if err != nil {
			readErrs = append(readErrs, err)
			continue
		}
		report.Files[ly.Name] = ly.Path
		if skipped {
			report.Skipped = append(report.Skipped, ly.Name)
			continue
		}
		parsed = append(parsed, parsedLayer{layer: ly, data: data})
	}
	if len(readErrs) > 0 {
		return &LoadError{Stage: "read", Errs: readErrs, Report: report}
	}

	// ---- phase 2: ordered merge with provenance ----
	merged, sources, duplicates, conflicts := mergeLayers(parsed)
	report.Issues = append(report.Issues, duplicates...)
	report.sources = sources
	if len(conflicts) > 0 {
		report.Issues = append(report.Issues, conflicts...)
		return &LoadError{Stage: "merge", Issues: conflicts, Report: report}
	}

	// ---- phase 3: decode into a FRESH struct (defaults overlaid first) ----
	fresh := reflect.New(l.rt)
	if l.defaults.IsValid() {
		fresh.Elem().Set(l.defaults.Elem())
	}

	d := &decoder{sources: sources, tag: l.tag, rejectUnknown: l.reject}
	d.decodeData(fresh.Elem(), merged)

	var typeErrs, fatalUnknown []Issue
	for _, is := range d.issues {
		switch is.Kind {
		case IssueTypeError:
			typeErrs = append(typeErrs, is)
		case IssueUnknownKey:
			if l.reject {
				fatalUnknown = append(fatalUnknown, is)
			}
		}
		report.Issues = append(report.Issues, is)
	}
	if len(typeErrs) > 0 || len(fatalUnknown) > 0 {
		fatal := append(append([]Issue{}, typeErrs...), fatalUnknown...)
		return &LoadError{Stage: "decode", Issues: fatal, Report: report}
	}

	// ---- phase 4: commit atomically ----
	snap := &Snapshot{cfg: fresh.Interface(), report: report, loadAt: time.Now()}

	l.mu.Lock()
	l.target.Elem().Set(fresh.Elem())
	l.snap = snap
	l.mu.Unlock()
	return nil
}

// Snapshot returns the current applied generation, or nil before the first
// successful load. The returned pointer stays valid after later reloads.
func (l *Loader) Snapshot() *Snapshot {
	l.mu.RLock()
	snap := l.snap
	l.mu.RUnlock()
	return snap
}

// Loaded reports whether a usable generation has been applied.
func (l *Loader) Loaded() bool {
	return l.Snapshot() != nil
}

// Report returns findings of the last successful load, nil otherwise.
func (l *Loader) Report() *Report {
	if snap := l.Snapshot(); snap != nil {
		return snap.report
	}
	return nil
}

// LoadInto one-shot helper: creates a Loader for target and layers, loads
// once and returns the Report.
func LoadInto(target any, layers []Layer, opts ...Option) (*Report, error) {
	l, err := New(target, layers, opts...)
	if err != nil {
		return nil, err
	}
	if err = l.Load(); err != nil {
		if le, ok := err.(*LoadError); ok {
			return le.Report, err
		}
		return nil, err
	}
	return l.Report(), nil
}

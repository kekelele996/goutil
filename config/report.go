package config

import (
	"fmt"
	"strings"
)

// IssueKind kind of the reported setting issue.
type IssueKind string

// reporting issue kinds.
const (
	// IssueDuplicateKey the setting is defined in more than one layer.
	// It is NOT an error: later layers are allowed to override earlier
	// ones. The issue is reported only to make the override visible and
	// to show which layer finally provides the value.
	IssueDuplicateKey IssueKind = "duplicate_key"

	// IssueMergeConflict a key is a nested table in one layer but a
	// scalar value in another, so the two shapes cannot be merged.
	IssueMergeConflict IssueKind = "merge_conflict"

	// IssueTypeError the winning raw value cannot be converted to the
	// basic type declared on the struct field.
	IssueTypeError IssueKind = "type_error"

	// IssueUnknownKey the merged data contains a key that no struct
	// field accepts. Non-fatal unless RejectUnknown is enabled.
	IssueUnknownKey IssueKind = "unknown_key"
)

// Issue describes a single setting found while loading.
//
// Setting is always the dotted full name, eg "db.port". Layers lists every
// layer that defines the setting (in merge order), and Winner is the layer
// whose value is finally used.
type Issue struct {
	// Kind of the issue.
	Kind IssueKind
	// Setting dotted setting name. eg "db.port"
	Setting string
	// Layers all layers defining the setting, in merge order.
	Layers []string
	// Winner layer whose value is finally adopted.
	Winner string
	// Want is the type declared on the struct field.
	Want string
	// Raw winning value from the merged data.
	Raw any
	// Msg human readable detail.
	Msg string
}

// String for render issue
func (i Issue) String() string {
	var b strings.Builder

	b.WriteString(string(i.Kind))
	b.WriteString(": setting ")
	b.WriteString(strconvQuote(i.Setting))

	if len(i.Layers) > 0 {
		b.WriteString(" defined in layers [")
		b.WriteString(strings.Join(i.Layers, ", "))
		b.WriteString("]")
	}
	if i.Winner != "" {
		b.WriteString(", adopted from layer ")
		b.WriteString(strconvQuote(i.Winner))
	}
	if i.Want != "" {
		b.WriteString(", want type ")
		b.WriteString(i.Want)
	}
	if i.Raw != nil {
		b.WriteString(", got ")
		fmt.Fprintf(&b, "%v", i.Raw)
	}
	if i.Msg != "" {
		b.WriteString(": ")
		b.WriteString(i.Msg)
	}
	return b.String()
}

// Report of one load attempt.
//
// Every setting can be traced back to the layer that finally provides it
// with SourceOf, and all notable findings are collected in Issues.
type Report struct {
	// Order effective layer merge order, including skipped layers.
	Order []string
	// Files resolved file path per layer. empty for in-memory layers.
	Files map[string]string
	// Skipped layers skipped because their file was not found and the
	// layer was marked Optional.
	Skipped []string
	// Issues collected during the load.
	Issues []Issue

	// sources maps dotted setting name to the winning layer.
	sources map[string]string
}

// SourceOf reports which layer finally provides the setting.
func (r *Report) SourceOf(setting string) (layer string, ok bool) {
	layer, ok = r.sources[setting]
	return
}

// IssuesOf returns all issues of the given kind.
func (r *Report) IssuesOf(kind IssueKind) []Issue {
	var out []Issue
	for _, is := range r.Issues {
		if is.Kind == kind {
			out = append(out, is)
		}
	}
	return out
}

// Duplicates lists settings defined in more than one layer.
func (r *Report) Duplicates() []Issue { return r.IssuesOf(IssueDuplicateKey) }

// TypeErrors lists settings whose winning value does not fit the field type.
func (r *Report) TypeErrors() []Issue { return r.IssuesOf(IssueTypeError) }

// LoadError is returned when a load cannot be applied. It aggregates every
// problem found in the attempt so that all bad settings can be fixed at once.
//
// The previously usable config (if any) is left untouched when LoadError is
// returned.
type LoadError struct {
	// Stage where the load failed: "read", "merge" or "decode".
	Stage string
	// Errs low-level errors, eg file read or syntax errors.
	Errs []error
	// Issues structured findings, eg type errors and merge conflicts.
	Issues []Issue
	// Report partial trace information collected before the failure.
	Report *Report
}

// Error implements error
func (e *LoadError) Error() string {
	var b strings.Builder

	b.WriteString("config: load failed at stage ")
	b.WriteString(e.Stage)

	for _, err := range e.Errs {
		b.WriteString("; ")
		b.WriteString(err.Error())
	}
	for _, is := range e.Issues {
		b.WriteString("; ")
		b.WriteString(is.String())
	}
	return b.String()
}

func strconvQuote(s string) string {
	return fmt.Sprintf("%q", s)
}

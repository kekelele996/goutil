package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gookit/goutil/internal/comfunc"
)

// Format of a layer settings source.
type Format string

// supported layer formats.
const (
	// FormatJSON nested JSON object. Numbers keep their literal form
	// (json.Number), so "8080" is not mistaken for a float.
	FormatJSON Format = "json"
	// FormatKV simple KEY=VALUE contents like dotenv/INI/properties.
	// Dotted keys expand into nested tables, eg "db.port=5432" becomes
	// {"db":{"port":"5432"}}; every value starts as a string and is
	// converted to the field type during decode.
	FormatKV Format = "kv"
)

// Layer is one settings source. Layers are applied in the order given,
// later layers overriding earlier ones.
type Layer struct {
	// Name identifies the layer in reports, eg "common", "dev", "prod".
	// Defaults to the file base name. Must be unique among layers.
	Name string
	// Path settings file. Ignored when Text is set.
	Path string
	// Text in-memory settings contents. Useful for embedded configs.
	Text string
	// Format forces the contents format. Auto-detected from the file
	// extension when empty: .json -> JSON; .env/.ini/.properties/.conf/
	// .txt -> KV.
	Format Format
	// Optional marks a missing file as skipped instead of an error.
	Optional bool
}

// FileLayer quickly create a file-backed layer.
func FileLayer(name, path string) Layer {
	return Layer{Name: name, Path: path}
}

// parsedLayer is a layer with its resolved name, format and raw data.
type parsedLayer struct {
	layer Layer
	data  map[string]any
}

// detectFormat resolves the layer format from Format or the file extension.
func (l Layer) detectFormat() (Format, error) {
	if l.Format != "" {
		switch l.Format {
		case FormatJSON, FormatKV:
			return l.Format, nil
		default:
			return "", fmt.Errorf("layer %q: unsupported format %q", l.Name, l.Format)
		}
	}

	ext := strings.ToLower(filepath.Ext(l.Path))
	switch ext {
	case ".json":
		return FormatJSON, nil
	case "", ".env", ".ini", ".properties", ".conf", ".txt":
		return FormatKV, nil
	default:
		return "", fmt.Errorf("layer %q: cannot detect format of %q, set Layer.Format", l.Name, l.Path)
	}
}

// read loads and parses the layer.
//
// It returns (nil, nil) when an Optional layer's file is missing.
// The process environment is never touched: KV contents are parsed into a
// local map only.
func (l Layer) read() (map[string]any, error, bool) {
	contents := l.Text
	if l.Text == "" && l.Path != "" {
		b, err := os.ReadFile(l.Path)
		if err != nil {
			if os.IsNotExist(err) && l.Optional {
				return nil, nil, true
			}
			return nil, fmt.Errorf("layer %q: read file %s: %w", l.Name, l.Path, err), false
		}
		contents = string(b)
	}

	format, err := l.detectFormat()
	if err != nil {
		return nil, err, false
	}

	data, err := parseLayerData(format, contents)
	if err != nil {
		return nil, fmt.Errorf("layer %q: %w", l.Name, err), false
	}
	return data, nil, false
}

func parseLayerData(format Format, contents string) (map[string]any, error) {
	switch format {
	case FormatJSON:
		return parseJSONData(contents)
	case FormatKV:
		return parseKVData(contents)
	default:
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}

// parseJSONData decodes one JSON object. Numbers are decoded as json.Number
// so integer fields never receive a lossy float.
func parseJSONData(contents string) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(contents)))
	dec.UseNumber()

	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}
	if err := dec.Decode(&raw); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse json: unexpected content after the first object")
		}
		return nil, fmt.Errorf("parse json: %w", err)
	}

	data, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parse json: top-level value must be an object")
	}
	return data, nil
}

// parseKVData parses KEY=VALUE lines (comments start with #, ; or //) and
// expands dotted keys into nested tables.
//
// "DB_HOST=x" stays flat, while "db.host=x" becomes {"db":{"host":"x"}}.
func parseKVData(contents string) (map[string]any, error) {
	flat, err := comfunc.ParseEnvLines(contents, comfunc.ParseEnvLineOption{})
	if err != nil {
		return nil, fmt.Errorf("parse kv contents: %w", err)
	}

	data := make(map[string]any, len(flat))
	for key, val := range flat {
		parts := splitDottedKey(key)
		if err := setNested(data, parts, val, key); err != nil {
			return nil, err
		}
	}
	return data, nil
}

func splitDottedKey(key string) []string {
	parts := strings.Split(key, ".")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// setNested places value at data[p0][p1]..., turning intermediate keys
// into tables. A key cannot be both a table and a scalar within one layer.
func setNested(data map[string]any, parts []string, value any, fullKey string) error {
	if len(parts) == 0 {
		return fmt.Errorf("empty key in %q", fullKey)
	}

	cur := data
	for i, part := range parts {
		if i == len(parts)-1 {
			cur[part] = value
			return nil
		}

		next, ok := cur[part].(map[string]any)
		if !ok {
			if _, exists := cur[part]; exists {
				return fmt.Errorf("key %q mixes a table and a scalar value", joinPath(parts[:i+1]...))
			}
			next = make(map[string]any)
			cur[part] = next
		}
		cur = next
	}
	return nil
}

// asTable reports whether v is a nested table (map[string]any).
func asTable(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

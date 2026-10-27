package config

import "strings"

// joinPath joins setting name parts with a dot.
func joinPath(parts ...string) string {
	return strings.Join(parts, ".")
}

// tableMerger deep-merges parsed layers in order while tracing the origin
// layer of every leaf and table node.
type tableMerger struct {
	// sources maps dotted leaf path to the winning layer.
	sources map[string]string
	// owner maps dotted node path (leaf or table) to the last layer
	// that touched it. Used when shapes clash across layers.
	owner map[string]string
	// seen maps dotted leaf path to all layers defining it.
	seen map[string][]string

	duplicates []Issue
	conflicts  []Issue
}

// mergeLayers deep-merges parsed layers in order. Later layers win:
//
//   - table + table: merged key by key;
//   - leaf + leaf: later value replaces earlier one;
//   - table + leaf (mixed shape): reported as a merge conflict and the
//     later value replaces the earlier one so the load can continue and
//     collect further errors.
//
// For every leaf key it records all layers defining it (seen) and the
// layer whose value finally wins (sources).
func mergeLayers(layers []parsedLayer) (merged map[string]any, sources map[string]string, duplicates, conflicts []Issue) {
	m := &tableMerger{
		sources: make(map[string]string),
		owner:   make(map[string]string),
		seen:    make(map[string][]string),
	}

	merged = make(map[string]any)
	for _, pl := range layers {
		m.mergeTable("", merged, pl.data, pl.layer.Name)
	}

	for setting, ls := range m.seen {
		if len(ls) > 1 {
			m.duplicates = append(m.duplicates, Issue{
				Kind:    IssueDuplicateKey,
				Setting: setting,
				Layers:  append([]string(nil), ls...),
				Winner:  m.sources[setting],
				Raw:     leafAt(merged, setting),
			})
		}
	}
	return merged, m.sources, m.duplicates, m.conflicts
}

// leafAt looks up the winning raw value of a dotted leaf path.
func leafAt(data map[string]any, path string) any {
	parts := strings.Split(path, ".")
	cur := data
	for i, p := range parts {
		v := cur[p]
		if i == len(parts)-1 {
			return v
		}
		t, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		cur = t
	}
	return nil
}

// mergeTable merges src into dst for one layer.
func (m *tableMerger) mergeTable(prefix string, dst, src map[string]any, layer string) {
	for key, in := range src {
		path := key
		if prefix != "" {
			path = joinPath(prefix, key)
		}

		inMap, inIsMap := asTable(in)
		if old, ok := dst[key]; ok {
			oldMap, oldIsMap := asTable(old)
			switch {
			case oldIsMap && inIsMap:
				// both tables: descend, no leaf override yet.
				m.owner[path] = layer
				m.mergeTable(path, oldMap, inMap, layer)
				continue
			case oldIsMap != inIsMap:
				shape := "table"
				if inIsMap {
					shape = "scalar"
				}
				m.conflicts = append(m.conflicts, Issue{
					Kind:    IssueMergeConflict,
					Setting: path,
					Layers:  []string{m.owner[path], layer},
					Winner:  layer,
					Raw:     in,
					Msg:     "earlier layer defines a table while this layer defines a " + shape,
				})
			}
		}

		m.owner[path] = layer
		if inIsMap {
			// new table replacing a scalar, or a brand-new key:
			// build it in a fresh map to avoid aliasing parsed data.
			next := make(map[string]any, len(inMap))
			dst[key] = next
			m.mergeTable(path, next, inMap, layer)
			continue
		}

		// leaf value (scalar or slice): later wins.
		ls := m.seen[path]
		if len(ls) == 0 || ls[len(ls)-1] != layer {
			m.seen[path] = append(ls, layer)
		}
		m.sources[path] = layer
		dst[key] = in
	}
}

package mcp

import (
	"encoding/json"
	"reflect"
)

// Change is how a refreshed definition relates to the approved one (PRD §7.4).
type Change string

// Changes.
const (
	ChangeNone         Change = "none"
	ChangeNew          Change = "new"
	ChangeCompatible   Change = "compatible"   // descriptions or titles changed, or optional inputs added
	ChangeIncompatible Change = "incompatible" // anything else: inputs removed or made required, types, annotations
)

// Compare classifies a new definition against the approved one. Both are MCP definition objects (a
// tool, resource, resource template or prompt). A nil approved definition is ChangeNew. Equal digests are
// ChangeNone; the caller passes digests to avoid canonicalising twice.
func Compare(approved, next json.RawMessage, approvedDigest, nextDigest string) Change {
	if approved == nil {
		return ChangeNew
	}
	if approvedDigest != "" && approvedDigest == nextDigest {
		return ChangeNone
	}
	var a, b map[string]any
	if json.Unmarshal(approved, &a) != nil || json.Unmarshal(next, &b) != nil {
		return ChangeIncompatible
	}
	if compatible(a, b) {
		if reflect.DeepEqual(a, b) {
			return ChangeNone
		}
		return ChangeCompatible
	}
	return ChangeIncompatible
}

// descriptive keys may change freely.
var descriptive = map[string]bool{"description": true, "title": true, "_meta": true, "icons": true}

// compatible reports whether b differs from a only in descriptive keys, or by optional input
// properties and optional prompt arguments added.
func compatible(a, b map[string]any) bool {
	for k, av := range a {
		if descriptive[k] {
			continue
		}
		bv, ok := b[k]
		if !ok {
			return false
		}
		switch k {
		case "inputSchema":
			if !schemaCompatible(av, bv) {
				return false
			}
		case "arguments":
			if !argumentsCompatible(av, bv) {
				return false
			}
		default:
			if !reflect.DeepEqual(stripDescriptive(av), stripDescriptive(bv)) {
				return false
			}
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok && !descriptive[k] {
			return false
		}
	}
	return true
}

// schemaCompatible allows added optional properties and description changes anywhere; everything
// else must be equal.
func schemaCompatible(av, bv any) bool {
	a, ok1 := av.(map[string]any)
	b, ok2 := bv.(map[string]any)
	if !ok1 || !ok2 {
		return reflect.DeepEqual(stripDescriptive(av), stripDescriptive(bv))
	}
	ap, _ := a["properties"].(map[string]any)
	bp, _ := b["properties"].(map[string]any)
	if !reflect.DeepEqual(stringSet(a["required"]), stringSet(b["required"])) {
		return false
	}
	for name, v := range ap {
		w, ok := bp[name]
		if !ok || !schemaCompatible(v, w) {
			return false
		}
	}
	for name := range bp {
		if _, ok := ap[name]; !ok && stringSet(b["required"])[name] {
			return false
		}
	}
	rest := func(m map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			if k != "properties" && k != "required" {
				out[k] = v
			}
		}
		return out
	}
	return reflect.DeepEqual(stripDescriptive(rest(a)), stripDescriptive(rest(b)))
}

func argumentsCompatible(av, bv any) bool {
	al, ok1 := av.([]any)
	bl, ok2 := bv.([]any)
	if !ok1 || !ok2 {
		return false
	}
	byName := func(l []any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, x := range l {
			if m, ok := x.(map[string]any); ok {
				n, _ := m["name"].(string)
				out[n] = m
			}
		}
		return out
	}
	am, bm := byName(al), byName(bl)
	for n, a := range am {
		b, ok := bm[n]
		if !ok || a["required"] != b["required"] {
			return false
		}
	}
	for n, b := range bm {
		if _, ok := am[n]; !ok && b["required"] == true {
			return false
		}
	}
	return true
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}

func stripDescriptive(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if !descriptive[k] {
				out[k] = stripDescriptive(x)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = stripDescriptive(x)
		}
		return out
	}
	return v
}

// CrossesRiskLine reports whether a change must go to review whatever the drift policy (PRD §7.4): a
// tool that becomes destructive or open-world, or a new tool that is.
func CrossesRiskLine(before *Annotations, after Annotations, kind Kind) bool {
	if kind != KindTool {
		return false
	}
	if before == nil {
		return after.Destructive() || after.OpenWorld()
	}
	return !before.Destructive() && after.Destructive() || !before.OpenWorld() && after.OpenWorld()
}

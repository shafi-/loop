package knowledge

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// planSchema is the structured-output contract for the area-partition
// call. Every field is required so OpenAI strict mode accepts it; empty
// arrays express "none".
var planSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"areas": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":  map[string]any{"type": "string"},
					"scope": map[string]any{"type": "string"},
					"dirs":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"files": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required":             []string{"name", "scope", "dirs", "files"},
				"additionalProperties": false,
			},
		},
	},
	"required":             []string{"areas"},
	"additionalProperties": false,
}

// area is one model-proposed partition unit before validation.
type area struct {
	Name  string   `json:"name"`
	Scope string   `json:"scope"`
	Dirs  []string `json:"dirs"`
	Files []string `json:"files"`
}

type planResult struct {
	Areas []area `json:"areas"`
}

// slugify turns an area name into a filename-safe slug.
func slugify(name string) string {
	var b strings.Builder
	lastDash := true // no leading dash
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == ' ':
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

// parsePlan validates the model's partition against the real workspace:
// unknown or escaping paths are dropped, empty areas removed, slugs
// deduplicated, and the cap applied (deterministic order).
func parsePlan(data string, ws string) ([]Note, error) {
	var p planResult
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return nil, fmt.Errorf("area plan was not valid JSON: %w", err)
	}
	used := map[string]bool{}
	var notes []Note
	for _, a := range p.Areas {
		if len(notes) >= MaxAreas {
			break
		}
		slug := slugify(a.Name)
		if slug == "" || used[slug] {
			continue
		}
		n := Note{Slug: slug, Title: strings.TrimSpace(a.Name), Scope: strings.TrimSpace(a.Scope)}
		n.Dirs = cleanPaths(a.Dirs, true)
		n.Files = cleanPaths(a.Files, false)
		if len(n.Dirs) == 0 && len(n.Files) == 0 {
			continue // an area covering nothing is not an area
		}
		used[slug] = true
		notes = append(notes, n)
	}
	if len(notes) == 0 {
		return nil, fmt.Errorf("area plan proposed no usable areas")
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].Slug < notes[j].Slug })
	return notes, nil
}

// cleanPaths keeps workspace-relative paths inside the workspace.
func cleanPaths(in []string, dirs bool) []string {
	var out []string
	for _, p := range in {
		p = strings.Trim(strings.TrimSpace(filepath.ToSlash(p)), "/")
		if p == "" || p == "." {
			continue
		}
		clean := filepath.Clean(p)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "..") {
			continue
		}
		out = append(out, filepath.ToSlash(clean))
	}
	sort.Strings(out)
	return out
}

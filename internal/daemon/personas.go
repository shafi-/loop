package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/shafi-/loop/internal/config"
)

// Persona management: the two libraries (project and global) listed,
// read for editing, and deleted — deletion refuses while any room or
// pipeline in the workspace still references the persona.

// personaJSON is one listed persona: identity plus where it lives.
type personaJSON struct {
	Name  string `json:"name"`
	Role  string `json:"role,omitempty"`
	Scope string `json:"scope"` // "project" | "global"
	Path  string `json:"path"`
}

func globalPersonaDirOr(w http.ResponseWriter) (string, bool) {
	dir, err := config.GlobalPersonaDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "resolving the global persona directory: %v", err)
		return "", false
	}
	return dir, true
}

// handlePersonas lists both libraries, project first.
func (s *Server) handlePersonas(w http.ResponseWriter, r *http.Request) {
	out := []personaJSON{}
	add := func(scope, dir string) {
		for _, f := range config.ScanPersonaDir(dir) {
			if f.Err != nil {
				continue // broken files aren't manageable personas
			}
			out = append(out, personaJSON{Name: f.Name, Role: f.Role, Scope: scope, Path: f.Path})
		}
	}
	add("project", "personas")
	if dir, ok := globalPersonaDirOr(w); ok {
		add("global", dir)
	}
	writeJSON(w, http.StatusOK, map[string]any{"personas": out})
}

// personaFileOf finds the file backing one persona name in one scope.
func personaFileOf(scope, name string) (string, bool) {
	var dir string
	switch scope {
	case "project":
		dir = "personas"
	case "global":
		g, err := config.GlobalPersonaDir()
		if err != nil {
			return "", false
		}
		dir = g
	default:
		return "", false
	}
	for _, f := range config.ScanPersonaDir(dir) {
		if f.Err == nil && f.Name == name {
			return f.Path, true
		}
	}
	return "", false
}

// handlePersonaDetail returns a persona's YAML for the edit page.
func (s *Server) handlePersonaDetail(w http.ResponseWriter, r *http.Request) {
	scope, name := r.URL.Query().Get("scope"), r.URL.Query().Get("name")
	path, ok := personaFileOf(scope, name)
	if !ok {
		writeError(w, http.StatusNotFound, "no %s persona %q", scope, name)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading %s: %v", path, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"yaml": string(data), "path": path})
}

// personaRefRe builds a matcher for `persona: <name>` entries in
// workspace documents.
func personaRefRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*-?\s*persona:\s*` + regexp.QuoteMeta(name) + `\s*$`)
}

// handlePersonaDelete removes a persona — unless a workspace room or
// pipeline still references it, which would silently break that file's
// next load.
func (s *Server) handlePersonaDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scope string `json:"scope"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with a \"name\"")
		return
	}
	if req.Scope == "" {
		req.Scope = "project"
	}
	path, ok := personaFileOf(req.Scope, req.Name)
	if !ok {
		writeError(w, http.StatusNotFound, "no %s persona %q", req.Scope, req.Name)
		return
	}

	// Reference guard over the conventional directories. Root-level
	// documents are not scanned — a documented limit, same as the
	// pickers which prefer the conventional dirs anyway.
	refRe := personaRefRe(req.Name)
	var referencing []string
	for _, dir := range []string{"rooms", "pipelines"} {
		for _, f := range scanYAMLDir(dir) {
			data, err := os.ReadFile(f.Path)
			if err != nil {
				continue
			}
			if refRe.Match(data) {
				referencing = append(referencing, filepath.ToSlash(f.Path))
			}
		}
	}
	if len(referencing) > 0 {
		writeError(w, http.StatusConflict,
			"%q is still referenced by %s — remove the reference(s) first", req.Name, strings.Join(referencing, ", "))
		return
	}
	if err := os.Remove(path); err != nil {
		writeError(w, http.StatusInternalServerError, "removing %s: %v", path, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"removed": path})
}

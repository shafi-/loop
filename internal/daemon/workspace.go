package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// WorkspaceFile is one discoverable YAML in the workspace: its path
// relative to the daemon's directory (exactly what run/host forms
// should submit) and the name declared inside, when present.
type WorkspaceFile struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path"`
}

// handleWorkspace lists candidate pipeline and room files: the
// conventional pipelines/ and rooms/ directories, plus root-level YAML
// classified by its top-level schema (`stages:` → pipeline,
// `agents:` → room). Files that match neither still work — typed
// paths remain the fallback.
func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	pipes := scanYAMLDir("pipelines")
	rooms := scanYAMLDir("rooms")
	rp, rr := classifyRoot(".")
	pipes = append(pipes, rp...)
	rooms = append(rooms, rr...)
	sort.Slice(pipes, func(i, j int) bool { return pipes[i].Path < pipes[j].Path })
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].Path < rooms[j].Path })
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace": s.workspace,
		"pipelines": pipes,
		"rooms":     rooms,
	})
}

var (
	stagesLineRe = regexp.MustCompile(`(?m)^stages:`)
	agentsLineRe = regexp.MustCompile(`(?m)^agents:`)
)

// classifyRoot reads the workspace root's YAML files (shallow,
// dotfiles skipped) and sorts them by schema: a `stages:` block makes
// a pipeline candidate, `agents:` a room candidate. A file can be
// neither; it is never both in practice, and if it were, it would
// appear in both lists — the forms accept it either way.
func classifyRoot(dir string) (pipes, rooms []WorkspaceFile) {
	pipes, rooms = []WorkspaceFile{}, []WorkspaceFile{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return pipes, rooms
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".yml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		f := WorkspaceFile{Name: nameOf(data), Path: filepath.Join(dir, n)}
		if stagesLineRe.Match(data) {
			pipes = append(pipes, f)
		}
		if agentsLineRe.Match(data) {
			rooms = append(rooms, f)
		}
	}
	return pipes, rooms
}

// scanYAMLDir reads one workspace directory shallowly, labeling each
// entry with its declared name (the cheap name-line regex — the id
// and path are the load-bearing data). Empty, not null, when the
// directory doesn't exist: clients render an empty picker, not an
// error.
func scanYAMLDir(dir string) []WorkspaceFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []WorkspaceFile{}
	}
	out := []WorkspaceFile{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".yml") {
			continue
		}
		path := filepath.Join(dir, n)
		out = append(out, WorkspaceFile{Name: snapshotName(path), Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

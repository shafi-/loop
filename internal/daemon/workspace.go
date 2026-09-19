package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/shafi-/loop/internal/config"
)

// WorkspaceFile is one discoverable YAML in the workspace: its path
// relative to the daemon's directory (exactly what run/host forms
// should submit) and the name declared inside, when present.
type WorkspaceFile struct {
	Name string `json:"name,omitempty"`
	Path string `json:"path"`
}

// handleWorkspace lists candidate pipeline and room files: the
// conventional .loop/pipelines/ and .loop/rooms/ directories, plus
// root-level YAML classified by its top-level schema (`stages:` →
// pipeline, `agents:` → room). Files that match neither still work —
// typed paths remain the fallback.
func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	pipes := scanYAMLDir(config.PipelinesDir)
	rooms := scanYAMLDir(config.RoomsDir)
	rp, rr := classifyRoot(".")
	pipes = append(pipes, rp...)
	rooms = append(rooms, rr...)
	sort.Slice(pipes, func(i, j int) bool { return pipes[i].Path < pipes[j].Path })
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].Path < rooms[j].Path })
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace": s.workspace,
		"pipelines": pipes,
		"rooms":     rooms,
		"threads":   scanRoomThreads(rooms),
	})
}

// scanRoomThreads walks .loop/rooms/ and matches each thread to a room
// YAML by declared name, so old conversations stay one click away.
func scanRoomThreads(roomYamls []WorkspaceFile) []RoomThread {
	byName := map[string]string{}
	for _, rf := range roomYamls {
		if data, err := os.ReadFile(rf.Path); err == nil {
			if room, err := config.ParseRoom(data); err == nil && room.Name != "" {
				if _, taken := byName[room.Name]; !taken {
					byName[room.Name] = rf.Path
				}
			}
		}
	}
	entries, err := os.ReadDir(".loop/rooms")
	if err != nil {
		return nil
	}
	var out []RoomThread
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		transcript := filepath.Join(".loop", "rooms", e.Name(), "transcript.jsonl")
		st, err := os.Stat(transcript)
		if err != nil {
			continue // never hosted for real, or rotated away
		}
		out = append(out, RoomThread{
			Name:       e.Name(),
			Lines:      countLines(transcript),
			LastActive: st.ModTime(),
			File:       byName[e.Name()],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActive.After(out[j].LastActive) })
	return out
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

package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestScanYAMLDir lists the conventional workspace directories: yaml
// files only, labeled by their declared name, sorted by path.
func TestScanYAMLDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pipelines/alpha.yaml", "name: alpha\nstages: []\n")
	write("pipelines/zebra.yml", "stages: []\n") // no name line
	write("pipelines/notes.txt", "name: not-a-pipeline\n")
	write("pipelines/sub/nested.yaml", "name: nested\n") // dirs skipped
	write("rooms/ops.yaml", "name: ops\nagents: []\n")
	write("top.yaml", "name: top\n") // outside the conventional dirs

	pipes := scanYAMLDir("pipelines")
	if len(pipes) != 2 {
		t.Fatalf("pipelines = %d entries, want 2: %+v", len(pipes), pipes)
	}
	if pipes[0].Path != filepath.Join("pipelines", "alpha.yaml") || pipes[0].Name != "alpha" {
		t.Errorf("pipes[0] = %+v", pipes[0])
	}
	if pipes[1].Path != filepath.Join("pipelines", "zebra.yml") || pipes[1].Name != "" {
		t.Errorf("pipes[1] = %+v", pipes[1])
	}
	rooms := scanYAMLDir("rooms")
	if len(rooms) != 1 || rooms[0].Name != "ops" {
		t.Errorf("rooms = %+v", rooms)
	}
	// A missing directory is an empty picker, never an error or nil.
	if missing := scanYAMLDir("nowhere"); missing == nil || len(missing) != 0 {
		t.Errorf("missing dir = %#v, want empty non-nil", missing)
	}
}

// Root-level YAML files (a flat workspace) are classified by their
// top-level schema: stages: → pipeline candidate, agents: → room.
func TestClassifyRoot(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("demo.yaml", "name: demo\nstages:\n  - id: a\n")
	write("room.yaml", "name: war-room\nagents:\n  - name: scout\n")
	write("config.yaml", "loglevel: info\n") // neither schema
	write(".hidden.yaml", "stages: []\n")    // dotfiles skipped
	write("notes.txt", "stages: []\n")       // non-yaml skipped

	pipes, rooms := classifyRoot(".")
	if len(pipes) != 1 || pipes[0].Name != "demo" {
		t.Errorf("pipes = %+v", pipes)
	}
	if len(rooms) != 1 || rooms[0].Name != "war-room" {
		t.Errorf("rooms = %+v", rooms)
	}
}

// TestWorkspaceEndpoint serves the picker payload over the API.
func TestWorkspaceEndpoint(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for rel, content := range map[string]string{
		"pipelines/deliver.yaml": "name: deliver\n",
		"rooms/war.yaml":         "name: war\n",
	} {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	srv := New("test", filepath.Join(".loop", "runs"), "loop-fake")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/workspace")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var wk struct {
		Workspace string `json:"workspace"`
		Pipelines []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"pipelines"`
		Rooms []struct {
			Path string `json:"path"`
		} `json:"rooms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wk); err != nil {
		t.Fatal(err)
	}
	if wk.Workspace != filepath.Base(dir) {
		t.Errorf("workspace = %q, want %q", wk.Workspace, filepath.Base(dir))
	}
	if len(wk.Pipelines) != 1 || wk.Pipelines[0].Name != "deliver" {
		t.Errorf("pipelines = %+v", wk.Pipelines)
	}
	if len(wk.Rooms) != 1 || wk.Rooms[0].Path != filepath.Join("rooms", "war.yaml") {
		t.Errorf("rooms = %+v", wk.Rooms)
	}
}

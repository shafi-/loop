package counters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// withEnv points counters at an isolated file and optionally enables
// them for the duration of the test.
func withEnv(t *testing.T, enabled bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "counters.json")
	t.Setenv("LOOP_COUNTERS_FILE", path)
	t.Setenv("LOOP_COUNTERS", "")
	if enabled {
		t.Setenv("LOOP_COUNTERS", "1")
	}
	return path
}

func readFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read counters: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("counters not valid JSON: %v", err)
	}
	return m
}

func TestOffByDefaultWritesNothing(t *testing.T) {
	path := withEnv(t, false)
	Bump("workspaces_initialized")
	BumpKey("pipeline_runs", "feature-delivery")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("disabled counters touched %s", path)
	}
}

func TestEnabledParsing(t *testing.T) {
	for _, v := range []string{"1", "true", "YES", "On", " 1 "} {
		t.Setenv("LOOP_COUNTERS", v)
		if !Enabled() {
			t.Errorf("LOOP_COUNTERS=%q should enable", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "2"} {
		t.Setenv("LOOP_COUNTERS", v)
		if Enabled() {
			t.Errorf("LOOP_COUNTERS=%q should not enable", v)
		}
	}
}

func TestBumpIncrementsAndPersists(t *testing.T) {
	path := withEnv(t, true)
	Bump("workspaces_initialized")
	Bump("workspaces_initialized")
	m := readFile(t, path)
	if got := m["workspaces_initialized"]; got != float64(2) {
		t.Errorf("workspaces_initialized = %v, want 2", got)
	}
}

func TestBumpKeyCountsPerName(t *testing.T) {
	withEnv(t, true)
	BumpKey("pipeline_runs", "feature-delivery")
	BumpKey("pipeline_runs", "feature-delivery")
	BumpKey("pipeline_runs", "agency-delivery")
	BumpKey("pipeline_reruns", "feature-delivery")
	m := Read()
	runs, ok := m["pipeline_runs"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline_runs = %#v, want a map", m["pipeline_runs"])
	}
	if runs["feature-delivery"] != float64(2) || runs["agency-delivery"] != float64(1) {
		t.Errorf("pipeline_runs = %v", runs)
	}
	reruns, ok := m["pipeline_reruns"].(map[string]any)
	if !ok || reruns["feature-delivery"] != float64(1) {
		t.Errorf("pipeline_reruns = %#v", m["pipeline_reruns"])
	}
}

func TestCorruptFileRestartsRatherThanFails(t *testing.T) {
	path := withEnv(t, true)
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	Bump("room_sessions_total") // must not panic or error out loud
	m := readFile(t, path)
	if got := m["room_sessions_total"]; got != float64(1) {
		t.Errorf("after corrupt restart, count = %v, want 1", got)
	}
}

func TestReadSwallowsProblems(t *testing.T) {
	withEnv(t, true)
	// No file yet: Read reports nil, never an error path for callers.
	if m := Read(); m != nil {
		t.Errorf("Read on missing file = %v, want nil", m)
	}
	// A regular file where a directory would be: mkdir and write both fail.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOP_COUNTERS_FILE", filepath.Join(blocker, "c.json"))
	Bump("x") // unwritable path: silently dropped
	if m := Read(); m != nil {
		t.Errorf("Read on unreadable path = %v, want nil", m)
	}
}

func TestPathDefaultsUnderHome(t *testing.T) {
	t.Setenv("LOOP_COUNTERS_FILE", "")
	t.Setenv("HOME", t.TempDir())
	if got := Path(); got != filepath.Join(os.Getenv("HOME"), ".loop", "counters.json") {
		t.Errorf("Path() = %q", got)
	}
}

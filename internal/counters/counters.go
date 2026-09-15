// Package counters maintains opt-in, anonymous, local usage counters.
//
// Off by default: set LOOP_COUNTERS=1 to opt in. What is recorded is
// plain counts — events and per-name tallies (pipeline names, room
// names) — no content, no identifiers, and nothing ever leaves the
// machine. The file lives at ~/.loop/counters.json (override with
// LOOP_COUNTERS_FILE).
//
// Every operation is best-effort: a counters problem (unreadable file,
// unwritable directory) is ignored, never propagated. Counting must
// not be able to break the command it counts for.
package counters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// envEnabled and envFile name the switches; kept as constants so the
// .env.sample and the manual can reference exactly these.
const (
	envEnabled = "LOOP_COUNTERS"
	envFile    = "LOOP_COUNTERS_FILE"
)

// Enabled reports whether the user opted in. Truthy values: 1, true,
// yes, on (case-insensitive); anything else — including unset — is off.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envEnabled))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Path is the counters file: LOOP_COUNTERS_FILE, else ~/.loop/counters.json.
// Empty when no home directory can be determined (operations then no-op).
func Path() string {
	if p := strings.TrimSpace(os.Getenv(envFile)); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".loop", "counters.json")
}

// Bump increments a scalar counter (e.g. workspaces_initialized).
func Bump(event string) {
	bump(event, "")
}

// BumpKey increments a per-name counter (e.g. pipeline_runs keyed by
// pipeline name). Names come from the user's YAML and stay local.
func BumpKey(event, name string) {
	bump(event, name)
}

// Read returns the stored counters (a Snapshot-shaped map) for display.
// Errors are swallowed: reading is informational, never load-bearing.
func Read() map[string]any {
	path := Path()
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return m
}

// bump is the one read-modify-write. Cross-process races can lose an
// increment; that is acceptable for counters and not worth a lock.
func bump(event, name string) {
	if !Enabled() {
		return
	}
	path := Path()
	if path == "" {
		return
	}
	m := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		// A corrupt file restarts the counts rather than failing the command.
		_ = json.Unmarshal(data, &m)
	}
	if name == "" {
		m[event] = count(m[event]) + 1
	} else {
		perName, _ := m[event].(map[string]any)
		if perName == nil {
			perName = map[string]any{}
			m[event] = perName
		}
		perName[name] = count(perName[name]) + 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	// Numbers are written as integers; json.Marshal keeps map keys sorted,
	// so the file is stable across writes.
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(out, '\n'), 0o644)
}

// count coerces a decoded JSON value back to a number; anything
// unexpected counts as zero.
func count(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return 0
}

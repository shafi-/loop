package dotenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadParsesCommonShapes(t *testing.T) {
	path := write(t, `
# comment line
PLAIN=hello
QUOTED="hello world"
SINGLE='hello again'
export EXPORTED=yes
URL=https://api.example.com/v1?key=123#fragment
FLAG=value # trailing comment
EMPTY=
`)
	// The no-override policy makes pre-existing variables win; clear the
	// namespace so assertions are deterministic.
	for _, key := range []string{"PLAIN", "QUOTED", "SINGLE", "EXPORTED", "URL", "FLAG", "EMPTY"} {
		os.Unsetenv(key)
	}

	loaded, warnings, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v", warnings)
	}
	for key, want := range map[string]string{
		"PLAIN":    "hello",
		"QUOTED":   "hello world",
		"SINGLE":   "hello again",
		"EXPORTED": "yes",
		"URL":      "https://api.example.com/v1?key=123#fragment",
		"FLAG":     "value",
		"EMPTY":    "",
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if loaded != 7 {
		t.Errorf("loaded = %d, want 7", loaded)
	}
}

func TestLoadRealEnvironmentWins(t *testing.T) {
	t.Setenv("WINS", "from-shell")
	path := write(t, "WINS=from-file\nOTHER=set\n")
	loaded, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("WINS") != "from-shell" {
		t.Errorf("file overrode the real environment: %q", os.Getenv("WINS"))
	}
	if os.Getenv("OTHER") != "set" {
		t.Errorf("OTHER = %q", os.Getenv("OTHER"))
	}
	if loaded != 1 { // only OTHER; WINS already existed
		t.Errorf("loaded = %d, want 1", loaded)
	}
}

func TestLoadMissingFileIsNoOp(t *testing.T) {
	loaded, warnings, err := Load(filepath.Join(t.TempDir(), "absent.env"))
	if err != nil || loaded != 0 || warnings != nil {
		t.Errorf("missing file: %d, %v, %v", loaded, warnings, err)
	}
}

func TestLoadWarnsOnMalformedLines(t *testing.T) {
	path := write(t, "GOOD=1\nthis line has no equals\n=NOKEY\n")
	_, warnings, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "\n"), ".env:2") {
		t.Errorf("warnings = %v", warnings)
	}
}

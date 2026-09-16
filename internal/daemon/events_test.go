package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	lines := []string{
		`{"ts":"t1","type":"run_started","pipeline":"demo"}`,
		`{"ts":"t2","type":"stage_started","stage":"draft","stage_type":"tool"}`,
		`{"ts":"t3","type":"run_completed","stage":"","steps":2}`,
	}
	content := lines[0] + "\n" + lines[1] + "\n" + lines[2] + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	all, err := readEvents(path, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("readEvents(after=0) = %d events, %v", len(all), err)
	}
	if all[0].Type != "run_started" || all[1].Seq != 2 || all[2].Type != "run_completed" {
		t.Errorf("events = %+v", all)
	}

	rest, err := readEvents(path, 2)
	if err != nil || len(rest) != 1 || rest[0].Seq != 3 {
		t.Fatalf("readEvents(after=2) = %+v, %v", rest, err)
	}

	if n := countEvents(path); n != 3 {
		t.Errorf("countEvents = %d, want 3", n)
	}

	// A missing file is an empty result, not an error — the child may
	// not have created its run dir yet.
	missing := filepath.Join(filepath.Dir(path), "no-such-run", "events.jsonl")
	if evs, err := readEvents(missing, 0); err != nil || evs != nil {
		t.Errorf("missing file: %v, %v", evs, err)
	}
	if n := countEvents(missing); n != 0 {
		t.Errorf("countEvents(missing) = %d", n)
	}
}

// A torn final line (crash mid-write) is skipped, not fatal.
func TestReadEventsToleratesTornLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	content := "{\"type\":\"run_started\"}\n{\"type\":\"stag" // torn write
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	evs, err := readEvents(path, 0)
	if err != nil || len(evs) != 1 || evs[0].Type != "run_started" {
		t.Errorf("torn-line read = %+v, %v", evs, err)
	}
}

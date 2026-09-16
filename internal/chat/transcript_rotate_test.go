package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Rotate archives the old file wholesale and writes the fresh one:
// kept prefix + the note line. The in-memory slice matches the file.
func TestTranscriptRotate(t *testing.T) {
	dir := t.TempDir()
	tr, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i, who := range []string{"user", "scout", "user"} {
		if err := tr.Append(who, strings.Repeat("line ", i+1)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tr.Rotate("transcript-archived.jsonl", 1, "↻ forked here"); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// The fresh transcript: the kept message plus the note.
	if len(tr.Messages) != 2 {
		t.Fatalf("messages after fork = %d, want 2", len(tr.Messages))
	}
	if tr.Messages[0].From != "user" || tr.Messages[1].From != "system" {
		t.Fatalf("unexpected rotation result: %+v", tr.Messages)
	}

	// On disk: the fresh file matches memory; the archive holds all
	// three original lines.
	fresh, err := os.ReadFile(filepath.Join(dir, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(fresh), "\n"); got != 2 {
		t.Errorf("fresh file has %d lines, want 2", got)
	}
	arch, err := os.ReadFile(filepath.Join(dir, "transcript-archived.jsonl"))
	if err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	if got := strings.Count(string(arch), "\n"); got != 3 {
		t.Errorf("archive has %d lines, want 3", got)
	}

	// The rotation survives a reopen — the archive, not the fresh file,
	// carries the original discussion.
	reopened, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.Messages) != 2 || reopened.Messages[1].Text != "↻ forked here" {
		t.Errorf("reopened transcript = %+v", reopened.Messages)
	}
}

// A reset is Rotate with keep=0: the fresh file holds only the note.
// An in-memory transcript (no path) rotates too, without touching disk.
func TestTranscriptRotateResetAndMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	tr, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = tr.Append("user", "hello")
	_ = tr.Append("scout", "hi")
	if err := tr.Rotate("transcript-reset.jsonl", 0, "↻ room reset"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if len(tr.Messages) != 1 || tr.Messages[0].From != "system" {
		t.Fatalf("after reset = %+v", tr.Messages)
	}
	var m Message
	line, err := os.ReadFile(filepath.Join(dir, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(line, &m) != nil || m.Text != "↻ room reset" {
		t.Errorf("reset file = %q", line)
	}

	// Keep larger than the transcript clamps, and an in-memory
	// transcript rotates without a filesystem.
	mem := &Transcript{}
	_ = mem.Append("user", "one")
	if err := mem.Rotate("x.jsonl", 5, "note"); err != nil {
		t.Errorf("in-memory rotate: %v", err)
	}
	if len(mem.Messages) != 2 {
		t.Errorf("in-memory rotate = %+v", mem.Messages)
	}
}

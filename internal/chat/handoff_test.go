package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The hand-off is the room→pipeline context contract: a deterministic
// file (no model in the loop) carrying the explicit task, the room's
// discussion (personas' verdicts included — they are the requirements),
// the plan files, and the digest pointer.
func TestWriteHandoff(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".loop", "rooms", "dev")
	tr, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	say := func(from, text string) {
		t.Helper()
		if err := tr.Append(from, text); err != nil {
			t.Fatal(err)
		}
	}
	say("user", "add rate limiting to the API")
	say("architect", "I suggest a token bucket; plan below.")
	say("user", "keep it under 50 lines and no new dependencies")

	r := &Room{Name: "dev", Transcript: tr, Workspace: ws}

	// Plans: b.md is newer than a.md and must be listed first.
	if err := os.MkdirAll(filepath.Join(ws, ".loop/plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]time.Time{
		"a.md": time.Now().Add(-2 * time.Hour),
		"b.md": time.Now().Add(-1 * time.Hour),
	} {
		p := filepath.Join(ws, ".loop/plans", name)
		if err := os.WriteFile(p, []byte("# "+name), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	path, err := r.WriteHandoff("ship it today")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(".loop", "rooms", "dev", "handoff.md") {
		t.Errorf("hand-off path = %q, want the workspace-relative room path", path)
	}

	data, err := os.ReadFile(filepath.Join(ws, path))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	for _, want := range []string{
		"# room hand-off: dev",
		"## task",
		"ship it today",
		"## the room's discussion (attributed, newest last)",
		"[user] add rate limiting to the API",
		"[architect] I suggest a token bucket; plan below.",
		"[user] keep it under 50 lines and no new dependencies",
		".loop/plans/b.md",
		".loop/plans/a.md",
		"no knowledge digest yet",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("hand-off missing %q:\n%s", want, doc)
		}
	}
	// Newest plan first.
	if i, j := strings.Index(doc, ".loop/plans/b.md"), strings.Index(doc, ".loop/plans/a.md"); i > j {
		t.Errorf("plans out of order (newest first):\n%s", doc)
	}

	// A digest on disk is pointed at, not inlined.
	if err := os.MkdirAll(filepath.Join(ws, ".loop", "knowledge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".loop", "knowledge", "digest.md"), []byte("digest"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.WriteHandoff(""); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(ws, path))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "digest: .loop/knowledge/digest.md") {
		t.Errorf("hand-off does not point at the digest:\n%s", data)
	}
	// Empty task states the fallback instead of leaking an empty line.
	if !strings.Contains(string(data), "(none given with /run") {
		t.Errorf("empty task not explained:\n%s", data)
	}
}

// Long rooms still produce a bounded hand-off: the discussion section
// spends its byte budget on the newest messages, announces dropped
// history, and clips any single oversized message.
func TestWriteHandoffBounded(t *testing.T) {
	ws := t.TempDir()
	tr, err := OpenTranscript(filepath.Join(ws, ".loop", "rooms", "dev"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if err := tr.Append("user", strings.Repeat("ask ", 200)+"!"); err != nil {
			t.Fatal(err)
		}
	}
	r := &Room{Name: "dev", Transcript: tr, Workspace: ws}
	path, err := r.WriteHandoff("")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(ws, path))
	if err != nil {
		t.Fatal(err)
	}
	// The discussion section is byte-capped (8KiB), so the whole file
	// stays well under the 16KiB the engine fold re-caps anyway.
	if info.Size() > 16*1024 {
		t.Errorf("hand-off is %d bytes, want it bounded well under 16KiB", info.Size())
	}
	data, _ := os.ReadFile(filepath.Join(ws, path))
	doc := string(data)
	if !strings.Contains(doc, omittedMarker) {
		t.Errorf("dropped history not announced:\n%s", doc)
	}
	// A clipped message stays one line — sections after it stay intact.
	for _, ln := range strings.Split(doc, "\n") {
		if strings.HasPrefix(ln, "[user] ") && len(ln) > PerMessageClip+len(clippedSuffix) {
			t.Errorf("message line not clipped (%d chars): %.60s…", len(ln), ln)
		}
	}
}

package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The hand-off is the room→pipeline context contract: a deterministic
// file (no model in the loop) carrying the explicit task, the asks as
// stated in the room, the plan files, and the digest pointer.
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
		"- add rate limiting to the API",
		"- keep it under 50 lines and no new dependencies",
		".loop/plans/b.md",
		".loop/plans/a.md",
		"no knowledge digest yet",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("hand-off missing %q:\n%s", want, doc)
		}
	}
	// The conversation section quotes the room's asks, not agent chatter.
	if strings.Contains(doc, "I suggest a token bucket") {
		t.Errorf("hand-off quotes agent chatter:\n%s", doc)
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

// Long rooms still produce a bounded hand-off: only the last asks are
// quoted and each is clipped to one line.
func TestWriteHandoffBounded(t *testing.T) {
	ws := t.TempDir()
	tr, err := OpenTranscript(filepath.Join(ws, ".loop", "rooms", "dev"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxHandoffAsks+5; i++ {
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
	// 15 asks × 400-char clips would be ~6KiB unbounded-with-clip; the
	// byte cap on the section plus 10-ask limit keeps the file small.
	if info.Size() > 16*1024 {
		t.Errorf("hand-off is %d bytes, want it bounded well under 16KiB", info.Size())
	}
	data, _ := os.ReadFile(filepath.Join(ws, path))
	doc := string(data)
	// 10 quoted asks plus the trim marker.
	if got := strings.Count(doc, "\n- "); got > maxHandoffAsks+1 {
		t.Errorf("hand-off quotes %d ask lines, want at most %d plus the trim marker", got, maxHandoffAsks)
	}
	// A clipped ask collapses to one line: bullets stay intact. ("- " +
	// clip + "…" is 2+400+3 bytes at most.)
	for _, ln := range strings.Split(doc, "\n") {
		if strings.HasPrefix(ln, "- ") && len(ln) > maxHandoffQuoteChars+5 {
			t.Errorf("ask line not clipped (%d chars): %.60s…", len(ln), ln)
		}
	}
}

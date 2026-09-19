package chat

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
)

// /compact summarizes the pre-window transcript into one line and
// archives the summarized turns. The model call itself is the
// host-wired Summarizer port — roomkit's test pins its prompting and
// metering; here the port is a stub and the transcript contract is what
// matters.
func TestCompactCommand(t *testing.T) {
	dir := t.TempDir()
	tr, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 30; i++ {
		if err := tr.Append("user", fmt.Sprintf("turn xxxxxxxxxxx %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	r := NewRoom(config.Room{Name: "demo"}, nil, tr)
	r.Settings.HistoryWindow = 5
	var got string
	r.Summarizer = func(_ context.Context, text string) (string, error) {
		got = text
		return "The team agreed on scope; budget question is open.", nil
	}

	if err := r.Say(context.Background(), "/compact", noopUI{}); err != nil {
		t.Fatal(err)
	}

	// The summarizer saw the pre-window turns.
	if !strings.Contains(got, "turn xxxxxxxxxxx 1") {
		t.Errorf("summarizer must receive the pre-window prefix:\n%.400s", got)
	}
	// The transcript now: summary line + the 5 kept turns.
	lines := tr.Window(0)
	if len(lines) != 6 {
		t.Fatalf("post-compact lines = %d, want 6 (summary + window)", len(lines))
	}
	if lines[0].From != "system" || !strings.Contains(lines[0].Text, "session so far: The team agreed on scope") {
		t.Errorf("summary line = %+v", lines[0])
	}
	// The summarized turns archived, never destroyed.
	matches, _ := filepath.Glob(filepath.Join(dir, "transcript-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("archive files = %v, want exactly one", matches)
	}
	// The new context opens with the summary.
	s := tr.BuildConversation(50, 0)
	if !strings.Contains(s, "session so far") {
		t.Errorf("conversation missing the summary line:\n%.300s", s)
	}
}

// A summarizer failure lands as a system line; the transcript stays.
func TestCompactSummarizerFailure(t *testing.T) {
	tr, err := OpenTranscript(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		if err := tr.Append("user", fmt.Sprintf("turn %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	r := NewRoom(config.Room{Name: "demo"}, nil, tr)
	r.Settings.HistoryWindow = 5
	r.Summarizer = func(context.Context, string) (string, error) {
		return "", fmt.Errorf("provider down")
	}
	if err := r.Say(context.Background(), "/compact", noopUI{}); err != nil {
		t.Fatal(err)
	}
	lines := tr.Window(0)
	if !strings.Contains(lines[len(lines)-1].Text, "/compact failed: provider down") {
		t.Errorf("failure line = %+v", lines[len(lines)-1])
	}
	// Nothing was archived: all 10 turns still live.
	if len(lines) != 11 {
		t.Fatalf("lines = %d, want the 10 turns plus the failure line", len(lines))
	}
}

// /compact refuses (as a notice, not an error) when the session still
// fits in the live window.
func TestCompactNothingToDo(t *testing.T) {
	tr, err := OpenTranscript(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Append("user", "only this"); err != nil {
		t.Fatal(err)
	}
	r := NewRoom(config.Room{Name: "demo"}, nil, tr)
	r.Summarizer = func(context.Context, string) (string, error) {
		t.Error("no summarizer call should happen")
		return "", nil
	}
	if err := r.Say(context.Background(), "/compact", noopUI{}); err != nil {
		t.Fatal(err)
	}
	lines := tr.Window(0)
	if len(lines) != 2 || !strings.Contains(lines[1].Text, "nothing to compact") {
		t.Fatalf("lines = %+v", lines)
	}
}

// Without a host-wired summarizer, /compact declines as a system line.
func TestCompactNeedsSummarizer(t *testing.T) {
	tr, err := OpenTranscript(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		if err := tr.Append("user", fmt.Sprintf("turn %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	r := NewRoom(config.Room{Name: "demo"}, nil, tr)
	r.Settings.HistoryWindow = 5
	if err := r.Say(context.Background(), "/compact", noopUI{}); err != nil {
		t.Fatal(err)
	}
	lines := tr.Window(0)
	if !strings.Contains(lines[len(lines)-1].Text, "needs an agent to summarize with") {
		t.Errorf("decline line = %+v", lines[len(lines)-1])
	}
}

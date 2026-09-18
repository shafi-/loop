package chat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
	"github.com/shafi-/loop/internal/usage"
)

// /compact summarizes the pre-window transcript into one line, archives
// the summarized turns, keeps the live window, and meters its own call.
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
	personas := []config.Persona{{Name: "ceo", System: "You are the CEO."}}
	var gotPrompt string
	summarizer := llm.NewMockFunc(func(req llm.Request) *llm.Response {
		gotPrompt = req.Messages[0].Content
		return &llm.Response{Text: "The team agreed on scope; budget question is open.", Usage: llm.Usage{InputTokens: 900, OutputTokens: 60}}
	})
	agents := []*agent.Agent{{Persona: personas[0], Provider: summarizer}}
	meter := usage.NewMeter()
	r := NewRoom(config.Room{Name: "demo", Agents: personas}, agents, tr)
	r.Usage = meter
	r.Settings.HistoryWindow = 5

	if err := r.Say(context.Background(), "/compact", noopUI{}); err != nil {
		t.Fatal(err)
	}

	// The summarizer saw the pre-window turns.
	if !strings.Contains(gotPrompt, "turn xxxxxxxxxxx 1") || !strings.Contains(gotPrompt, "Summarize this session") {
		t.Errorf("summarizer prompt wrong:\n%.400s", gotPrompt)
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
	if _, err := os.Stat(filepath.Join(dir, "transcript-20200101-000000.jsonl")); err == nil {
		t.Log("unexpected fixed archive name")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "transcript-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("archive files = %v, want exactly one", matches)
	}
	// The compact call is metered under its own label.
	entries := meter.Snapshot()
	if len(entries) != 1 || entries[0].Label != "compact" || entries[0].InputTokens != 900 {
		t.Fatalf("metered entries = %+v", entries)
	}
	// The new context opens with the summary.
	s := tr.BuildConversation(50, 0)
	if !strings.Contains(s, "session so far") {
		t.Errorf("conversation missing the summary line:\n%.300s", s)
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
	personas := []config.Persona{{Name: "ceo", System: "x"}}
	summarizer := llm.NewMock(&llm.Response{Text: "should not be called"})
	r := NewRoom(config.Room{Name: "demo", Agents: personas},
		[]*agent.Agent{{Persona: personas[0], Provider: summarizer}}, tr)
	if err := r.Say(context.Background(), "/compact", noopUI{}); err != nil {
		t.Fatal(err)
	}
	lines := tr.Window(0)
	if len(lines) != 2 || !strings.Contains(lines[1].Text, "nothing to compact") {
		t.Fatalf("lines = %+v", lines)
	}
	if summarizer.Calls() != 0 {
		t.Error("no summarizer call should happen")
	}
}

package roomkit

import (
	"context"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
	"github.com/shafi-/loop/internal/usage"
)

type nopUI struct{}

func (nopUI) AgentReplyStart(string)        {}
func (nopUI) AgentTextDelta(string, string) {}
func (nopUI) AgentsSeen([]string)           {}
func (nopUI) AgentCapped(string, int)       {}
func (nopUI) Notice(string, ...any)         {}

// Room assembles the whole recipe: the session ledger, the run-usage
// port, and — with at least one agent — a summarizer.
func TestRoomAssembly(t *testing.T) {
	tr, err := chat.OpenTranscript(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Room{Name: "demo", Agents: []config.Persona{
		{Name: "ceo", System: "s", Model: &config.ModelConfig{Provider: "mock"}},
	}}
	meter := usage.NewMeter()
	r, err := Room(cfg, tr, meter)
	if err != nil {
		t.Fatal(err)
	}
	if r.Usage != meter {
		t.Errorf("session ledger not attached")
	}
	if r.RunUsage == nil {
		t.Errorf("run-usage port not wired")
	}
	if r.Summarizer == nil {
		t.Errorf("summarizer not wired")
	}

	// A room with no agents has no summarizer to ask.
	empty, err := Room(&config.Room{Name: "solo"}, tr, meter)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Summarizer != nil {
		t.Errorf("agentless room must have no summarizer")
	}
}

// The summarizer asks in the room's voice, trims the answer, and meters
// its call under its own label.
func TestSummarizerPromptsAndMeters(t *testing.T) {
	var got llm.Request
	mock := llm.NewMockFunc(func(req llm.Request) *llm.Response {
		got = req
		return &llm.Response{Text: "  scope locked; budget open  ", Usage: llm.Usage{InputTokens: 900, OutputTokens: 60}}
	})
	a := &agent.Agent{Persona: config.Persona{Name: "ceo", System: "s"}, Provider: mock}
	meter := usage.NewMeter()

	summary, err := summarizer(a, meter)(context.Background(), "THE PREFIX")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "  scope locked; budget open  " {
		t.Errorf("summarizer passes the model text through (chat trims): %q", summary)
	}
	if !strings.HasSuffix(got.Messages[0].Content, "THE PREFIX\n\nSummarize this session so far.") {
		t.Errorf("prompt = %.200q", got.Messages[0].Content)
	}
	if !strings.Contains(got.System, "One tight paragraph") {
		t.Errorf("system voice = %.200q", got.System)
	}
	if got.MaxTokens != 700 {
		t.Errorf("max tokens = %d", got.MaxTokens)
	}
	entries := meter.Snapshot()
	if len(entries) != 1 || entries[0].Label != "compact" || entries[0].InputTokens != 900 {
		t.Fatalf("metered entries = %+v", entries)
	}
}

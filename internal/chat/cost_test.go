package chat

import (
	"context"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/agent"
	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
	"github.com/shafi-/loop/internal/usage"
)

// /cost reports the session's ledger as a system line every client
// sees — one Surface, the transcript.
func TestCostCommand(t *testing.T) {
	dir := t.TempDir()
	tr, err := OpenTranscript(dir)
	if err != nil {
		t.Fatal(err)
	}
	meter := usage.NewMeter()
	meter.Record("ceo", llm.Usage{InputTokens: 8412, OutputTokens: 1038})
	meter.Record("cfo", llm.Usage{InputTokens: 1200, OutputTokens: 90})

	personas := []config.Persona{
		{Name: "ceo", System: "You are the CEO."},
		{Name: "cfo", System: "You are the CFO."},
	}
	mocks := map[string]*llm.Mock{
		"ceo": llm.NewMock(&llm.Response{Text: "ok"}),
		"cfo": llm.NewMock(&llm.Response{Text: "ok"}),
	}
	agents := []*agent.Agent{}
	for _, p := range personas {
		agents = append(agents, &agent.Agent{Persona: p, Provider: mocks[p.Name]})
	}
	r := NewRoom(config.Room{Name: "demo", Agents: personas}, agents, tr)
	r.Usage = meter

	if err := r.Say(context.Background(), "/cost", noopUI{}); err != nil {
		t.Fatal(err)
	}
	lines := tr.Window(10)
	last := lines[len(lines)-1]
	if last.From != "system" || !strings.Contains(last.Text, "2 calls · in 9,612 · out 1,128 tokens") {
		t.Fatalf("cost line = %+v", last)
	}
	if !strings.Contains(last.Text, "ceo: 1 calls") || !strings.Contains(last.Text, "cfo: 1 calls") {
		t.Errorf("per-agent attribution missing:\n%s", last.Text)
	}

	// Fresh session: no calls yet — the report says so, not zeroes.
	m2 := usage.NewMeter()
	r2 := NewRoom(config.Room{Name: "demo", Agents: personas}, agents, tr)
	r2.Usage = m2
	if got := r2.CostReport(); !strings.Contains(got, "no model calls recorded") {
		t.Errorf("fresh report = %q", got)
	}
	// Untracked room: honest about it.
	r3 := NewRoom(config.Room{Name: "demo", Agents: personas}, agents, tr)
	if got := r3.CostReport(); !strings.Contains(got, "not tracked") {
		t.Errorf("untracked report = %q", got)
	}
}

type noopUI struct{}

func (noopUI) AgentReplyStart(string)     {}
func (noopUI) AgentTextDelta(_, _ string) {}
func (noopUI) AgentsSeen([]string)        {}
func (noopUI) AgentCapped(string, int)    {}
func (noopUI) Notice(string, ...any)      {}

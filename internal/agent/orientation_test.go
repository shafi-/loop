package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// Tool-using room agents are oriented on the run-record layout: asked
// for a run's status, they must know to read state.json/events.jsonl
// instead of guessing filenames until their tool budget runs out.
func TestReplyWithToolsOrientsOnWorkspace(t *testing.T) {
	m := llm.NewMock(
		&llm.Response{ToolCalls: []llm.ToolCall{{ID: "t1", Name: "read_file", Args: `{"path":"state.json"}`}}},
		&llm.Response{Text: "done"},
	)
	a := &Agent{Persona: config.Persona{Name: "scout", Tools: []string{"read_file"}}, Provider: m}
	if _, err := a.Reply(context.Background(), []config.Persona{a.Persona}, "status?", nil); err != nil {
		t.Fatal(err)
	}
	for _, req := range m.Requests() {
		if !strings.Contains(req.System, ".loop/runs/") || !strings.Contains(req.System, "state.json") {
			t.Errorf("request system prompt missing workspace orientation:\n%.300s", req.System)
		}
	}
	// Non-tool agents keep the lean prompt (they cannot read files).
	plain := &Agent{Persona: config.Persona{Name: "muse"}, Provider: llm.NewMock(&llm.Response{Text: "hi"})}
	if _, err := plain.Reply(context.Background(), []config.Persona{plain.Persona}, "hi", nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.Provider.(*llm.Mock).Requests()[0].System, ".loop/runs/") {
		t.Error("orientation must not leak to tool-less agents")
	}
}

// The speak decision is framed CEO-first: the user's message is an
// invitation to contribute, not noise to be silenced.
func TestDecideSpeakFramesCEO(t *testing.T) {
	m := llm.NewMock(&llm.Response{Text: `{"speak":true,"priority":5,"reason":"asked"}`})
	a := &Agent{Persona: config.Persona{Name: "scout", Role: "scout"}, Provider: m}
	d, err := a.DecideSpeak(context.Background(), []config.Persona{a.Persona}, "earlier talk", "should we pivot?")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Speak {
		t.Errorf("decision = %+v", d)
	}
	req := m.Requests()[0]
	for _, want := range []string{"CEO", "best", "priority"} {
		if !strings.Contains(req.Messages[0].Content, want) {
			t.Errorf("decision prompt missing %q:\n%s", want, req.Messages[0].Content)
		}
	}
	if strings.Contains(req.Messages[0].Content, "Silence is respectable") {
		t.Error("the silence-biased framing must stay dead")
	}
}

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// The workspace brief grounds room agents in the project the room was
// opened in: it rides the system prompt of both reply paths.
func TestWorkspaceBriefReachesReplyPrompts(t *testing.T) {
	const brief = "Project context — you are discussing THIS project.\n- Stack: Go (module webapp)"
	room := []config.Persona{{Name: "scout", System: "You are the scout."}}

	// Plain reply path.
	m := llm.NewMock(&llm.Response{Text: "on it"})
	a := &Agent{Persona: room[0], Provider: m, Workspace: brief}
	if _, err := a.Reply(context.Background(), room, "hi", nil); err != nil {
		t.Fatal(err)
	}
	sys := m.Requests()[0].System
	for _, want := range []string{"You are the scout.", brief, "THIS project"} {
		if !strings.Contains(sys, want) {
			t.Errorf("reply system missing %q:\n%.500s", want, sys)
		}
	}

	// Tool path (first round carries tools, then the text answer).
	mt := llm.NewMock(
		&llm.Response{ToolCalls: []llm.ToolCall{{ID: "t1", Name: "read_file", Args: `{"path":"go.mod"}`}}},
		&llm.Response{Text: "done"},
	)
	at := &Agent{Persona: config.Persona{Name: "scout", Tools: []string{"read_file"}}, Provider: mt, Workspace: brief}
	if _, err := at.Reply(context.Background(), room, "status?", nil); err != nil {
		t.Fatal(err)
	}
	for i, req := range mt.Requests() {
		if !strings.Contains(req.System, "THIS project") {
			t.Errorf("tool-round %d system missing the brief:\n%.400s", i, req.System)
		}
	}

	// No brief set: prompts stay lean — byte-shape identical to before
	// the field existed (just persona + framing + rules).
	m0 := llm.NewMock(&llm.Response{Text: "hi"})
	a0 := &Agent{Persona: room[0], Provider: m0}
	if _, err := a0.Reply(context.Background(), room, "hi", nil); err != nil {
		t.Fatal(err)
	}
	if s := m0.Requests()[0].System; strings.Contains(s, "Project context") {
		t.Errorf("brief leaked without being set:\n%s", s)
	}

	// The speak decision stays brief-free: it is a cheap per-message
	// call that does not even carry the persona's system today.
	md := llm.NewMock(&llm.Response{Text: `{"speak":true,"priority":3,"reason":"asked"}`})
	ad := &Agent{Persona: room[0], Provider: md, Workspace: brief}
	if _, err := ad.DecideSpeak(context.Background(), room, "[user] hello?"); err != nil {
		t.Fatal(err)
	}
	if s := md.Requests()[0].System; strings.Contains(s, "Project context") {
		t.Error("the speak decision must stay brief-free")
	}
}

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// scriptedTools replays tool-call rounds, then a final text answer.
func toolCall(id, name, args string) *llm.Response {
	return &llm.Response{ToolCalls: []llm.ToolCall{{ID: id, Name: name, Args: args}}, StopReason: llm.StopToolUse}
}

func TestReplyWithToolsWritesFileThenAnswers(t *testing.T) {
	dir := t.TempDir()
	m := llm.NewMock(
		toolCall("t1", "write_file", `{"path":"plans/feature-x.md","content":"# Feature X\nthe plan"}`),
		&llm.Response{Text: "Plan persisted to plans/feature-x.md — ready to implement.", StopReason: llm.StopEndTurn},
	)
	a := &Agent{
		Persona:  config.Persona{Name: "architect", Role: "Architect", System: "terse", Tools: []string{"read_file", "write_file"}},
		Provider: m,
		CWD:      dir,
	}
	var notices []string
	a.ToolHook = func(name, detail string) { notices = append(notices, name+": "+detail) }

	var streamed string
	text, err := a.Reply(context.Background(), nil, "draft the plan for feature-X", func(d string) { streamed += d })
	if err != nil {
		t.Fatal(err)
	}
	if text != "Plan persisted to plans/feature-x.md — ready to implement." || streamed != text {
		t.Errorf("text=%q streamed=%q", text, streamed)
	}
	// The file landed inside the workspace, content intact.
	got, err := os.ReadFile(filepath.Join(dir, "plans", "feature-x.md"))
	if err != nil || string(got) != "# Feature X\nthe plan" {
		t.Errorf("file = %q (%v)", got, err)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "wrote plans/feature-x.md") {
		t.Errorf("notices = %v", notices)
	}
	// The round-trip reached the provider: round 2 saw the tool result.
	reqs := m.Requests()
	if len(reqs) != 2 {
		t.Fatalf("rounds = %d, want 2", len(reqs))
	}
	second := reqs[1].Messages
	if len(second) != 3 { // user, assistant(tool_use), tool result
		t.Fatalf("round-2 messages = %+v", second)
	}
	if second[1].ToolCalls[0].Name != "write_file" || second[2].Role != llm.RoleTool || second[2].ToolCallID != "t1" {
		t.Errorf("round-2 history = %+v", second)
	}
	if !strings.Contains(second[2].Content, "wrote plans/feature-x.md") {
		t.Errorf("tool result content = %q", second[2].Content)
	}
	// Tools were offered on every round — read_file and write_file
	// explicitly, plus project_notes auto-granted (read-only knowledge).
	for i, req := range reqs {
		if len(req.Tools) != 3 || req.Tools[2].Name != "project_notes" {
			t.Errorf("round %d tools = %v", i, req.Tools)
		}
	}
}

func TestProjectNotesTool(t *testing.T) {
	// Empty workspace: the tool reports that nothing exists yet, without
	// an error — the model should recover, not crash the turn.
	out, err := execRoomTool(context.Background(), "project_notes", `{"slug":""}`, t.TempDir())
	if err != nil || !strings.Contains(out, "no project notes yet") {
		t.Errorf("empty list = %q (%v)", out, err)
	}
	// Seeded workspace: list shows the index, a slug reads the note.
	dir := t.TempDir()
	idx := `{"version":1,"notes":[{"slug":"engine","title":"Engine","scope":"runs it all","dirs":["internal/engine"]}]}`
	k := filepath.Join(dir, ".loop", "knowledge")
	os.MkdirAll(filepath.Join(k, "notes"), 0o755)
	os.WriteFile(filepath.Join(k, "index.json"), []byte(idx), 0o644)
	os.WriteFile(filepath.Join(k, "notes", "engine.md"), []byte("<!-- header -->\nthe engine note"), 0o644)

	out, err = execRoomTool(context.Background(), "project_notes", `{"slug":""}`, dir)
	if err != nil || !strings.Contains(out, "engine — Engine: runs it all") {
		t.Errorf("list = %q (%v)", out, err)
	}
	out, err = execRoomTool(context.Background(), "project_notes", `{"slug":"engine"}`, dir)
	if err != nil || !strings.Contains(out, "the engine note") || strings.Contains(out, "header") {
		t.Errorf("read = %q (%v)", out, err)
	}
	out, _ = execRoomTool(context.Background(), "project_notes", `{"slug":"ghost"}`, dir)
	if !strings.Contains(out, `no note "ghost"`) {
		t.Errorf("missing slug = %q", out)
	}
}

func TestReplyWithToolsPathGuard(t *testing.T) {
	dir := t.TempDir()
	// Absolute path and `..` escape are refused — the model receives the
	// error as a tool result and can correct course.
	m := llm.NewMock(
		toolCall("t1", "write_file", `{"path":"/etc/loop-escape","content":"x"}`),
		toolCall("t2", "write_file", `{"path":"../escape.md","content":"x"}`),
		&llm.Response{Text: "I could not write outside the workspace.", StopReason: llm.StopEndTurn},
	)
	a := &Agent{
		Persona:  config.Persona{Name: "architect", Role: "Architect", Tools: []string{"write_file"}},
		Provider: m,
		CWD:      dir,
	}
	var notices []string
	a.ToolHook = func(name, detail string) { notices = append(notices, detail) }

	if _, err := a.Reply(context.Background(), nil, "write outside", nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/etc/loop-escape", filepath.Join(filepath.Dir(dir), "escape.md")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("path guard failed: %s exists", p)
		}
	}
	// Both attempts were reported as failures to the room.
	if len(notices) != 2 || !strings.Contains(notices[0], "failed") || !strings.Contains(notices[1], "escapes") {
		t.Errorf("notices = %v", notices)
	}
	// The model saw the errors as tool results.
	res2 := m.Requests()[1].Messages[2].Content
	res3 := m.Requests()[2].Messages[4].Content
	if !strings.Contains(res2, "relative") || !strings.Contains(res3, "escapes") {
		t.Errorf("tool results = %q / %q", res2, res3)
	}
}

func TestReplyWithToolsRoundBudget(t *testing.T) {
	// A model that never stops calling tools must not monopolize the turn.
	loops := make([]*llm.Response, MaxToolRounds+3)
	for i := range loops {
		loops[i] = toolCall("t", "read_file", `{"path":"a.md"}`)
	}
	m := llm.NewMock(loops...)
	a := &Agent{
		Persona:  config.Persona{Name: "looper", Role: "Bot", Tools: []string{"read_file"}},
		Provider: m,
		CWD:      t.TempDir(),
	}
	if err := os.WriteFile(filepath.Join(a.CWD, "a.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := a.Reply(context.Background(), nil, "loop forever", nil)
	if err == nil || !strings.Contains(err.Error(), "tool rounds") {
		t.Fatalf("err = %v, want round-budget exhaustion", err)
	}
	if m.Calls() != MaxToolRounds {
		t.Errorf("rounds = %d, want %d", m.Calls(), MaxToolRounds)
	}
}

func TestReplyWithoutToolsUsesSingleCompletion(t *testing.T) {
	m := llm.NewMock(&llm.Response{Text: "plain answer", StopReason: llm.StopEndTurn})
	a := &Agent{Persona: config.Persona{Name: "solo", Role: "Loner"}, Provider: m}
	if _, err := a.Reply(context.Background(), nil, "hi", nil); err != nil {
		t.Fatal(err)
	}
	if m.Calls() != 1 || len(m.Requests()[0].Tools) != 0 {
		t.Errorf("tool-less personas must stay single-shot: calls=%d tools=%v", m.Calls(), m.Requests()[0].Tools)
	}
}

func TestSafePath(t *testing.T) {
	cases := []struct {
		p    string
		want string // "" = rejected
	}{
		{"plans/x.md", filepath.Join("wd", "plans", "x.md")},
		{"./plans/../x.md", filepath.Join("wd", "x.md")},
		{"", ""},
		{"/abs/x", ""},
		{"../escape", ""},
		{"a/../../escape", ""},
	}
	for _, c := range cases {
		got, err := safePath("wd", c.p)
		if c.want == "" {
			if err == nil {
				t.Errorf("safePath(%q) = %q, want rejection", c.p, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("safePath(%q) = %q (%v), want %q", c.p, got, err, c.want)
		}
	}
}

func TestExecRoomToolRunCommand(t *testing.T) {
	out, err := execRoomTool(context.Background(), "run_command", `{"command":"echo room-tool-ok"}`, t.TempDir())
	if err != nil || !strings.Contains(out, "room-tool-ok") {
		t.Errorf("out=%q err=%v", out, err)
	}
	if _, err := execRoomTool(context.Background(), "run_command", `{"command":"exit 3"}`, ""); err == nil {
		t.Error("failing command must report an error")
	}
	if _, err := execRoomTool(context.Background(), "skynet", `{}`, ""); err == nil {
		t.Error("unknown tool must be refused")
	}
}

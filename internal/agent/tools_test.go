package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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
		toolCall("t1", "write_file", `{"path":".loop/plans/feature-x.md","content":"# Feature X\nthe plan"}`),
		&llm.Response{Text: "Plan persisted to .loop/plans/feature-x.md — ready to implement.", StopReason: llm.StopEndTurn},
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
	if text != "Plan persisted to .loop/plans/feature-x.md — ready to implement." || streamed != text {
		t.Errorf("text=%q streamed=%q", text, streamed)
	}
	// The file landed inside the workspace, content intact.
	got, err := os.ReadFile(filepath.Join(dir, ".loop/plans", "feature-x.md"))
	if err != nil || string(got) != "# Feature X\nthe plan" {
		t.Errorf("file = %q (%v)", got, err)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "wrote .loop/plans/feature-x.md") {
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
	if !strings.Contains(second[2].Content, "wrote .loop/plans/feature-x.md") {
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
		{".loop/plans/x.md", filepath.Join("wd", ".loop/plans", "x.md")},
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

// The room is for planning: the machinery refuses any write outside
// .loop/plans/ — a chat persona cannot mutate the project no matter
// what the model decides; implementation belongs to a pipeline run.
// Writes inside .loop/plans/ (the architect's plan flow) keep working.
func TestRoomWritesConfinedToPlans(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}

	refused, err := execRoomTool(context.Background(), "write_file",
		`{"path":"app.py","content":"hacked"}`, dir)
	if err != nil {
		t.Fatalf("refusal must be a tool result, not an error: %v", err)
	}
	if !strings.Contains(refused, ".loop/plans/") || !strings.Contains(refused, "refused") {
		t.Errorf("refusal does not teach the flow: %q", refused)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "app.py"))
	if string(data) != "original" {
		t.Errorf("the project file was mutated anyway: %q", data)
	}

	for _, outside := range []string{".loop/knowledge/x.md", "notes.md", "sub/dir/file.txt"} {
		out, err := execRoomTool(context.Background(), "write_file",
			`{"path":"`+outside+`","content":"x"}`, dir)
		if err != nil || !strings.Contains(out, "refused") {
			t.Errorf("write to %s must be refused (out=%q err=%v)", outside, out, err)
		}
	}

	wrote, err := execRoomTool(context.Background(), "write_file",
		`{"path":".loop/plans/add-goodbye.md","content":"# plan"}`, dir)
	if err != nil || !strings.Contains(wrote, "wrote") {
		t.Errorf("plan writes must keep working (out=%q err=%v)", wrote, err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, ".loop", "plans", "add-goodbye.md")); string(data) != "# plan" {
		t.Errorf("plan file not written: %q", data)
	}
}

// Rooms run validation commands only. The shell's escape hatches —
// heredocs, interpreter inline scripts, output redirection into files,
// the mutating utilities — are refused deterministically, so the
// write_file guard cannot be walked around with a `python3 - <<PY`
// one-liner. Legitimate validation commands keep working.
func TestRoomCommandGuard(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("print('app')"), 0o644); err != nil {
		t.Fatal(err)
	}

	refused := []string{
		"python3 - <<'PY'\np='x'\ns=open(p).read()\nopen(p,'w').write(s)\nPY", // the observed bypass
		"python3 -c \"open('app.py','w').write('hacked')\"",
		"perl -e 'unlink \"app.py\"'",
		"echo hacked > app.py",
		"cat header >> app.py",
		"sed -i s/app/module/ app.py",
		"tail -f server.log | tee copy.log",
		"rm -rf .",
		"cp secrets.py app.py",
	}
	for _, cmd := range refused {
		out, err := execRoomTool(context.Background(), "run_command", `{"command":`+strconv.Quote(cmd)+`}`, dir)
		if err != nil {
			t.Fatalf("refusal must be a tool result, not an error: %v (cmd %q)", err, cmd)
		}
		if !strings.Contains(out, "refused") || !strings.Contains(out, "pipeline") {
			t.Errorf("command not refused: %q -> %q", cmd, out)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "app.py")); string(data) != "print('app')" {
		t.Errorf("app.py was mutated anyway: %q", data)
	}

	allowed := []string{
		"go test ./internal/cli/ -run TestEnvPaths 2>&1 | tail -2",
		"python3 app.py",
		"grep -c pattern file.txt",
		"curl -s http://localhost:8080/health",
		"git diff --stat",
		"ls -la; wc -l app.py",
	}
	for _, cmd := range allowed {
		out, err := execRoomTool(context.Background(), "run_command", `{"command":`+strconv.Quote(cmd)+`}`, dir)
		if strings.Contains(out, "refused") {
			t.Errorf("validation command wrongly refused: %q -> %q", cmd, out)
		}
		_ = err // the command itself may fail (no go test here); refusal is what we pin
	}
}

// Tool lines are room conversation: a multi-line command renders as one
// short line with a line-count note, clipped when the first line is long.
func TestSummarizeCommand(t *testing.T) {
	heredoc := "python3 - <<'PY'\ns=open(p).read()\nopen(p,'w').write(s)\nPY"
	got := summarizeCommand(heredoc)
	if strings.Contains(got, "\n") || !strings.Contains(got, "(+3 lines)") {
		t.Errorf("multi-line command not summarized to one line: %q", got)
	}
	long := strings.Repeat("x", 200)
	if got := summarizeCommand(long); len(got) > 120 || !strings.HasSuffix(got, "…") {
		t.Errorf("long command not clipped: %d chars", len(got))
	}
	if got := summarizeCommand("go test ./... 2>&1 | tail -2"); got != "go test ./... 2>&1 | tail -2" {
		t.Errorf("short command mangled: %q", got)
	}
}

// Room shell commands are human-in-the-loop: held for the host's
// approver, run only on explicit yes, denied as tool content when not
// (and always denied with no approver attached — a room with no human
// path never runs a shell command).
func TestRunCommandHeldForApproval(t *testing.T) {
	dir := t.TempDir()
	canary := filepath.Join(dir, "canary.txt")

	deny := func(ctx context.Context, tool, detail string) (bool, string, error) {
		return false, "the user said: not that way", nil
	}
	a := &Agent{
		Persona:     config.Persona{Name: "eng", Role: "Engineer", Tools: []string{"run_command"}},
		Provider:    llm.NewMock(&llm.Response{Text: "ok", StopReason: llm.StopEndTurn}),
		CWD:         dir,
		AskApproval: deny,
	}
	// A read-only command, structurally fine, but the user said no.
	tc := llm.ToolCall{ID: "t1", Name: "run_command", Args: `{"command":"cat ` + canary + `"}`}
	out, err := a.execTool(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "denied:") || !strings.Contains(out, "not that way") {
		t.Errorf("denial not delivered to the model as content: %q", out)
	}
	if _, serr := os.Stat(canary); !os.IsNotExist(serr) {
		t.Errorf("a denied command's side effect exists anyway")
	}

	// No approver at all: deterministic denial, no ask, no run.
	bare := &Agent{
		Persona:  config.Persona{Name: "eng", Role: "Engineer", Tools: []string{"run_command"}},
		Provider: llm.NewMock(&llm.Response{Text: "ok", StopReason: llm.StopEndTurn}),
		CWD:      dir,
	}
	out, err = bare.execTool(context.Background(), llm.ToolCall{ID: "t2", Name: "run_command", Args: `{"command":"echo hi"}`})
	if err != nil || !strings.Contains(out, "denied:") || !strings.Contains(out, "no approver") {
		t.Errorf("unapproved room must deny commands: out=%q err=%v", out, err)
	}

	// Explicit yes runs a validation command.
	allow := func(ctx context.Context, tool, detail string) (bool, string, error) { return true, "", nil }
	bare.AskApproval = allow
	out, err = bare.execTool(context.Background(), llm.ToolCall{ID: "t3", Name: "run_command", Args: `{"command":"echo hello"}`})
	if err != nil || strings.TrimSpace(out) != "hello" {
		t.Errorf("approved command did not run: out=%q err=%v", out, err)
	}
}

// toolCallStruct is a tiny helper building a ToolCall from JSON args.
func toolCallStruct(name, args string) llm.ToolCall {
	return llm.ToolCall{ID: "x", Name: name, Args: args}
}

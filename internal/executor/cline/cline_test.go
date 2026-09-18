package cline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shafi-/loop/internal/executor"
)

// fakeHost is a shell script pretending to be the node host: it emits
// canned protocol lines. Testing the Go side needs no Node.
func fakeHost(t *testing.T, script string) *Executor {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "index.mjs")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Executor{NodeBin: "sh", HostPath: path}
}

func TestRunMapsEventsAndDoneOutput(t *testing.T) {
	ex := fakeHost(t, `#!/bin/sh
cat >/dev/null
echo '{"type":"notice","text":"starting"}'
echo '{"type":"text","text":"Hel"}'
echo '{"type":"text","text":"lo"}'
echo '{"type":"tool_call","tool":"read_file","detail":"main.go"}'
echo '{"type":"tool_result","tool":"read_file","detail":"ok"}'
echo '{"type":"done","output":"final answer"}'
`)
	var events []executor.Event
	res, err := ex.Run(context.Background(), executor.Task{
		Instruction: "do the thing",
		Model:       executor.ModelSpec{Provider: "anthropic", Model: "claude-sonnet-4-5"},
	}, func(ev executor.Event) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "final answer" {
		t.Errorf("output = %q", res.Output)
	}
	if len(events) != 5 {
		t.Fatalf("events = %+v", events)
	}
	if events[1].Type != executor.EventText || events[1].Text != "Hel" {
		t.Errorf("event 1 = %+v", events[1])
	}
	if events[3].Type != executor.EventToolCall || events[3].Tool != "read_file" {
		t.Errorf("event 3 = %+v", events[3])
	}
	if events[4].Type != executor.EventToolResult {
		t.Errorf("event 4 = %+v", events[4])
	}
}

func TestRunErrorEventBecomesError(t *testing.T) {
	ex := fakeHost(t, `#!/bin/sh
cat >/dev/null
echo '{"type":"error","message":"provider key rejected"}'
`)
	_, err := ex.Run(context.Background(), executor.Task{}, nil)
	if err == nil || !strings.Contains(err.Error(), "provider key rejected") {
		t.Errorf("err = %v", err)
	}
}

func TestRunExitWithoutDoneReportsStderr(t *testing.T) {
	ex := fakeHost(t, `#!/bin/sh
cat >/dev/null
echo '{"type":"text","text":"partial"}'
echo "Cannot find module '@cline/sdk'" >&2
exit 1
`)
	_, err := ex.Run(context.Background(), executor.Task{}, nil)
	if err == nil || !strings.Contains(err.Error(), "Cannot find module") || !strings.Contains(err.Error(), "exited without completing") {
		t.Errorf("err = %v", err)
	}
}

func TestRunMissingHostFailsWithInstallHint(t *testing.T) {
	ex := &Executor{NodeBin: "definitely-not-a-binary", HostPath: "/nonexistent/index.mjs"}
	_, err := ex.Run(context.Background(), executor.Task{}, nil)
	if err == nil || !strings.Contains(err.Error(), "host missing") || !strings.Contains(err.Error(), "loop executor install cline") {
		t.Errorf("err = %v", err)
	}
}

func TestRunSendsTaskLine(t *testing.T) {
	dir := t.TempDir()
	received := filepath.Join(dir, "got.json")
	ex := fakeHost(t, "#!/bin/sh\ncat > "+received+"\necho '{\"type\":\"done\",\"output\":\"ok\"}'\n")
	_, err := ex.Run(context.Background(), executor.Task{
		Instruction: "the instruction",
		System:      "be terse",
		CWD:         "/tmp",
		Model: executor.ModelSpec{
			Provider: "openai", Model: "llama-3", BaseURL: "http://x/v1", MaxTokens: 99,
		},
		Tools:    []string{"read_file"},
		MaxTurns: 7,
		Approval: "ask",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(received)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		`"type":"task"`, `"instruction":"the instruction"`, `"system":"be terse"`,
		`"provider":"openai"`, `"model":"llama-3"`, `"baseUrl":"http://x/v1"`,
		`"maxTokens":99`, `"maxTurns":7`, `"approval":"ask"`, `"tools":["read_file"]`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("task line missing %s:\n%s", want, s)
		}
	}
}

func TestParseNodeVersion(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"v24.12.0\n", 24},
		{"v22.0.0", 22},
		{"22.3.1", 22},
	} {
		got, err := parseNodeVersion(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseNodeVersion(%q) = %d, %v", tc.in, got, err)
		}
	}
	for _, bad := range []string{"", "node\n", "vx.y"} {
		if _, err := parseNodeVersion(bad); err == nil {
			t.Errorf("parseNodeVersion(%q) should fail", bad)
		}
	}
}

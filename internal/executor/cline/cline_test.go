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
read task
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
read task
echo '{"type":"error","message":"provider key rejected"}'
`)
	_, err := ex.Run(context.Background(), executor.Task{}, nil)
	if err == nil || !strings.Contains(err.Error(), "provider key rejected") {
		t.Errorf("err = %v", err)
	}
}

func TestRunExitWithoutDoneReportsStderr(t *testing.T) {
	ex := fakeHost(t, `#!/bin/sh
read task
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
	ex := fakeHost(t, "#!/bin/sh\nread task\nprintf '%s\\n' \"$task\" > "+received+"\necho '{\"type\":\"done\",\"output\":\"ok\"}'\n")
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

// approvalHost is a fake host exercising the ask loop: it reads the
// task, emits one approval_request, stores the response line it gets
// back verbatim (as the file — embedding it in a done line would break
// the JSON), and finishes.
func approvalHost(t *testing.T, detail string) (*Executor, string) {
	t.Helper()
	dir := t.TempDir()
	got := filepath.Join(dir, "answer.json")
	script := "#!/bin/sh\n" +
		"read task\n" +
		"echo '{\"type\":\"approval_request\",\"requestId\":\"ap1\",\"tool\":\"run_commands\",\"detail\":\"" + detail + "\"}'\n" +
		"read answer\n" +
		"printf '%s' \"$answer\" > " + got + "\n" +
		"echo '{\"type\":\"done\",\"output\":\"ok\"}'\n"
	return fakeHost(t, script), got
}

func readAnswer(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestRunAnswersApprovalRequests proves the ask loop end to end: the
// host's approval_request reaches the task's approver, and the answer —
// approved — rides the response line back to the host.
func TestRunAnswersApprovalRequests(t *testing.T) {
	ex, answer := approvalHost(t, "rm -rf /")
	asked := 0
	_, err := ex.Run(context.Background(), executor.Task{
		Approval: "ask",
		AskApproval: func(ctx context.Context, tool, detail string) (bool, string, error) {
			asked++
			if tool != "run_commands" || detail != "rm -rf /" {
				t.Errorf("approval ask = (%q, %q)", tool, detail)
			}
			return true, "", nil
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Fatalf("approver asked %d times", asked)
	}
	s := readAnswer(t, answer)
	if !strings.Contains(s, `"type":"approval_response"`) ||
		!strings.Contains(s, `"requestId":"ap1"`) ||
		!strings.Contains(s, `"approved":true`) {
		t.Errorf("approval response missing or wrong: %s", s)
	}
}

// The same round-trip, denied: the reason must ride the response line —
// it is what the model sees as the rejection text.
func TestRunAnswersApprovalDenialWithReason(t *testing.T) {
	ex, answer := approvalHost(t, "make")
	_, err := ex.Run(context.Background(), executor.Task{
		Approval: "ask",
		AskApproval: func(ctx context.Context, tool, detail string) (bool, string, error) {
			return false, "we do not run make here", nil
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := readAnswer(t, answer)
	if !strings.Contains(s, `"approved":false`) ||
		!strings.Contains(s, `"reason":"we do not run make here"`) {
		t.Errorf("denial response missing or wrong: %s", s)
	}
}

// No approver attached (a host requesting approval from a task without
// AskApproval) must be denied, not hang.
func TestRunDeniesApprovalWithoutApprover(t *testing.T) {
	ex, answer := approvalHost(t, "whoami")
	_, err := ex.Run(context.Background(), executor.Task{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := readAnswer(t, answer)
	if !strings.Contains(s, `"approved":false`) ||
		!strings.Contains(s, "no approver attached") {
		t.Errorf("nil-approver denial wrong: %s", s)
	}
}

// An approver error denies the call with the error as reason instead of
// failing the run: the model can route around a denied command.
func TestRunApproverErrorDenies(t *testing.T) {
	ex, answer := approvalHost(t, "ls")
	_, err := ex.Run(context.Background(), executor.Task{
		AskApproval: func(ctx context.Context, tool, detail string) (bool, string, error) {
			return false, "", context.DeadlineExceeded
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := readAnswer(t, answer)
	if !strings.Contains(s, `"approved":false`) ||
		!strings.Contains(s, "approval unavailable") {
		t.Errorf("error-denial response wrong: %s", s)
	}
}

// Cumulative usage events map onto EventUsage with their totals intact
// — the engine diffs consecutive events into its meter.
func TestRunMapsUsageEvents(t *testing.T) {
	ex := fakeHost(t, `#!/bin/sh
read task
echo '{"type":"usage","input":100,"output":50}'
echo '{"type":"usage","input":150,"output":80}'
echo '{"type":"done","output":"ok"}'
`)
	var usage []executor.Event
	res, err := ex.Run(context.Background(), executor.Task{}, func(ev executor.Event) {
		if ev.Type == executor.EventUsage {
			usage = append(usage, ev)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "ok" {
		t.Errorf("output = %q", res.Output)
	}
	if len(usage) != 2 {
		t.Fatalf("usage events = %+v", usage)
	}
	if usage[1].InputTokens != 150 || usage[1].OutputTokens != 80 {
		t.Errorf("event 1 = %+v", usage[1])
	}
}

// done's string output and usage's numeric output share the JSON key —
// the hostEvent decoding must keep them distinct (a tagged-field
// conflict silently drops both).
func TestRunDoneOutputStillDecodes(t *testing.T) {
	ex := fakeHost(t, `#!/bin/sh
read task
echo '{"type":"usage","input":7,"output":3}'
echo '{"type":"done","output":"final answer"}'
`)
	res, err := ex.Run(context.Background(), executor.Task{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "final answer" {
		t.Errorf("output = %q", res.Output)
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

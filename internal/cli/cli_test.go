package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// runCLI executes a real command against buffers, with optional piped
// stdin, inside the current working directory. It returns the command's
// captured stdout, stderr, and error — the same things a user would see.
func runCLI(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()

	// Commands read os.Stdin directly (human prompts, chat); swap it for
	// the duration of the call. Restored before assertions run.
	if stdin != "" {
		f := filepath.Join(t.TempDir(), "stdin")
		if err := os.WriteFile(f, []byte(stdin), 0o644); err != nil {
			t.Fatal(err)
		}
		rf, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stdin
		os.Stdin = rf
		t.Cleanup(func() { os.Stdin = old; rf.Close() })
	}

	var out, errBuf bytes.Buffer
	root := NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errBuf.String(), err
}

const cliDemoPipeline = `name: cli-demo
stages:
  - id: draft
    type: tool
    run: echo "draft v1"
  - id: approval
    type: human
    prompt: "Approve ${stages.draft.output}? (yes)"
  - id: ship
    type: tool
    run: echo shipped
`

func TestInitScaffoldsSkipsAndForces(t *testing.T) {
	t.Chdir(t.TempDir())
	// init seeds the global persona library under $HOME — isolate it so
	// the test never touches (or depends on) the real ~/.loop.
	t.Setenv("HOME", t.TempDir())

	out, _, err := runCLI(t, "", "init")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join("pipelines", "feature-delivery.yaml"),
		filepath.Join("pipelines", "implement.yaml"),
		filepath.Join("pipelines", "review.yaml"),
		filepath.Join("rooms", "leadership.yaml"),
		filepath.Join("rooms", "feature.yaml"),
		filepath.Join("rooms", "dev.yaml"),
		"README.md",
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("init did not write %s: %v", path, err)
		}
	}
	if !strings.Contains(out, "Workspace ready (7 file(s))") {
		t.Errorf("out = %q", out)
	}
	// The shipped personas landed in the (isolated) global library.
	home, _ := os.UserHomeDir()
	for _, name := range []string{"architect", "engineer", "reviewer", "product-owner", "cfo", "end-user"} {
		if _, err := os.Stat(filepath.Join(home, ".loop", "personas", name+".yaml")); err != nil {
			t.Errorf("persona %s not seeded: %v", name, err)
		}
	}

	// A second init skips existing files instead of clobbering them.
	_, errOut, err := runCLI(t, "", "init")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "already exists, skipping") || strings.Contains(errOut, "wrote") {
		t.Errorf("second init should skip: %q", errOut)
	}

	// --force overwrites.
	out, _, err = runCLI(t, "", "init", "--force")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Workspace ready (7 file(s))") {
		t.Errorf("forced init out = %q", out)
	}
}

func TestInitCountsWhenOptedIn(t *testing.T) {
	t.Chdir(t.TempDir())
	// Same isolation: counting an init must not seed the real home.
	t.Setenv("HOME", t.TempDir())
	countersFile := filepath.Join(t.TempDir(), "counters.json")
	t.Setenv("LOOP_COUNTERS", "1")
	t.Setenv("LOOP_COUNTERS_FILE", countersFile)

	if _, _, err := runCLI(t, "", "init"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(countersFile)
	if err != nil {
		t.Fatalf("init did not count: %v", err)
	}
	if !strings.Contains(string(data), `"workspaces_initialized": 1`) {
		t.Errorf("counters = %s", data)
	}
}

func TestValidatePassAndFailure(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("good.yaml", []byte(cliDemoPipeline), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, "", "validate", "good.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "✓ good.yaml") {
		t.Errorf("out = %q", out)
	}

	// An unknown field is a strict-decode failure: reported per file,
	// and the command fails without exiting the process.
	bad := strings.Replace(cliDemoPipeline, "  - id: draft\n", "  - id: draft\n    mystery: true\n", 1)
	if err := os.WriteFile("bad.yaml", []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errOut, err := runCLI(t, "", "validate", "bad.yaml")
	if err == nil {
		t.Fatal("invalid pipeline must fail validation")
	}
	if !strings.Contains(errOut, "✗ bad.yaml") {
		t.Errorf("stderr should name the bad file: %q", errOut)
	}
}

func TestRunPauseAndResumeEndToEnd(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("demo.yaml", []byte(cliDemoPipeline), 0o644); err != nil {
		t.Fatal(err)
	}

	// A blank line re-asks; /quit pauses cleanly with exit 0.
	// (The prompt banner itself goes to raw stderr via terminalHuman;
	// the re-ask behavior is pinned by run_human_test.)
	_, errOut, err := runCLI(t, "\n/quit\n", "run", "demo.yaml")
	if err != nil {
		t.Fatalf("pause must be a clean exit, got %v", err)
	}
	if !strings.Contains(errOut, `paused at stage "approval"`) || !strings.Contains(errOut, "resume with: loop run demo.yaml --resume") {
		t.Errorf("pause output = %q", errOut)
	}

	entries, err := os.ReadDir(filepath.Join(".loop", "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("runs = %v (%v)", entries, err)
	}
	runID := entries[0].Name()
	state := readFile(t, filepath.Join(".loop", "runs", runID, "state.json"))
	if !strings.Contains(state, `"paused": "approval"`) {
		t.Errorf("state = %s", state)
	}

	// Resume with an agreeing answer completes the run.
	_, errOut, err = runCLI(t, "yes\n", "run", "demo.yaml", "--resume", runID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "complete") {
		t.Errorf("resume output = %q", errOut)
	}
	state = readFile(t, filepath.Join(".loop", "runs", runID, "state.json"))
	if !strings.Contains(state, `"done": true`) {
		t.Errorf("final state = %s", state)
	}
}

func TestRunFailureReturnsErrorWithResumeHint(t *testing.T) {
	t.Chdir(t.TempDir())
	failing := `name: cli-fail
stages:
  - id: boom
    type: tool
    run: echo "it broke" >&2; exit 1
`
	if err := os.WriteFile("failing.yaml", []byte(failing), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := runCLI(t, "", "run", "failing.yaml")
	if err == nil {
		t.Fatal("failing run must return an error")
	}
	for _, want := range []string{`failed at stage "boom"`, "it broke", "resume with: loop run failing.yaml --resume"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q:\n%v", want, err)
		}
	}
}

func TestChatSessionQuitKeepsTranscript(t *testing.T) {
	t.Chdir(t.TempDir())
	room := `name: cli-room
agents:
  - name: solo
    role: Lone agent
    system: You are terse.
`
	if err := os.WriteFile("room.yaml", []byte(room), 0o644); err != nil {
		t.Fatal(err)
	}
	// Any key lets the env-driven provider construct; none is ever used
	// because the session only quits.
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	out, _, err := runCLI(t, "/quit\n", "chat", "room.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "room \"cli-room\"") || !strings.Contains(out, "session ended") {
		t.Errorf("out = %q", out)
	}
	// The transcript file materializes on the first message; an empty
	// session still leaves its directory (what the farewell names).
	roomsDir := filepath.Join(".loop", "rooms", "cli-room")
	if fi, err := os.Stat(roomsDir); err != nil || !fi.IsDir() {
		t.Errorf("room dir missing: %v", err)
	}
}

func TestDoctorReportsCountersState(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("LOOP_COUNTERS", "")
	out, _, err := runCLI(t, "", "doctor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "checking loop prerequisites") || !strings.Contains(out, "counters: off") {
		t.Errorf("doctor out = %q", out)
	}

	t.Setenv("LOOP_COUNTERS", "1")
	t.Setenv("LOOP_COUNTERS_FILE", filepath.Join(t.TempDir(), "c.json"))
	out, _, err = runCLI(t, "", "doctor")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "counters: on") {
		t.Errorf("doctor out = %q", out)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Compile-time check that the helper stays honest about cobra wiring.
var _ = cobra.NoArgs
var _ io.Writer = (*bytes.Buffer)(nil)

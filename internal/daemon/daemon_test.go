package daemon

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLoop is a shell script standing in for the loop binary: it emits
// our exact stderr shapes and reads one answer from stdin. It writes no
// events.jsonl — the events endpoints are covered by the real-binary
// test and the events_test below.
func fakeLoop(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "fake-loop")
	content := `#!/bin/sh
echo "run fake starting: demo (2 stages)" >&2
echo "── your input needed ──────────────────────" >&2
echo "Approve the draft?" >&2
echo "(/pause · /quit · /exit pause the run — resumable)" >&2
read answer
echo "✓ run fake complete (2 steps)" >&2
echo "answered:$answer" > "` + dir + `/answer.txt"
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// startTestServer runs a Server on a temp unix socket. The socket path
// is kept short (macOS caps sockaddr_un at ~104 chars, and t.TempDir
// embeds the test name).
func startTestServer(t *testing.T, bin, runsDir string) (*Client, string) {
	t.Helper()
	srv := New("test", runsDir, bin)
	base, err := os.MkdirTemp("", "loopd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	path := filepath.Join(base, "d.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx, ln)
	t.Cleanup(func() { cancel() })

	cl, err := Dial(path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return cl, path
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The API round trip with a fake child: submit → gate prompt visible →
// answer delivered to the child's stdin → run retires. This is the
// daemon's reason to exist: a gate asked by a child the server owns is
// answerable through the API.
func TestDaemonGateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	bin := fakeLoop(t, dir)
	cl, _ := startTestServer(t, bin, filepath.Join(dir, "runs"))

	// A quiet daemon answers pings with its version.
	pong, err := cl.Ping()
	if err != nil || pong.Version != "test" {
		t.Fatalf("ping = %v, %v", pong, err)
	}

	sub, err := cl.Submit(bin, "", nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if sub.RunID == "" || sub.Warning != "" {
		t.Fatalf("submit = %+v", sub)
	}

	waitFor(t, 5*time.Second, func() bool {
		info, known, err := cl.Run(sub.RunID)
		return err == nil && known && info.Waiting && strings.Contains(info.Prompt, "Approve the draft?")
	}, "the gate to surface through the API")

	if err := cl.Answer(sub.RunID, "yes"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "answer.txt"))
		return err == nil && strings.Contains(string(data), "answered:yes")
	}, "the child to receive the answer")
	waitFor(t, 5*time.Second, func() bool {
		_, known, _ := cl.Run(sub.RunID)
		return !known // retired once finished
	}, "the finished run to leave the active list")

	// A second submit on a quiet daemon carries no warning.
	sub2, err := cl.Submit(bin, "", nil)
	if err != nil || sub2.Warning != "" {
		t.Fatalf("second submit = %+v, %v", sub2, err)
	}
}

// Submitting while another run is active is allowed (concurrent runs are
// a feature) but warned about: both share the daemon's workspace.
func TestDaemonWarnsOnConcurrentSubmits(t *testing.T) {
	dir := t.TempDir()
	slow := filepath.Join(dir, "slow-loop")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cl, _ := startTestServer(t, slow, filepath.Join(dir, "runs"))

	if _, err := cl.Submit(slow, "", nil); err != nil {
		t.Fatal(err)
	}
	sub2, err := cl.Submit(slow, "", nil)
	if err != nil {
		t.Fatalf("concurrent submit must be allowed: %v", err)
	}
	if !strings.Contains(sub2.Warning, "also active") {
		t.Errorf("warning = %q", sub2.Warning)
	}

	// Halting one of the two active runs works by id and pauses it.
	if err := cl.Halt(sub2.RunID); err != nil {
		t.Fatalf("halt: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		info, known, _ := cl.Run(sub2.RunID)
		return err == nil && known && !info.Alive
	}, "the halted run to exit")
}

func TestDaemonErrors(t *testing.T) {
	dir := t.TempDir()
	cl, _ := startTestServer(t, fakeLoop(t, dir), filepath.Join(dir, "runs"))

	if _, known, err := cl.Run("nope"); known || err != nil {
		t.Errorf("unknown run: known=%v err=%v", known, err)
	}
	if err := cl.Answer("nope", "yes"); err == nil {
		t.Error("answering an unknown run must fail")
	}
	if _, err := cl.Events("nope", 0); err == nil {
		t.Error("events for an unknown run must fail")
	}
	if err := cl.Halt("nope"); err == nil {
		t.Error("halting an unknown run must fail")
	}
	// Answering a run whose gate is not open is a conflict, not a mystery.
	sub, err := cl.Submit(filepath.Join(dir, "slow-409"), "", nil)
	if err == nil {
		slow := filepath.Join(dir, "slow-409")
		_ = os.WriteFile(slow, []byte("#!/bin/sh\nsleep 5\n"), 0o755)
		sub, err = cl.Submit(slow, "", nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Answer(sub.RunID, "yes"); err == nil || !strings.Contains(err.Error(), "approval") {
		t.Errorf("answering a non-waiting run = %v", err)
	}
}

// The full story with the real binary: a keyless gate pipeline runs
// under the daemon, the gate is answered through the API, the rejection
// terminal completes the run, and events.jsonl carries the whole audit
// trail for the polling endpoint. Builds the real binary (skipped with
// -short).
func TestDaemonRealPipelineGateRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real binary (skipped with -short)")
	}
	bin := daemonTestBinary(t)
	dir := t.TempDir()
	t.Chdir(dir) // children inherit the daemon's cwd: keep runs dir consistent
	if err := os.WriteFile("demo.yaml", []byte(`name: demo
stages:
  - id: draft
    type: tool
    run: echo "plan v1"
  - id: approval
    type: human
    gate: true
    prompt: "Approve ${stages.draft.output}?"
  - id: route
    type: router
    when:
      - if: "${stages.approval.intent} == 'yes'"
        next: ship
      - next: rejected
  - id: rejected
    type: tool
    terminal: true
    run: echo rejected
  - id: ship
    type: tool
    terminal: true
    run: echo shipped
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cl, _ := startTestServer(t, bin, filepath.Join(dir, ".loop", "runs"))
	abs, _ := filepath.Abs("demo.yaml")
	sub, err := cl.Submit(abs, "", nil)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	waitFor(t, 15*time.Second, func() bool {
		info, known, _ := cl.Run(sub.RunID)
		return err == nil && known && info.Waiting
	}, "the gate to surface")

	// "no" is the deterministic vocabulary — keyless by design (free
	// words would invoke the gate classifier and need a provider).
	if err := cl.Answer(sub.RunID, "no"); err != nil {
		t.Fatalf("answer: %v", err)
	}
	waitFor(t, 15*time.Second, func() bool {
		info, _, _ := cl.Run(sub.RunID)
		return !info.Alive && info.LastLine == "phase: done"
	}, "the run to complete (rejection terminal)")

	// The polling endpoint serves the whole audit trail, typed.
	events, err := cl.Events(sub.RunID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev.Type)
	}
	joined := strings.Join(kinds, ",")
	for _, want := range []string{"run_started", "stage_started", "human_prompt", "human_answer", "human_intent", "router_decision", "run_completed"} {
		if !strings.Contains(joined, want) {
			t.Errorf("events missing %q:\n%s", want, joined)
		}
	}

	// Incremental reads: after the first event, everything but it.
	rest, err := cl.Events(sub.RunID, 1)
	if err != nil || len(rest) != len(events)-1 || rest[0].Seq != 2 {
		t.Errorf("incremental events = %d (want %d), first seq %d (want 2), err %v", len(rest), len(events)-1, rest[0].Seq, err)
	}
}

var (
	daemonBinOnce sync.Once
	daemonBin     string
	daemonBinErr  error
)

func daemonTestBinary(t *testing.T) string {
	t.Helper()
	daemonBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "loop-daemon-e2e-")
		if err != nil {
			daemonBinErr = err
			return
		}
		daemonBin = filepath.Join(dir, "loop")
		goBin := os.Getenv("GO_BIN")
		if goBin == "" {
			goBin = "go"
		}
		cmd := exec.Command(goBin, "build", "-o", daemonBin, "./cmd/loop")
		cmd.Dir = "../.."
		out, err := cmd.CombinedOutput()
		if err != nil {
			daemonBinErr = fmt.Errorf("building loop: %v: %s", err, out)
		}
	})
	if daemonBinErr != nil {
		t.Fatal(daemonBinErr)
	}
	return daemonBin
}

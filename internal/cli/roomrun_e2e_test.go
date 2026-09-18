package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// buildLoopOnce builds the real binary for end-to-end tests (the room
// session spawns it as the `loop run` child). Skipped under -short.
var (
	builtBinOnce sync.Once
	builtBin     string
	builtBinErr  error
)

func loopBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end test: builds the real binary (skipped with -short)")
	}
	builtBinOnce.Do(func() {
		// A package-level temp dir, not t.TempDir(): the binary must
		// outlive the first test that built it. The OS reaps /tmp.
		dir, err := os.MkdirTemp("", "loop-e2e-")
		if err != nil {
			builtBinErr = err
			return
		}
		builtBin = filepath.Join(dir, "loop")
		goBin := os.Getenv("GO_BIN")
		if goBin == "" {
			goBin = "go"
		}
		cmd := exec.Command(goBin, "build", "-o", builtBin, "./cmd/loop")
		cmd.Dir = "../.."
		out, err := cmd.CombinedOutput()
		if err != nil {
			builtBinErr = fmt.Errorf("building loop: %v: %s", err, out)
		}
	})
	if builtBinErr != nil {
		t.Fatal(builtBinErr)
	}
	return builtBin
}

// roomSession drives `loop chat` over real pipes: lines are written to
// its stdin, its combined output is captured and polled for markers.
type roomSession struct {
	t        *testing.T
	stdinW   *os.File
	mu       sync.Mutex
	captured []string
	done     chan error
}

func startRoomSession(t *testing.T, args ...string) *roomSession {
	t.Helper()
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = stdinR

	root := NewRootCmd()
	root.SetOut(outW)
	root.SetErr(outW)
	root.SetArgs(args)

	s := &roomSession{t: t, stdinW: stdinW, done: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			s.mu.Lock()
			s.captured = append(s.captured, sc.Text())
			s.mu.Unlock()
		}
	}()
	go func() {
		s.done <- root.Execute()
		outW.Close()
	}()
	t.Cleanup(func() {
		os.Stdin = oldStdin
		stdinR.Close()
		stdinW.Close()
	})
	return s
}

func (s *roomSession) send(line string) {
	s.t.Helper()
	if _, err := s.stdinW.WriteString(line + "\n"); err != nil {
		s.t.Fatalf("sending %q: %v", line, err)
	}
}

// await polls the captured output until a line containing marker shows
// up (or the deadline passes).
func (s *roomSession) await(marker string) {
	s.t.Helper()
	s.awaitCount(marker, 1)
}

// awaitCount waits until the marker has appeared at least n times —
// second rounds of the same flow must not match first-round lines.
func (s *roomSession) awaitCount(marker string, n int) {
	s.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		hits := 0
		s.mu.Lock()
		for _, line := range s.captured {
			if strings.Contains(line, marker) {
				hits++
			}
		}
		s.mu.Unlock()
		if hits >= n {
			return
		}
		select {
		case err := <-s.done:
			s.t.Fatalf("chat session ended before %q x%d (err=%v); captured:\n%s", marker, n, err, strings.Join(s.captured, "\n"))
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.t.Fatalf("timed out waiting for output containing %q x%d; captured:\n%s", marker, n, strings.Join(s.captured, "\n"))
}

const e2eGatePipeline = `name: demo
stages:
  - id: draft
    type: tool
    run: echo "plan v1"
  - id: approval
    type: human
    gate: true
    prompt: "Approve ${stages.draft.output}? yes, no, or describe changes"
  - id: route
    type: router
    when:
      - if: "${stages.approval.intent} == 'yes'"
        next: ship
      - if: "${stages.approval.intent} == 'no'"
        next: rejected
      - next: revise
  - id: revise
    type: llm
    prompt: "Apply: ${stages.approval.answer}"
  - id: back
    type: router
    when:
      - next: approval
  - id: rejected
    type: tool
    terminal: true
    run: echo "rejected — nothing shipped"
  - id: ship
    type: tool
    terminal: true
    input: ${stages.draft.output}
    run: mkdir -p deliveries && cat > deliveries/plan.md
`

func TestRoomRunsOwnedPipelineEndToEnd(t *testing.T) {
	bin := loopBinary(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("ANTHROPIC_API_KEY", "test-key") // provider construction; no call ever happens
	oldPath := executablePath
	executablePath = func() (string, error) { return bin, nil }
	t.Cleanup(func() { executablePath = oldPath })

	write := func(name, content string) {
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("demo.yaml", e2eGatePipeline)
	write("room.yaml", `name: e2e
pipelines:
  - name: demo
    file: demo.yaml
agents:
  - name: solo
    role: Lone agent
    system: terse.
`)

	s := startRoomSession(t, "chat", "room.yaml")
	s.send("/run demo")
	s.await("needs your approval")
	s.await("Approve plan v1?")
	s.send("/approve yes")
	s.await("completed (run")
	s.send("/status")
	s.await("no active runs")
	// The gate approved once; run it again and say no — the rejection
	// terminal completes the run without shipping.
	s.send("/run demo")
	s.awaitCount("needs your approval", 2)
	s.send("/approve no")
	s.await("→ rejected (tool)")
	s.awaitCount("completed (run", 2)
	s.send("/status")
	s.awaitCount("no active runs", 2)
	s.send("/quit")
	s.await("session ended")

	// The gate approved once: the ship terminal wrote the artifact. The
	// rejection must not have overwritten it (or written anything else).
	plan, err := os.ReadFile(filepath.Join(dir, "deliveries", "plan.md"))
	if err != nil || strings.TrimSpace(string(plan)) != "plan v1" {
		t.Errorf("deliveries/plan.md = %q (%v)", plan, err)
	}
	// The transcript holds the room-side audit of the exchange.
	tr, err := os.ReadFile(filepath.Join(dir, ".loop", "rooms", "e2e", "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tr), "[approval needed]") {
		t.Errorf("transcript missing the gate marker:\n%s", tr)
	}
}

func TestRoomRunsTwoPipelinesConcurrently(t *testing.T) {
	bin := loopBinary(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	oldPath := executablePath
	executablePath = func() (string, error) { return bin, nil }
	t.Cleanup(func() { executablePath = oldPath })

	write := func(name, content string) {
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("alpha.yaml", "name: alpha\nstages:\n  - id: work\n    type: tool\n    terminal: true\n    run: echo alpha-done > alpha.out\n")
	write("beta.yaml", "name: beta\nstages:\n  - id: work\n    type: tool\n    terminal: true\n    run: echo beta-done > beta.out\n")
	write("room.yaml", `name: e2e2
pipelines:
  - name: alpha
    file: alpha.yaml
  - name: beta
    file: beta.yaml
agents:
  - name: solo
    role: Lone agent
    system: terse.
`)

	s := startRoomSession(t, "chat", "room.yaml")
	s.send("/run alpha")
	s.send("/run beta") // different alias: starts concurrently
	s.await("alpha completed (run")
	s.await("beta completed (run")
	s.send("/quit")
	s.await("session ended")

	for name, want := range map[string]string{"alpha.out": "alpha-done", "beta.out": "beta-done"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || strings.TrimSpace(string(data)) != want {
			t.Errorf("%s = %q (%v)", name, data, err)
		}
	}
}

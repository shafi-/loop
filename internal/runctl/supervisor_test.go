package runctl

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeChild is a shell script standing in for the loop binary: it emits
// our exact stderr shapes, reads one answer from stdin, and finishes.
func writeFakeChild(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "fake-loop")
	content := `#!/bin/sh
echo "run fake starting: demo (2 stages)" >&2
echo "→ draft (tool)" >&2
echo "── your input needed ──────────────────────" >&2
echo "Approve the draft?" >&2
echo "(/pause · /quit · /exit pause the run — resumable)" >&2
read answer
echo "→ ship (tool)" >&2
echo "✓ run fake complete (2 steps)" >&2
echo "answered:$answer" > "` + dir + `/answer.txt"
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// newTestSupervisor builds a supervisor whose only handler records
// notices. No chat, no transcript: the core runs bare.
func newTestSupervisor(t *testing.T, bin string) (*Supervisor, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var notices []string
	sup := NewSupervisor(bin, Handlers{
		OnNotice: func(d *Driver, text string) {
			mu.Lock()
			defer mu.Unlock()
			notices = append(notices, fmt.Sprintf("%s %s", d.Alias, text))
		},
	})
	return sup, &notices
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

func TestSupervisorApproveRoutesToChildStdin(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeChild(t, dir)
	sup, _ := newTestSupervisor(t, bin)

	if _, err := sup.Start(Spec{Alias: "demo", File: bin}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		alias, ok := sup.WaitingGate()
		return ok && alias == "demo"
	}, "the gate to start asking")

	if err := sup.Approve("", "yes"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "answer.txt"))
		return err == nil && strings.Contains(string(data), "answered:yes")
	}, "the child to receive the answer")

	sup.Shutdown()
}

func TestSupervisorRefusesSecondRunPerAlias(t *testing.T) {
	dir := t.TempDir()
	// A child that waits, so the alias stays busy.
	script := filepath.Join(dir, "slow-loop")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sup, _ := newTestSupervisor(t, script)
	if _, err := sup.Start(Spec{Alias: "demo", File: script}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Start(Spec{Alias: "demo", File: script}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("second run on a busy alias must be refused: %v", err)
	}
	sup.Shutdown()
}

func TestSupervisorRunInfoTracksPhase(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeChild(t, dir)
	sup, _ := newTestSupervisor(t, bin)

	if _, err := sup.Start(Spec{Alias: "demo", File: bin}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		runs := sup.Runs()
		return len(runs) == 1 && runs[0].Waiting
	}, "Runs to report the waiting gate")
	if runs := sup.Runs(); runs[0].RunID == "" || runs[0].Alias != "demo" || runs[0].Prompt == "" {
		t.Errorf("Runs() = %+v", runs)
	}

	if err := sup.Approve("", "yes"); err != nil {
		t.Fatal(err)
	}
	// After completion the run is retired from the map.
	waitFor(t, 5*time.Second, func() bool {
		return len(sup.Runs()) == 0
	}, "the finished run to be retired")

	sup.Shutdown()
}

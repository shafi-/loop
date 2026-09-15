package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shafi-/loop/internal/chat"
	"github.com/shafi-/loop/internal/config"
)

// collectTranslator records everything the translator emits for a feed.
type collectTranslator struct {
	runStatusTranslator
	notices  []string
	gates    []string
	endings  [][2]string // kind, headline
}

func newCollector() *collectTranslator {
	c := &collectTranslator{}
	c.Notice = func(t string) { c.notices = append(c.notices, t) }
	c.Gate = func(p string) { c.gates = append(c.gates, p) }
	c.Ended = func(kind, headline string) { c.endings = append(c.endings, [2]string{kind, headline}) }
	return c
}

// The translator is the room's read of a child's stderr: progress lines
// pass through, gate banners reassemble the full question, terminal
// lines classify the ending, and filler never leaks.
func TestRunStatusTranslator(t *testing.T) {
	c := newCollector()
	for _, line := range []string{
		"run 2026 starting: demo (3 stages)",
		"→ draft (tool)",
		"→ approval (human)",
		"",
		"── your input needed ──────────────────────",
		"Review the plan below. Reply yes to approve,",
		"no to reject, or describe changes.",
		"",
		"PLAN v1 — the whole document",
		"(/pause · /quit · /exit pause the run — resumable)",
		"> ",
	} {
		c.Feed(line)
	}
	if len(c.gates) != 1 {
		t.Fatalf("gates = %v", c.gates)
	}
	want := "Review the plan below. Reply yes to approve,\nno to reject, or describe changes.\n\nPLAN v1 — the whole document"
	if c.gates[0] != want {
		t.Errorf("gate prompt = %q\nwant %q", c.gates[0], want)
	}
	if !c.Waiting() {
		t.Error("gate must be waiting after the hint marker")
	}
	for _, line := range []string{
		"→ route (router)",
		"> → ship (tool)", // the child's dangling prompt marker, glued
		"⤷ route routed to ship",
		"  resume with: loop run demo.yaml --resume 2026",
		"✓ run 2026 complete (4 steps)",
	} {
		c.Feed(line)
	}
	// Progress after the answer resolves the wait.
	if c.Waiting() {
		t.Error("progress after a gate clears the waiting state")
	}
	// Filler, the glued prompt marker, and the child's shell-form resume
	// hint never surface.
	joined := strings.Join(c.notices, "\n")
	if strings.Contains(joined, ">") || strings.Contains(joined, "resume with:") {
		t.Errorf("notices carry filler:\n%s", joined)
	}
	if !strings.Contains(joined, "→ ship (tool)") {
		t.Errorf("the glued line must survive with its marker stripped:\n%s", joined)
	}
	if len(c.endings) != 1 || c.endings[0][0] != "completed" {
		t.Errorf("endings = %v", c.endings)
	}
}

func TestRunStatusTranslatorEndings(t *testing.T) {
	for _, tc := range [][2]string{
		{"paused", "⏸ run 2026 paused at stage \"approval\""},
		{"failed", "✗ run 2026 failed at stage \"plan\": boom"},
	} {
		c := newCollector()
		c.Feed(tc[1])
		if len(c.endings) != 1 || c.endings[0][0] != tc[0] {
			t.Errorf("Feed(%q) endings = %v, want %s", tc[1], c.endings, tc[0])
		}
	}
}

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

func newTestSession(t *testing.T, bin string) (*roomRunSession, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var notices []string
	ui := &recordingUI{mu: &mu, notices: &notices}
	tr, err := chat.OpenTranscript(filepath.Join(t.TempDir(), "room"))
	if err != nil {
		t.Fatal(err)
	}
	pipelines := []config.RoomPipeline{{Name: "demo", File: "demo.yaml"}}
	// resolveFile joins the room dir; point it at the fake's dir.
	sess := newRoomRunSession(bin, filepath.Join("nowhere", "room.yaml"), pipelines, ui, io_discard(), tr)
	return sess, &notices
}

type recordingUI struct {
	mu       *sync.Mutex
	notices  *[]string
}

func (u *recordingUI) AgentReplyStart(string)          {}
func (u *recordingUI) AgentTextDelta(_, _ string)      {}
func (u *recordingUI) AgentsSeen([]string)             {}
func (u *recordingUI) AgentCapped(string, int)         {}
func (u *recordingUI) Notice(format string, a ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	*u.notices = append(*u.notices, fmt.Sprintf(format, a...))
}

func io_discard() *strings.Builder { return &strings.Builder{} }

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

func TestRoomRunApproveRoutesToChildStdin(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeChild(t, dir)
	sess, _ := newTestSession(t, bin)

	if err := sess.Start("demo", "", nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		alias, ok := sess.WaitingGate()
		return ok && alias == "demo"
	}, "the gate to start asking")

	if err := sess.Approve("", "yes"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(filepath.Join(dir, "answer.txt"))
		return err == nil && strings.Contains(string(data), "answered:yes")
	}, "the child to receive the answer")

	sess.Shutdown()
}

func TestRoomRunRefusesSecondRunPerAlias(t *testing.T) {
	dir := t.TempDir()
	// A child that waits, so the alias stays busy.
	script := filepath.Join(dir, "slow-loop")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sess, _ := newTestSession(t, script)
	if err := sess.Start("demo", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := sess.Start("demo", "", nil); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("second run on a busy alias must be refused: %v", err)
	}
	sess.Shutdown()
}

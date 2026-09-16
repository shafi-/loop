package runctl

import (
	"strings"
	"testing"
)

// collector records everything the translator emits for a feed.
type collector struct {
	Translator
	notices []string
	gates   []string
	endings [][2]string // kind, headline
}

func newCollector() *collector {
	c := &collector{}
	c.Notice = func(t string) { c.notices = append(c.notices, t) }
	c.Gate = func(p string) { c.gates = append(c.gates, p) }
	c.Ended = func(kind, headline string) { c.endings = append(c.endings, [2]string{kind, headline}) }
	return c
}

// The translator is the host's read of a child's stderr: progress lines
// pass through, gate banners reassemble the full question, terminal
// lines classify the ending, and filler never leaks.
func TestTranslator(t *testing.T) {
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

func TestTranslatorEndings(t *testing.T) {
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

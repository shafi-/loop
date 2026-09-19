package chat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/shafi-/loop/internal/usage"
)

// usageFile is the room's lifetime spend of past sessions, one JSON
// usage.Total, next to the transcript.
func usageFile(dir string) string { return filepath.Join(dir, "usage.json") }

func loadUsageBase(dir string) usage.Total {
	if dir == "" {
		return usage.Total{}
	}
	data, err := os.ReadFile(usageFile(dir))
	if err != nil {
		return usage.Total{} // first session, or unreadable: start at zero
	}
	var t usage.Total
	if json.Unmarshal(data, &t) != nil {
		return usage.Total{} // corrupt: a ledger must never break the room
	}
	return t
}

// FlushUsage persists the room's lifetime spend (past sessions + this
// one so far) as usage.json next to the transcript, atomically. Called
// at the end of every turn; hosts also call it after out-of-turn spend
// (the knowledge layer). Best-effort: a flush failure never fails a turn.

// FlushUsage persists the room's lifetime spend (past sessions + this
// one so far) as usage.json next to the transcript, atomically. Called
// at the end of every turn; hosts also call it after out-of-turn spend
// (the knowledge layer). Best-effort: a flush failure never fails a turn.
func (r *Room) FlushUsage() {
	if r.Usage == nil {
		return
	}
	dir := r.Transcript.Dir()
	if dir == "" {
		return // in-memory transcript (tests): nothing to persist
	}
	data, err := json.Marshal(r.usageBase.Add(r.Usage.Totals()))
	if err != nil {
		return
	}
	tmp := usageFile(dir) + ".tmp"
	if os.WriteFile(tmp, data, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, usageFile(dir))
}

// UsageAll is the room's lifetime conversation spend: every past
// session plus this one so far.

// UsageAll is the room's lifetime conversation spend: every past
// session plus this one so far.
func (r *Room) UsageAll() usage.Total {
	if r.Usage == nil {
		return r.usageBase
	}
	return r.usageBase.Add(r.Usage.Totals())
}

// SetKnowledge attaches the knowledge layer; a room whose settings say
// knowledge: false declines it. Hosts call this once, after NewRoom.

// CostReport renders what this room has spent, at three depths: this
// session (with per-agent attribution), every session since the room
// began, and the pipeline runs it commanded (their spend happens in
// child `loop run` processes, so it is read back from their run
// records — any session, current and archived transcripts alike).
func (r *Room) CostReport() string {
	if r.Usage == nil {
		return "◈ usage: not tracked for this session"
	}
	tot := r.Usage.Totals()
	var b strings.Builder
	if tot.Calls == 0 {
		b.WriteString("◈ session usage: no model calls recorded yet")
	} else {
		fmt.Fprintf(&b, "◈ session usage — %s", tot.FormatTotal())
		if entries := r.Usage.Snapshot(); len(entries) > 1 {
			var parts []string
			for _, e := range entries {
				parts = append(parts, fmt.Sprintf("%s: %d calls · in %s · out %s",
					e.Label, e.Calls, usage.Human(e.InputTokens), usage.Human(e.OutputTokens)))
			}
			b.WriteString("\n" + strings.Join(parts, "\n"))
		}
	}
	if r.usageBase.Calls > 0 {
		fmt.Fprintf(&b, "\n◈ all sessions — %s (this room's conversations, since the beginning)",
			r.UsageAll().FormatTotal())
	}
	if pu, runs := r.pipelineUsage(); runs > 0 {
		fmt.Fprintf(&b, "\n◈ pipelines — %s (%d run%s commanded from this room; spend lives in their run records)",
			pu.FormatTotal(), runs, plural(runs))
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// roomRunIDRe matches loop's generated run ids as they appear in
// transcript text (progress lines, completion lines, resume hints) —
// the record of which runs this room commanded.

// roomRunIDRe matches loop's generated run ids as they appear in
// transcript text (progress lines, completion lines, resume hints) —
// the record of which runs this room commanded.
var roomRunIDRe = regexp.MustCompile(`\b\d{8}-\d{6}-[0-9a-f]{4}\b`)

// pipelineUsage sums the spend of every run this room's transcripts
// mention — current and archived — from .loop/runs/<id>/state.json.

// pipelineUsage sums the spend of every run this room's transcripts
// mention — current and archived — from .loop/runs/<id>/state.json.
func (r *Room) pipelineUsage() (usage.Total, int) {
	if r.Workspace == "" {
		return usage.Total{}, 0
	}
	seen := map[string]bool{}
	collect := func(text string) {
		for _, id := range roomRunIDRe.FindAllString(text, -1) {
			seen[id] = true
		}
	}
	for _, m := range r.Transcript.Window(0) {
		collect(m.Text)
	}
	if dir := r.Transcript.Dir(); dir != "" {
		if archives, err := filepath.Glob(filepath.Join(dir, "transcript-*.jsonl")); err == nil {
			for _, path := range archives {
				if data, err := os.ReadFile(path); err == nil {
					collect(string(data))
				}
			}
		}
	}
	runsDir := filepath.Join(r.Workspace, ".loop", "runs")
	if r.RunUsage == nil {
		return usage.Total{}, 0
	}
	var tot usage.Total
	n := 0
	for id := range seen {
		if u, ok := r.RunUsage(runsDir, id); ok {
			tot = tot.Add(u)
			n++
		}
	}
	return tot, n
}

// rotationCommands seeds the engine's built-in room verbs.

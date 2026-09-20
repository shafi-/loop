package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/shafi-/loop/internal/config"
)

// The room→pipeline hand-off. Starting a pipeline from a room must hand
// the run the room's context: the task as stated, the room's discussion
// (verbatim — no model in the loop), the plan files on disk, and where
// the knowledge digest lives. WriteHandoff renders that brief
// deterministically into the room's record dir; hosts pass the returned
// path to the run as the `room_context` var — and the engine folds the
// brief into the first model-facing stage's input, so the run sees the
// room even when its pipeline never reads the var. A pipeline started
// outside a room simply has no such var — stages guard for that.

const (
	// maxHandoffDiscussionBytes bounds the discussion section: the room's
	// recent conversation is the requirements context; older history
	// stays in the transcript.
	maxHandoffDiscussionBytes = 8 * 1024
	// maxHandoffQuoteChars clips one quoted task line.
	maxHandoffQuoteChars = 400
)

// WriteHandoff writes the room's task brief to <room dir>/handoff.md
// (next to the transcript — it is the room's record of what it asked
// for) and returns the workspace-relative path, for the `room_context`
// var. The newest hand-off wins: each /run overwrites it. task is the
// explicit task the user gave with --var task=… ("" when none).
func (r *Room) WriteHandoff(task string) (string, error) {
	dir := r.Transcript.Dir()
	if dir == "" {
		return "", fmt.Errorf("room has no record directory (in-memory transcript)")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# room hand-off: %s\n\n", r.Name)
	fmt.Fprintf(&b, "Written %s, when this room started the run. This file is the task brief.\n\n",
		time.Now().UTC().Format(time.RFC3339))

	b.WriteString("## task\n\n")
	if t := strings.TrimSpace(task); t != "" {
		b.WriteString(clipLine(t, maxHandoffQuoteChars*2))
		b.WriteString("\n")
	} else {
		b.WriteString("(none given with /run — the asks and the plan files below carry the requirements)\n")
	}

	b.WriteString("\n## the room's discussion (attributed, newest last)\n\n")
	// The whole window, not just the user's asks: the personas' verdicts
	// are part of the requirements. BuildConversation is the same
	// renderer the deep dives ship — newest-priority under the byte
	// budget, per-message clips marked, dropped history announced.
	discussion := r.Transcript.BuildConversation(r.Settings.HistoryWindow, maxHandoffDiscussionBytes)
	if strings.TrimSpace(discussion) == "" {
		b.WriteString("(nothing said in the room yet)\n")
	}
	b.WriteString(discussion)

	b.WriteString("\n## plan files (newest first)\n\n")
	plans := planFiles(r.Workspace)
	if len(plans) == 0 {
		b.WriteString("(no " + config.PlansDir + "/*.md — the plan may live entirely in the asks above)\n")
	}
	for _, p := range plans {
		fmt.Fprintf(&b, "- %s\n", p)
	}

	b.WriteString("\n## project knowledge\n\n")
	if _, err := os.Stat(filepath.Join(r.Workspace, ".loop", "knowledge", "digest.md")); err == nil {
		b.WriteString("- digest: .loop/knowledge/digest.md — maintained summary of the codebase's landed state\n")
	} else {
		b.WriteString("- no knowledge digest yet (.loop/knowledge/)\n")
	}

	full := filepath.Join(dir, "handoff.md")
	if err := os.WriteFile(full, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	rel := full
	if r.Workspace != "" {
		if p, err := filepath.Rel(r.Workspace, full); err == nil {
			rel = p
		}
	}
	return rel, nil
}

// planFiles lists the workspace's .loop/plans/*.md, newest first (mtime,
// then path, so equal mtimes stay deterministic).
func planFiles(ws string) []string {
	if ws == "" {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(ws, config.PlansDir, "*.md"))
	type plan struct {
		path string
		mod  time.Time
	}
	var found []plan
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		found = append(found, plan{m, info.ModTime()})
	}
	sort.Slice(found, func(i, j int) bool {
		if !found[i].mod.Equal(found[j].mod) {
			return found[i].mod.After(found[j].mod)
		}
		return found[i].path < found[j].path
	})
	out := make([]string, 0, len(found))
	for _, p := range found {
		rel, err := filepath.Rel(ws, p.path)
		if err != nil {
			rel = p.path
		}
		out = append(out, rel)
	}
	return out
}

// clipLine trims s to n characters on one line: whitespace collapses so
// a quoted message can't break the bullet or section structure.
func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

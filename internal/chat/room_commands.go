package chat

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/shafi-/loop/internal/knowledge"
)

// CommandInfo is one entry of the room's verb menu.
type CommandInfo struct {
	Name string `json:"name"`
	Desc string `json:"desc"`
}

// CommandList returns the room's verbs with their one-line descriptions,
// in registration order — what "/" surfaces to the user.

// CommandList returns the room's verbs with their one-line descriptions,
// in registration order — what "/" surfaces to the user.
func (r *Room) CommandList() []CommandInfo {
	out := make([]CommandInfo, 0, len(r.CustomCommands))
	for _, c := range r.CustomCommands {
		if c.Handle == nil {
			continue
		}
		out = append(out, CommandInfo{Name: c.Name, Desc: c.Desc})
	}
	return out
}

// Room is one configured multi-agent channel.

// notesCommand seeds /notes: the knowledge layer for zero tokens —
// list the area notes, or print one. Deterministic file reads appended
// as system lines, so every attached client and every non-tool agent
// sees the same thing.
func (r *Room) notesCommand() CustomCommand {
	return CustomCommand{
		Name: "notes",
		Desc: "project knowledge: list the area notes, or read one (/notes <slug>)",
		Handle: func(_ context.Context, args []string, ui UI) error {
			line := knowledge.NotesReport(r.Workspace, args)
			if err := r.Transcript.Append("system", line); err != nil {
				return err
			}
			ui.Notice("%s", line)
			return nil
		},
	}
}

// costCommand seeds /cost: the session's token ledger as one system
// line, so every attached client — terminal, web, daemon — sees the
// same answer without any surface-specific plumbing.

// costCommand seeds /cost: the session's token ledger as one system
// line, so every attached client — terminal, web, daemon — sees the
// same answer without any surface-specific plumbing.
func (r *Room) costCommand() CustomCommand {
	return CustomCommand{
		Name: "cost",
		Desc: "this session's token ledger, per agent when several spoke",
		Handle: func(_ context.Context, args []string, ui UI) error {
			line := r.CostReport()
			if err := r.Transcript.Append("system", line); err != nil {
				return err
			}
			ui.Notice("%s", line)
			return nil
		},
	}
}

// maxCompactInput bounds the text one /compact summarizes; the tail is
// what matters (older turns are the first to fall off anyway).

// maxCompactInput bounds the text one /compact summarizes; the tail is
// what matters (older turns are the first to fall off anyway).
const maxCompactInput = 32 << 10

// compactCommand seeds /compact: one LLM call summarizes everything
// before the live window into a "session so far" line; the summarized
// prefix archives (never destroyed) and the summary rides as the first
// context line. Manual by design — compaction spends the user's tokens
// only when they ask.

// compactCommand seeds /compact: one LLM call summarizes everything
// before the live window into a "session so far" line; the summarized
// prefix archives (never destroyed) and the summary rides as the first
// context line. Manual by design — compaction spends the user's tokens
// only when they ask.
func (r *Room) compactCommand() CustomCommand {
	return CustomCommand{
		Name: "compact",
		Desc: "one model call: older turns become a \"session so far\" summary",
		Handle: func(ctx context.Context, _ []string, ui UI) error {
			window := r.Settings.HistoryWindow
			msgs := r.Transcript.Window(0)
			if len(msgs) <= window {
				return r.Transcript.Append("system",
					"◈ nothing to compact — the whole session still fits in the live window")
			}
			if r.Summarizer == nil {
				return r.Transcript.Append("system", "◈ /compact needs an agent to summarize with")
			}
			prefix := Render(msgs[:len(msgs)-window])
			if len(prefix) > maxCompactInput {
				prefix = "[… older turns clipped …]\n" + prefix[len(prefix)-maxCompactInput:]
			}
			summary, err := r.Summarizer(ctx, prefix)
			if err != nil {
				return r.Transcript.Append("system", "◈ /compact failed: "+err.Error())
			}
			summary = strings.TrimSpace(summary)
			if summary == "" {
				return r.Transcript.Append("system", "◈ /compact produced no summary — transcript unchanged")
			}
			archive := fmt.Sprintf("transcript-%s.jsonl", time.Now().UTC().Format("20060102-150405"))
			if err := r.Transcript.Compact(archive, window, "◈ session so far: "+summary); err != nil {
				return err
			}
			ui.Notice("◈ compacted — %d turns summarized, archived as %s", len(msgs)-window, archive)
			return nil
		},
	}
}

// CostReport renders what this room has spent, at three depths: this
// session (with per-agent attribution), every session since the room
// began, and the pipeline runs it commanded (their spend happens in
// child `loop run` processes, so it is read back from their run
// records — any session, current and archived transcripts alike).

// rotationCommands seeds the engine's built-in room verbs.
func (r *Room) rotationCommands() []CustomCommand {
	return []CustomCommand{
		{
			Name: "reset",
			Desc: "archive this conversation and start a fresh one",
			Handle: func(_ context.Context, args []string, ui UI) error {
				archive, err := r.Rotate(0, "reset")
				if err != nil {
					return err
				}
				ui.Notice("↻ reset — archived as %s", archive)
				return nil
			},
		},
		{
			Name: "fork",
			Desc: "keep the first n lines, archive the rest (/fork <n>)",
			Handle: func(_ context.Context, args []string, ui UI) error {
				if len(args) != 1 {
					return fmt.Errorf("usage: /fork <n> — keep the first n transcript lines")
				}
				n, err := strconv.Atoi(args[0])
				if err != nil || n < 0 {
					return fmt.Errorf("usage: /fork <n> — keep the first n transcript lines")
				}
				archive, err := r.Rotate(n, "fork")
				if err != nil {
					return err
				}
				ui.Notice("↻ fork — archived as %s", archive)
				return nil
			},
		},
	}
}

// Command returns the room verb by name, if registered. Callers may
// adjust its Guard (the daemon vetoes rotation while its pipeline runs
// are alive) — the pointer points into the room's registry.

// Command returns the room verb by name, if registered. Callers may
// adjust its Guard (the daemon vetoes rotation while its pipeline runs
// are alive) — the pointer points into the room's registry.
func (r *Room) Command(name string) *CustomCommand {
	for i := range r.CustomCommands {
		if r.CustomCommands[i].Name == name {
			return &r.CustomCommands[i]
		}
	}
	return nil
}

// Rotate restarts the room's conversation: the current transcript is
// archived, the first keep messages survive (0 = a fresh start), and a
// system line marks the point. kind is "reset" or "fork"; it only
// shapes the note text. Returns the archive file's name.

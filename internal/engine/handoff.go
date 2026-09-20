package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shafi-/loop/internal/config"
)

// The room→run hand-off, engine side. A room host starts runs with
// `--var room_context=<brief path>`; a pipeline may read that var itself
// (a tool stage cats the file, prompts reference it) — but most
// pipelines never will, and a var nothing reads is a hand-off that
// silently didn't happen. So the engine guarantees delivery: the first
// executed llm/agent stage gets the brief folded into its input, unless
// that stage's own template already reads the var. Deterministic — same
// brief, same pipeline, same prompt — and audited in events.jsonl.

const (
	// roomContextVar is the var room hosts set to the brief's path.
	roomContextVar = "room_context"
	// maxHandoffFoldBytes caps the brief folded into a prompt. The hosts
	// already keep briefs small; this is the engine-side backstop.
	maxHandoffFoldBytes = 16 << 10
)

// handoffStage and handoffLogged live on stageDeps: the fold decision is
// made once per run — at the first model-facing stage that executes —
// and a retry of that same stage re-folds its freshly rendered input
// (the decision is marked before the attempt runs, so a retried attempt
// keeps its brief instead of appending twice).
//
// foldHandoff returns rendered unchanged, or with the brief appended.
// rawTemplate is the stage's pre-interpolation input/prompt, for the
// self-delivery check.
func (d *stageDeps) foldHandoff(s *config.Stage, rawTemplate, rendered string) string {
	if d.handoffStage != "" && d.handoffStage != s.ID {
		return rendered // delivered (or deliberately not) at an earlier stage
	}
	d.handoffStage = s.ID

	if strings.Contains(rawTemplate, roomContextVar) || strings.Contains(rawTemplate, "room-context") {
		// The pipeline delivers the brief itself (the starter implement
		// pipeline's room-context stage); folding too would read it twice.
		d.logHandoff(s.ID, "handoff_selfdelivered", nil)
		return rendered
	}
	if d.Pipeline == nil {
		return rendered
	}
	path, _ := d.Pipeline.Vars[roomContextVar].(string)
	if strings.TrimSpace(path) == "" {
		return rendered // no room started this run — byte-identical behavior
	}
	brief, err := readHandoffBrief(d.CWD, path)
	if err != nil {
		d.logHandoff(s.ID, "handoff_skipped", map[string]any{"path": path, "why": err.Error()})
		return rendered
	}
	d.logHandoff(s.ID, "handoff_folded", map[string]any{"path": path, "bytes": len(brief)})
	return rendered + "\n\n" + handoffFoldTail(brief)
}

// handoffFoldTail is the literal block folded after a stage's input.
// The closing line is what rescues pipelines that ship example task
// text (a demo `idea:` var) — the room brief outranks it.
func handoffFoldTail(brief string) string {
	return "--- room context (from the room that started this run) ---\n" +
		brief + "\n" +
		"The context above is the authoritative statement of the task for this run; where any example or placeholder task text above conflicts with it, follow the room context."
}

// readHandoffBrief loads the brief, resolving workspace-relative paths
// against the run's working directory (the same rule the shell in a
// tool stage applies to ${vars.room_context}).
func readHandoffBrief(cwd, path string) (string, error) {
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	brief := strings.TrimSpace(string(data))
	if brief == "" {
		return "", fmt.Errorf("brief %s is empty", path)
	}
	if len(brief) > maxHandoffFoldBytes {
		brief = brief[:maxHandoffFoldBytes] + "\n… (clipped — the full brief is in the file)"
	}
	return brief, nil
}

// logHandoff lands the fold decision in events.jsonl, once per run —
// retries re-fold but must not duplicate the audit line. Unlogged runs
// (unit tests) skip it silently.
func (d *stageDeps) logHandoff(stageID, kind string, payload map[string]any) {
	if d.Log == nil || d.handoffLogged {
		return
	}
	d.handoffLogged = true
	d.Log.Event(kind, stageID, payload)
}

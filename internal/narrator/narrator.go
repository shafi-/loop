// Package narrator is the engine's "core loop" LLM: it turns stage
// transitions into human-readable progress, restates human-stage prompts
// as decision-ready questions, and summarizes failures. It is strictly
// best-effort: narration problems are never run failures, and mediation
// falls back to the raw prompt.
package narrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// maxNarrationInput caps anything we send for narration — a narrator
// call should never cost more than the stage it describes.
const maxNarrationInput = 2000

// Narrator produces short human-facing texts via loop's own provider.
type Narrator struct {
	Provider llm.Provider
	Model    string
	// Logf receives narration failures (they are not run errors).
	Logf func(format string, args ...any)
}

const systemPrompt = `You are loop's narrator. You keep the user informed while a
deterministic pipeline runs. Reply with ONE short line (at most two
sentences): plain language, no markdown, no preamble, no quotation
marks around your reply.`

// StageDone implements the engine's narrator contract: one progress line
// after a completed stage.
func (n *Narrator) StageDone(ctx context.Context, s *config.Stage, output string) string {
	return n.say(ctx, fmt.Sprintf(
		`Pipeline stage %q (type %s) just completed. Its output follows; summarize the gist for the user in one line.`+"\n\n%s",
		s.ID, s.Type, truncate(output)))
}

// StageFailed implements the contract: a short explanation of a halted
// stage with one concrete next step.
func (n *Narrator) StageFailed(ctx context.Context, s *config.Stage, stageErr error) string {
	return n.say(ctx, fmt.Sprintf(
		`Pipeline stage %q (type %s) failed and the run halted. The error:
%s

Explain briefly what happened and give one concrete next step.`,
		s.ID, s.Type, truncate(stageErr.Error())))
}

// MediateHuman implements the contract: restate a (possibly huge) human
// stage prompt as a crisp, decision-ready question. Empty string on
// failure — callers fall back to the raw prompt.
func (n *Narrator) MediateHuman(ctx context.Context, s *config.Stage, prompt string) string {
	return n.say(ctx, fmt.Sprintf(
		`The pipeline needs the user's input. Restate this prompt as a crisp
question with the key facts needed for the decision. Preserve any
explicit choices the user must answer (e.g. yes/no).

--- ORIGINAL PROMPT (stage %q) ---
%s`,
		s.ID, truncate(prompt)))
}

// say performs one narration call; any failure yields "" (with a log
// line), never an error — narration is best-effort by contract.
func (n *Narrator) say(ctx context.Context, userPrompt string) string {
	req := llm.Request{
		Model:    n.Model,
		System:   systemPrompt,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: userPrompt}},
	}
	resp, err := n.Provider.Complete(ctx, req)
	if err != nil {
		n.logf("narration unavailable: %v", err)
		return ""
	}
	line := strings.TrimSpace(resp.Text)
	line = strings.Trim(line, "\"“”")
	return line
}

func (n *Narrator) logf(format string, args ...any) {
	if n.Logf != nil {
		n.Logf(format, args...)
	}
}

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxNarrationInput {
		return s
	}
	return s[:maxNarrationInput] + "\n…(truncated)"
}

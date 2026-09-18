package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/shafi-/loop/internal/config"
	"github.com/shafi-/loop/internal/llm"
)

// runHumanStage pauses the run and asks the user. The answer is stored
// under both `answer` (canonical) and `output` (so downstream references
// need no special cases). When a narrator is configured, the raw prompt —
// which may embed huge stage outputs — is restated as a crisp,
// decision-ready question; the raw text is preserved in the run log.
//
// A gate (gate: true) additionally turns the answer into an intent
// label — stages.<id>.intent — so routers branch on what the user
// meant, not on exact string matching of free text.
func runHumanStage(ctx context.Context, s *config.Stage, c *Context, d *stageDeps) (*stageOutcome, error) {
	prompt, err := c.Interpolate(s.Human.Prompt)
	if err != nil {
		return nil, fmt.Errorf("prompt template: %w", err)
	}
	if d.Narrator != nil {
		if mediated := d.Narrator.MediateHuman(ctx, s, prompt); mediated != "" {
			if d.Log != nil {
				d.Log.Event("human_prompt_raw", s.ID, map[string]any{"text": truncateForLog(prompt)})
			}
			prompt = mediated
		}
	}
	if d.Human == nil {
		return nil, fmt.Errorf("human stage requires an interactive session (no HumanIO configured)")
	}
	// The audit trail records both sides of the exchange: what the user was
	// asked and what they answered. Context snapshots keep only the latest
	// answer, so revision rounds would otherwise leave no record.
	if d.Log != nil {
		d.Log.Event("human_prompt", s.ID, map[string]any{"text": truncateForLog(prompt)})
	}
	answer, err := d.Human.Prompt(ctx, prompt)
	if err != nil {
		return nil, err
	}
	if d.Log != nil {
		d.Log.Event("human_answer", s.ID, map[string]any{"answer": truncateForLog(answer)})
	}
	if s.Human.Gate {
		intent, via, err := classifyIntent(ctx, s, d, prompt, answer)
		if err != nil {
			return nil, err
		}
		c.SetOutput(s.ID, "intent", intent)
		if d.Log != nil {
			d.Log.Event("human_intent", s.ID, map[string]any{"intent": intent, "via": via})
		}
	}
	return &stageOutcome{Output: answer}, nil
}

// gateClassifierSystem bounds the classification call: the model sees
// the question the human was asked and their words, and must answer
// with exactly one label. The doubt rule is deliberate — misreading
// approval is the expensive direction.
const gateClassifierSystem = `You classify a human reviewer's reply to a question about a piece of work.
You will see the question they were asked and their reply. Answer with
exactly one word:

  yes      — the reply approves proceeding
  no       — the reply rejects proceeding, without specific change requests
  changes  — the reply asks for modifications, however phrased

When in doubt, prefer "changes": it is safe to be asked again, never
safe to misread approval. Answer with exactly one word and nothing else.`

// classifyIntent maps a human answer to yes | no | changes. Crisp
// vocabulary is deterministic (no model involved — a keyless
// environment still gates yes/no); anything else gets exactly one
// classification call with the full context the user was shown.
// "via" records how the intent was derived, for the run log.
func classifyIntent(ctx context.Context, s *config.Stage, d *stageDeps, prompt, answer string) (intent, via string, err error) {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "yes", "y":
		return "yes", "vocabulary", nil
	case "no", "n":
		return "no", "vocabulary", nil
	}

	provider, err := d.Providers(s.Human.Model)
	if err != nil {
		// Config problems (missing key, unknown provider) fail loudly with
		// their hint — masking them would hide actionable setup errors.
		return "", "", fmt.Errorf("gate classification for %s: %w", s.ID, err)
	}
	provider = d.metered(provider, "gate:"+s.ID)
	req := llmRequest(s.Human.Model, gateClassifierSystem, []llm.Message{{
		Role: llm.RoleUser,
		Content: "The question the reviewer was asked:\n\n" + prompt +
			"\n\nTheir reply:\n\n" + answer,
	}})
	resp, err := provider.Complete(ctx, req)
	if err != nil {
		return "", "", fmt.Errorf("gate classification for %s: %w", s.ID, llmFailure(err, s.Human.Model))
	}
	switch intentWord(resp.Text) {
	case "yes":
		return "yes", "llm", nil
	case "no":
		return "no", "llm", nil
	case "changes":
		return "changes", "llm", nil
	}
	// Garbage classifier output degrades conservatively: treat words as
	// change requests (the safe loop — the reviewer is asked again),
	// never as approval.
	if d.Warnf != nil {
		d.Warnf("! gate %s: classifier answered %q — treating as \"changes\"", s.ID, strings.TrimSpace(resp.Text))
	}
	return "changes", "fallback", nil
}

// intentWord normalizes a classifier completion to a label candidate:
// lowercase first word, punctuation stripped.
func intentWord(s string) string {
	fields := strings.Fields(strings.TrimSpace(strings.ToLower(s)))
	if len(fields) == 0 {
		return ""
	}
	return strings.Trim(fields[0], ".,;:!?\"'`()")
}

// truncateForLog keeps run-log entries bounded.
func truncateForLog(s string) string {
	if len(s) <= 4000 {
		return s
	}
	return s[:4000] + "…"
}

package engine

import (
	"context"
	"fmt"

	"github.com/nerddevsltd/loop/internal/config"
)

// runHumanStage pauses the run and asks the user. The answer is stored
// under both `answer` (canonical) and `output` (so downstream references
// need no special cases). When a narrator is configured, the raw prompt —
// which may embed huge stage outputs — is restated as a crisp,
// decision-ready question; the raw text is preserved in the run log.
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
	answer, err := d.Human.Prompt(ctx, prompt)
	if err != nil {
		return nil, err
	}
	return &stageOutcome{Output: answer}, nil
}

// truncateForLog keeps run-log entries bounded.
func truncateForLog(s string) string {
	if len(s) <= 4000 {
		return s
	}
	return s[:4000] + "…"
}
